package portfolioengine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/testkit"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func newClient(url, key string) *Client {
	return New(Options{BaseURL: url, APIKey: key, Logger: discard, RetryDelay: time.Millisecond})
}

func unavailable(*http.Request, map[string]any) (int, any) {
	return http.StatusServiceUnavailable, testkit.PortfolioEngineError{Code: "UNAVAILABLE", Message: "try again"}
}

func TestDecodesEnvelopeAndSendsTheKey(t *testing.T) {
	g := testkit.NewPortfolioEngine(t)
	res, err := newClient(g.URL, testkit.PortfolioEngineKey).CreateStrategy(context.Background(), CreateStrategyRequest{
		Name:       "Brighter — test",
		Allocation: Allocation{Assets: []AllocationAsset{{AssetID: "eip155:56/erc20:0x01", Weight: "100"}}},
		Schedule:   Schedule{Type: "interval", Frequency: "hourly"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StrategyID != "strat-1" || res.Version != 1 {
		t.Fatalf("got %+v", res)
	}
	reqs := g.Requests("POST /strategies")
	if len(reqs) != 1 || reqs[0].Body["name"] != "Brighter — test" {
		t.Fatalf("requests: %+v", reqs)
	}
}

func TestWrongKeyIsAnUpstreamError(t *testing.T) {
	g := testkit.NewPortfolioEngine(t)
	err := newClient(g.URL, "wrong").Start(context.Background(), "pf-1")
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != http.StatusUnauthorized {
		t.Fatalf("got %v", err)
	}
}

func TestNotConfigured(t *testing.T) {
	if err := New(Options{}).Start(context.Background(), "pf-1"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("got %v", err)
	}
}

func TestErrorEnvelopeIsKeptVerbatim(t *testing.T) {
	g := testkit.NewPortfolioEngine(t)
	g.Handle("POST /enroll", func(*http.Request, map[string]any) (int, any) {
		return http.StatusBadRequest, testkit.PortfolioEngineError{Code: "API_100", Message: "Signature does not match"}
	})
	_, err := newClient(g.URL, testkit.PortfolioEngineKey).Enroll(context.Background(), EnrollRequest{StrategyID: "strat-1"})
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("got %v", err)
	}
	if ge.Status != http.StatusBadRequest || ge.Code != "API_100" || ge.Message != "Signature does not match" {
		t.Fatalf("got %+v", ge)
	}
	if !strings.Contains(ge.Error(), "POST /enroll") {
		t.Fatalf("the log form should name the call: %s", ge.Error())
	}
}

func TestRetryAfterIsRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("retry-after", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"success":false,"error":{"code":"COOLDOWN","message":"Rebalance cooldown"}}`))
	}))
	defer srv.Close()
	_, err := newClient(srv.URL, "key").Rebalance(context.Background(), "pf-1")
	var ge *Error
	if !errors.As(err, &ge) || ge.RetryAfter != 42*time.Second || ge.Code != "COOLDOWN" {
		t.Fatalf("got %v", err)
	}
}

func TestReadsRetryTransientFailures(t *testing.T) {
	g := testkit.NewPortfolioEngine(t)
	var calls atomic.Int32
	g.Handle("GET /portfolios/{id}/positions", func(r *http.Request, b map[string]any) (int, any) {
		if calls.Add(1) < 3 {
			return unavailable(r, b)
		}
		return http.StatusOK, map[string]any{"portfolioId": r.PathValue("id")}
	})
	raw, err := newClient(g.URL, testkit.PortfolioEngineKey).Positions(context.Background(), "pf-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "pf-1") || calls.Load() != 3 {
		t.Fatalf("raw %s after %d calls", raw, calls.Load())
	}
}

func TestReadsGiveUpAfterThreeAttempts(t *testing.T) {
	g := testkit.NewPortfolioEngine(t)
	g.Handle("GET /portfolios/{id}", unavailable)
	_, err := newClient(g.URL, testkit.PortfolioEngineKey).Portfolio(context.Background(), "pf-1")
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != http.StatusServiceUnavailable {
		t.Fatalf("got %v", err)
	}
	if n := len(g.Requests("GET /portfolios/{id}")); n != 3 {
		t.Fatalf("%d attempts, want 3", n)
	}
}

func TestWritesAreNeverRetried(t *testing.T) {
	g := testkit.NewPortfolioEngine(t)
	g.Handle("POST /portfolios/{id}/rebalance", unavailable)
	if _, err := newClient(g.URL, testkit.PortfolioEngineKey).Rebalance(context.Background(), "pf-1"); err == nil {
		t.Fatal("expected an error")
	}
	if n := len(g.Requests("POST /portfolios/{id}/rebalance")); n != 1 {
		t.Fatalf("a write was sent %d times", n)
	}
}

func TestNotFound(t *testing.T) {
	gone := []*Error{
		{Status: http.StatusNotFound},
		{Status: http.StatusBadRequest, Message: "Unknown portfolio pf-1"},
		{Status: http.StatusBadRequest, Message: "PORTFOLIO_NOT_FOUND"},
	}
	for _, e := range gone {
		if !e.NotFound() {
			t.Errorf("%+v should read as not found", e)
		}
	}
	if (&Error{Status: http.StatusBadRequest, Message: "amount too small"}).NotFound() {
		t.Error("an ordinary 400 is not a missing portfolio")
	}
}

func TestPerformanceAsksForMoneyWeightedReturns(t *testing.T) {
	g := testkit.NewPortfolioEngine(t)
	p, err := newClient(g.URL, testkit.PortfolioEngineKey).Performance(context.Background(), "pf-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Meta.Method != "MWR" || len(p.Points) != 3 || p.Points[2].ValueUsd != "155" || !p.Points[2].IsLive || *p.Points[1].PercentChange != "5" {
		t.Fatalf("performance %+v", p)
	}
}
