package backtestengine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/testkit"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func newClient(url string) *Client {
	return New(Options{BaseURL: url, KeyID: "id", Secret: "secret", Logger: discard})
}

var request = BacktestRequest{Tree: map[string]any{"step": "root"}, StartDate: "2026-01-01", EndDate: "2026-09-10", Benchmark: "SPY"}

func TestBacktestSendsAnInlineStrategyTree(t *testing.T) {
	fake := testkit.NewBacktestEngine(t)
	fake.Set(map[string]float64{"AAPL": 0.5, "BIL": 0.5}, time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC))
	bt, err := newClient(fake.URL).Backtest(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := bt.LatestAllocation()
	if !ok || a.Date != "2026-09-09" || a.Weights["AAPL"] != 0.5 || a.Weights["BIL"] != 0.5 {
		t.Fatalf("allocation %+v", a)
	}
	if _, err := bt.Curve(); err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamFailureIsAnError(t *testing.T) {
	fake := testkit.NewBacktestEngine(t)
	fake.Fail(500)
	_, err := newClient(fake.URL).Backtest(context.Background(), request)
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != 500 {
		t.Fatalf("got %v", err)
	}
}

func TestNotConfigured(t *testing.T) {
	if _, err := New(Options{}).Backtest(context.Background(), request); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("got %v", err)
	}
}

func TestErrorDetailDropsTheEchoedPayload(t *testing.T) {
	got := errorDetail([]byte(`{"value":{"strategyTree":"a huge echo"},"in":["start_date"]}`))
	if strings.Contains(got, "huge echo") || !strings.Contains(got, "start_date") {
		t.Fatalf("got %s", got)
	}
	if got := errorDetail([]byte(`{"humanized":"start_date must be a date"}`)); got != "start_date must be a date" {
		t.Fatalf("got %s", got)
	}
	if got := errorDetail([]byte("plain text")); got != "plain text" {
		t.Fatalf("got %s", got)
	}
}
