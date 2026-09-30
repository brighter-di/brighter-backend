package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Responses are JSON and compress several times over; the template catalogue
// alone is tens of kilobytes. nginx in front passes them through unchanged.
var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// gzipResponse compresses once a body is actually written, so empty answers
// (a CORS preflight, 204, 304) stay empty.
type gzipResponse struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (g *gzipResponse) WriteHeader(status int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	if status != http.StatusNoContent && status != http.StatusNotModified && g.Header().Get("content-encoding") == "" {
		g.Header().Set("content-encoding", "gzip")
		g.Header().Del("content-length")
		g.gz = gzipWriters.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponse) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz == nil {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

func (g *gzipResponse) close() {
	if g.gz != nil {
		_ = g.gz.Close()
		gzipWriters.Put(g.gz)
	}
}

func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("vary", "Accept-Encoding")
		if r.Method == http.MethodHead || r.Method == http.MethodOptions || !strings.Contains(r.Header.Get("accept-encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		g := &gzipResponse{ResponseWriter: w}
		defer g.close()
		next.ServeHTTP(g, r)
	})
}
