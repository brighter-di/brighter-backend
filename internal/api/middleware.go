package api

import (
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/brighter-di/backend/internal/service"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/health/live" {
			return
		}
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"bytes", rec.bytes, "elapsed_ms", time.Since(started).Milliseconds(), "ip", s.clientIP(r))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "method", r.Method, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, apiError{Error: "Internal error.", Code: "INTERNAL"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// cors echoes only allowlisted origins — never "*" — except on /mcp. CORS is
// not access control; it stops other sites from driving this API through a
// visitor's browser. The real protection is that no secret ever leaves this
// process. /mcp is open to every origin because browser-based MCP clients
// live on origins we cannot list, and it is safe to: it is public, read-only,
// takes no credentials and carries its own rate limits.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(r.Header.Get("origin"), "/")
		if r.URL.Path == "/mcp" {
			h := w.Header()
			h.Set("access-control-allow-origin", "*")
			h.Set("access-control-allow-methods", "GET, POST, OPTIONS")
			h.Set("access-control-allow-headers", "content-type, mcp-protocol-version, mcp-session-id")
			h.Set("access-control-expose-headers", "retry-after, mcp-session-id")
			h.Set("access-control-max-age", "600")
			if r.Method == http.MethodOptions && r.Header.Get("access-control-request-method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		allowed := origin != "" && slices.Contains(s.cfg.CORSOrigins, origin)
		if allowed {
			h := w.Header()
			h.Set("access-control-allow-origin", origin)
			h.Add("vary", "Origin")
			h.Set("access-control-allow-methods", "GET, POST, OPTIONS")
			h.Set("access-control-allow-headers", "content-type, authorization")
			h.Set("access-control-expose-headers", "retry-after")
			h.Set("access-control-max-age", "600")
		}
		if r.Method == http.MethodOptions && r.Header.Get("access-control-request-method") != "" {
			if allowed {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusForbidden)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limit(l *ipLimiter, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.allow(s.clientIP(r)); !ok {
			writeRateLimited(w, wait)
			return
		}
		next(w, r)
	})
}

// meter rate-limits only the work a request sends upstream: the service asks
// for admission before a cache miss, so answers already cached are never
// counted.
func (s *Server) meter(l *ipLimiter, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)
		ctx := service.WithAdmit(r.Context(), func() error {
			if ok, wait := l.allow(ip); !ok {
				return &rateLimited{wait: wait}
			}
			return nil
		})
		next(w, r.WithContext(ctx))
	})
}

// rateLimited is a request a limiter refused; wait is when to try again.
type rateLimited struct{ wait time.Duration }

func (e *rateLimited) Error() string { return "rate limited" }

func writeRateLimited(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("retry-after", strconv.Itoa(max(1, int(wait.Seconds()+0.5))))
	writeJSON(w, http.StatusTooManyRequests, apiError{Error: "Too many requests — slow down.", Code: "RATE_LIMITED"})
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if ip := strings.TrimSpace(r.Header.Get("x-real-ip")); ip != "" {
			return ip
		}
		if xff := r.Header.Get("x-forwarded-for"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipLimiter is a token bucket per client IP.
type ipLimiter struct {
	mu      sync.Mutex
	perSec  rate.Limit
	burst   int
	clients map[string]*client
	swept   time.Time
}

type client struct {
	lim  *rate.Limiter
	seen time.Time
}

func newIPLimiter(perSec float64, burst int) *ipLimiter {
	return &ipLimiter{perSec: rate.Limit(perSec), burst: burst, clients: map[string]*client{}, swept: time.Now()}
}

func (l *ipLimiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.swept) > time.Minute {
		for k, c := range l.clients {
			if now.Sub(c.seen) > 10*time.Minute {
				delete(l.clients, k)
			}
		}
		l.swept = now
	}
	c, ok := l.clients[ip]
	if !ok {
		c = &client{lim: rate.NewLimiter(l.perSec, l.burst)}
		l.clients[ip] = c
	}
	c.seen = now
	res := c.lim.ReserveN(now, 1)
	if delay := res.DelayFrom(now); delay > 0 {
		res.CancelAt(now)
		return false, delay
	}
	return true, 0
}
