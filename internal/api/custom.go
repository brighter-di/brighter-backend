package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/brighter-di/backend/internal/service"
	"github.com/brighter-di/backend/internal/strategy"
)

// customBacktestBody is what Brighter Studio posts to run its current code.
type customBacktestBody struct {
	Strategy strategy.CustomStrategy `json:"strategy"`
	Range    string                  `json:"range"`
	Points   int                     `json:"points"`
}

// globalLimiter caps work across all clients together. A new Studio strategy
// is always a cache miss, and the backtest engine's backtest limit is shared with the
// scheduler that keeps live portfolios current.
type globalLimiter struct{ lim *rate.Limiter }

func newGlobalLimiter(perHour float64, burst int) *globalLimiter {
	return &globalLimiter{lim: rate.NewLimiter(rate.Limit(perHour/3600), burst)}
}

func (g *globalLimiter) allow() (bool, time.Duration) {
	now := time.Now()
	res := g.lim.ReserveN(now, 1)
	if delay := res.DelayFrom(now); delay > 0 {
		res.CancelAt(now)
		return false, delay
	}
	return true, 0
}

// backtestCustom runs a Studio strategy through the same read-only, inline
// backtest engine backtest as the templates. Only cache misses are counted.
func (s *Server) backtestCustom(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var body customBacktestBody
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&body); err != nil {
		s.fail(w, r, &service.Error{Status: http.StatusBadRequest, Code: "BAD_REQUEST", Message: "Send the strategy as JSON."})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		s.fail(w, r, &service.Error{Status: http.StatusBadRequest, Code: "BAD_REQUEST", Message: "Send exactly one backtest request as JSON."})
		return
	}
	if body.Range == "" {
		body.Range = "5y"
	}
	ip := s.clientIP(r)
	ctx := service.WithAdmit(r.Context(), func() error {
		if ok, wait := s.research.allow(ip); !ok {
			return &rateLimited{wait: wait}
		}
		if ok, wait := s.custom.allow(); !ok {
			return &rateLimited{wait: wait}
		}
		return nil
	})
	v, err := s.svc.BacktestCustom(ctx, body.Strategy, body.Range)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := *v
	if body.Points > 0 {
		out = out.Downsample(min(max(body.Points, 20), 5000))
	}
	w.Header().Set("cache-control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
