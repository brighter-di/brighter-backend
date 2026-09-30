package prices

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/testkit"
)

var (
	discard = slog.New(slog.NewTextHandler(io.Discard, nil))
	from    = time.Date(2025, 9, 10, 0, 0, 0, 0, time.UTC)
	to      = time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
)

func newClient(url string) *Client {
	return New(Options{BaseURL: url, Logger: discard, RetryDelay: time.Millisecond})
}

func TestHistoryParsesSortsAndSkipsBadRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/quote/SPY/historical" || r.URL.Query().Get("assetclass") != "etf" {
			t.Errorf("request %s", r.URL)
		}
		if !strings.Contains(r.Header.Get("user-agent"), "Mozilla") {
			t.Error("the source needs a browser user agent")
		}
		_, _ = w.Write([]byte(`{"data":{"tradesTable":{"rows":[
			{"date":"09/10/2026","close":"$1,234.56"},
			{"date":"not a date","close":"$1.00"},
			{"date":"09/08/2026","close":"N/A"},
			{"date":"09/09/2026","close":"$1,200.00"}]}}}`))
	}))
	defer srv.Close()

	bars, err := newClient(srv.URL).History(context.Background(), "SPY", ETF, from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := []Bar{{Date: "2026-09-09", Close: 1200}, {Date: "2026-09-10", Close: 1234.56}}
	if !reflect.DeepEqual(bars, want) {
		t.Fatalf("got %+v", bars)
	}
}

func TestHistoryRetriesServerFailures(t *testing.T) {
	n := testkit.NewNasdaq(t)
	n.Set("MSFT", to, 1, 2)
	n.FailNext(2)
	bars, err := newClient(n.URL).History(context.Background(), "MSFT", Stock, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 || n.Calls() != 3 {
		t.Fatalf("%d bars after %d calls", len(bars), n.Calls())
	}
}

func TestHistoryGivesUpAfterThreeAttempts(t *testing.T) {
	n := testkit.NewNasdaq(t)
	n.FailNext(10)
	_, err := newClient(n.URL).History(context.Background(), "MSFT", Stock, from, to)
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != http.StatusInternalServerError {
		t.Fatalf("got %v", err)
	}
	if n.Calls() != 3 {
		t.Fatalf("%d attempts, want 3", n.Calls())
	}
}

func TestHistoryDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	_, err := newClient(srv.URL).History(context.Background(), "MSFT", Stock, from, to)
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != http.StatusNotFound || calls.Load() != 1 {
		t.Fatalf("got %v after %d calls", err, calls.Load())
	}
}

func TestUnknownSymbol(t *testing.T) {
	n := testkit.NewNasdaq(t)
	_, err := newClient(n.URL).History(context.Background(), "NOPE", Stock, from, to)
	var pe *Error
	if !errors.As(err, &pe) || !strings.Contains(pe.Detail, "Symbol not exists") {
		t.Fatalf("got %v", err)
	}
}

func TestNumber(t *testing.T) {
	cases := map[string]float64{"$1,234.56": 1234.56, "+1.23": 1.23, "-0.45%": -0.45, " 7 ": 7}
	for in, want := range cases {
		if got, ok := number(in); !ok || got != want {
			t.Errorf("number(%q) = %v, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "N/A", "abc"} {
		if _, ok := number(in); ok {
			t.Errorf("number(%q) should fail", in)
		}
	}
}
