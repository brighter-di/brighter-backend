// Package testkit fakes the upstream APIs brighterd calls — the backtest engine, the portfolio engine
// and Nasdaq — and provides a throwaway Postgres schema. It is for tests only.
package testkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/brighter-di/backend/internal/store"
)

/* ── Postgres ───────────────────────────────────────────────────────── */

// Store opens a migrated store in a fresh schema of the database at
// TEST_DATABASE_URL and drops the schema when the test ends. Without the
// variable the test is skipped.
func Store(t testing.TB) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping Postgres test")
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "test_" + hex.EncodeToString(b[:])
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL must be a URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

/* ── the backtest engine ───────────────────────────────────────────────────────── */

const strategyKey = "0b3c1d2e-aaaa-bbbb-cccc-000000000000"

// The backtest engine fakes the backtest engine's backtest endpoint. Every backtest returns a
// 30-day equity curve for the strategy and SPY ending on the cutoff, and the
// configured allocation on the cutoff.
type BacktestEngine struct {
	*httptest.Server
	mu       sync.Mutex
	weights  map[string]float64
	cutoff   time.Time
	fail     int
	calls    int
	requests []json.RawMessage
}

func NewBacktestEngine(t testing.TB) *BacktestEngine {
	c := &BacktestEngine{weights: map[string]float64{"BIL": 1}, cutoff: time.Now().UTC()}
	c.Server = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.Close)
	return c
}

// Set is what backtests allocate to from now on, and the day they allocate on.
func (c *BacktestEngine) Set(weights map[string]float64, cutoff time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.weights, c.cutoff = weights, cutoff
}

// Fail makes every backtest answer with status; 0 restores success.
func (c *BacktestEngine) Fail(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = status
}

// Calls is how many backtests were requested.
func (c *BacktestEngine) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// Requests returns copies of the exact JSON sent to the upstream endpoint.
func (c *BacktestEngine) Requests() []json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]json.RawMessage, len(c.requests))
	for i, request := range c.requests {
		out[i] = append(json.RawMessage(nil), request...)
	}
	return out
}

// EpochDay is the backtest engine's day number for t: days since 1970-01-01 UTC.
func EpochDay(t time.Time) int { return int(t.UTC().Unix() / 86400) }

func (c *BacktestEngine) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/backtest" {
		http.NotFound(w, r)
		return
	}
	var body struct {
		StrategyTree struct {
			RawValue map[string]any `json:"raw_value"`
		} `json:"strategyTree"`
		StartDate  string   `json:"start_date"`
		EndDate    string   `json:"end_date"`
		Benchmarks []string `json:"benchmark_tickers"`
	}
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil || json.Unmarshal(raw, &body) != nil || body.StrategyTree.RawValue == nil || body.StartDate == "" || body.EndDate == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": "strategyTree.raw_value, start_date and end_date are required"})
		return
	}
	c.mu.Lock()
	c.calls++
	c.requests = append(c.requests, raw)
	weights, cutoff, fail := c.weights, c.cutoff, c.fail
	c.mu.Unlock()
	if fail != 0 {
		writeJSON(w, fail, map[string]any{"message": "upstream failure"})
		return
	}

	last := EpochDay(cutoff)
	strat, spy := map[string]float64{}, map[string]float64{}
	for i := range 30 {
		d := strconv.Itoa(last - 29 + i)
		strat[d] = 100000 * (1 + 0.002*float64(i))
		spy[d] = 500 * (1 + 0.001*float64(i))
	}
	dailyWeights := map[string]map[string]float64{}
	for ticker, wt := range weights {
		dailyWeights[ticker] = map[string]float64{strconv.Itoa(last): wt}
	}
	benchmark := "SPY"
	if len(body.Benchmarks) == 1 {
		benchmark = body.Benchmarks[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"last_market_day": last,
		"dvm_capital":     map[string]any{strategyKey: strat, benchmark: spy},
		"daily_weights":   dailyWeights,
	})
}

/* ── the portfolio engine ─────────────────────────────────────────────────────────── */

const (
	// PortfolioEngineKey is the only API key the fake the portfolio engine accepts.
	PortfolioEngineKey = "test-portfolioEngine-key"
	// DepositAddress is the smart account every fake enrollment returns.
	DepositAddress = "0xdddddddddddddddddddddddddddddddddddddddd"
)

// PortfolioEngineRequest is one call the fake the portfolio engine received.
type PortfolioEngineRequest struct {
	Pattern string // the route, e.g. "POST /strategies/{id}/versions"
	Path    string
	Body    map[string]any
}

// PortfolioEngineError makes a handler answer with the portfolio engine's error envelope.
type PortfolioEngineError struct {
	Code    string
	Message string
}

// PortfolioEngineHandler answers one route with an HTTP status and the envelope's
// data, or a PortfolioEngineError.
type PortfolioEngineHandler func(r *http.Request, body map[string]any) (int, any)

// The portfolio engine fakes the portfolio engine routes brighterd uses. Every route succeeds by
// default; Handle replaces one.
type PortfolioEngine struct {
	*httptest.Server
	mu         sync.Mutex
	handlers   map[string]PortfolioEngineHandler
	requests   []PortfolioEngineRequest
	versions   map[string]int
	strategies atomic.Int64
	portfolios atomic.Int64
}

func NewPortfolioEngine(t testing.TB) *PortfolioEngine {
	g := &PortfolioEngine{handlers: map[string]PortfolioEngineHandler{}, versions: map[string]int{}}
	mux := http.NewServeMux()
	for pattern, h := range g.defaults() {
		g.handlers[pattern] = h
		mux.HandleFunc(pattern, g.route(pattern))
	}
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

// Handle replaces the behaviour of one of the fake's routes.
func (g *PortfolioEngine) Handle(pattern string, h PortfolioEngineHandler) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.handlers[pattern]; !ok {
		panic("testkit: the fake PortfolioEngine has no route " + pattern)
	}
	g.handlers[pattern] = h
}

// Requests returns the calls made to one route, oldest first.
func (g *PortfolioEngine) Requests(pattern string) []PortfolioEngineRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []PortfolioEngineRequest
	for _, r := range g.requests {
		if r.Pattern == pattern {
			out = append(out, r)
		}
	}
	return out
}

func (g *PortfolioEngine) route(pattern string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != PortfolioEngineKey {
			writeEnvelope(w, http.StatusUnauthorized, PortfolioEngineError{Code: "UNAUTHORIZED", Message: "invalid api key"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.requests = append(g.requests, PortfolioEngineRequest{Pattern: pattern, Path: r.URL.Path, Body: body})
		h := g.handlers[pattern]
		g.mu.Unlock()
		status, data := h(r, body)
		writeEnvelope(w, status, data)
	}
}

func (g *PortfolioEngine) defaults() map[string]PortfolioEngineHandler {
	ok := func(data any) PortfolioEngineHandler {
		return func(*http.Request, map[string]any) (int, any) { return http.StatusOK, data }
	}
	op := map[string]any{"operationId": "op-1", "submittedAt": "2026-09-10T15:00:00Z"}
	auth := map[string]any{
		"authorizationId": "auth-1", "expiresAt": "2026-09-10T15:10:00Z",
		"typedData": map[string]any{
			"domain": map[string]any{"name": "PortfolioEngine", "chainId": 56}, "types": map[string]any{},
			"primaryType": "Withdraw", "message": map[string]any{},
		},
	}
	return map[string]PortfolioEngineHandler{
		"POST /strategies": func(*http.Request, map[string]any) (int, any) {
			return http.StatusOK, map[string]any{"strategyId": fmt.Sprintf("strat-%d", g.strategies.Add(1)), "version": 1}
		},
		"POST /strategies/{id}/versions": func(r *http.Request, _ map[string]any) (int, any) {
			g.mu.Lock()
			defer g.mu.Unlock()
			id := r.PathValue("id")
			g.versions[id] = max(g.versions[id], 1) + 1
			return http.StatusOK, map[string]any{"strategyId": id, "version": g.versions[id]}
		},
		"POST /enroll/signature": ok(map[string]any{
			"flowId": "flow-1", "accountIndex": "0", "agentAccountId": "agent-1", "accountType": "ECDSA",
			"message": map[string]any{"kind": "ecdsa", "raw": "0x" + strings.Repeat("ab", 32)},
		}),
		"POST /enroll": func(_ *http.Request, body map[string]any) (int, any) {
			chain := 56
			if ids, ok := body["chainIds"].([]any); ok && len(ids) > 0 {
				if f, ok := ids[0].(float64); ok {
					chain = int(f)
				}
			}
			return http.StatusOK, map[string]any{
				"portfolioId":   fmt.Sprintf("pf-%d", g.portfolios.Add(1)),
				"strategyId":    body["strategyId"],
				"smartAccounts": []any{map[string]any{"accountId": fmt.Sprintf("eip155:%d:%s", chain, DepositAddress)}},
			}
		},
		"GET /portfolios/{id}": func(r *http.Request, _ map[string]any) (int, any) {
			return http.StatusOK, map[string]any{
				"portfolioId": r.PathValue("id"), "strategyName": "Brighter — test strategy", "ownerAccountId": "eip155:0:0x0",
				"schedule": map[string]any{
					"status": "active", "frequency": "hourly", "intervalMs": 3600000,
					"nextDueAt": "2026-09-10T16:00:00Z", "lastRebalanceAt": nil,
				},
			}
		},
		"GET /portfolios/{id}/positions": func(r *http.Request, _ map[string]any) (int, any) {
			return http.StatusOK, map[string]any{
				"portfolioId": r.PathValue("id"), "totalValueUsd": "0", "fetchedAt": "2026-09-10T15:00:00Z",
				"assets": []any{}, "warnings": []any{},
			}
		},
		// $100 in, a 5% day, then $50 more: profit stays $5.
		"GET /portfolios/{id}/performance": func(r *http.Request, _ map[string]any) (int, any) {
			return http.StatusOK, map[string]any{
				"portfolioId": r.PathValue("id"), "strategyId": "strat-1",
				"meta": map[string]any{"method": r.URL.Query().Get("returnMethod"), "currency": "USD", "resolution": "1d", "asOf": "2026-09-10T16:00:00Z"},
				"points": []any{
					map[string]any{"date": "2026-09-08", "percentChange": "0", "valueUsd": "100", "cashFlowUsd": "100", "isLive": false},
					map[string]any{"date": "2026-09-09", "percentChange": "5", "valueUsd": "105", "cashFlowUsd": "0", "isLive": false},
					map[string]any{"date": "2026-09-10", "percentChange": "4.8", "valueUsd": "155", "cashFlowUsd": "50", "isLive": true},
				},
			}
		},
		"POST /portfolios/{id}/start":     ok(map[string]any{}),
		"POST /portfolios/{id}/stop":      ok(map[string]any{}),
		"POST /portfolios/{id}/rebalance": ok(op),
		"GET /portfolios/{id}/operations/{op}": func(r *http.Request, _ map[string]any) (int, any) {
			return http.StatusOK, map[string]any{"operationId": r.PathValue("op"), "portfolioId": r.PathValue("id"), "kind": "rebalance", "state": "completed"}
		},
		"POST /portfolios/{id}/withdraw/signature":      ok(auth),
		"POST /portfolios/{id}/withdraw":                ok(op),
		"POST /portfolios/{id}/liquidate-all/signature": ok(auth),
		"POST /portfolios/{id}/liquidate-all":           ok(op),
	}
}

func writeEnvelope(w http.ResponseWriter, status int, data any) {
	if e, ok := data.(PortfolioEngineError); ok {
		writeJSON(w, status, map[string]any{"success": false, "error": map[string]any{"code": e.Code, "message": e.Message}})
		return
	}
	writeJSON(w, status, map[string]any{"success": true, "data": data})
}

/* ── Nasdaq ─────────────────────────────────────────────────────────── */

// Nasdaq fakes Nasdaq's historical quote endpoint.
type Nasdaq struct {
	*httptest.Server
	mu     sync.Mutex
	closes map[string]map[string]float64 // ticker → YYYY-MM-DD → close
	fail   int
	calls  int
}

func NewNasdaq(t testing.TB) *Nasdaq {
	n := &Nasdaq{closes: map[string]map[string]float64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/quote/{ticker}/historical", n.serve)
	n.Server = httptest.NewServer(mux)
	t.Cleanup(n.Close)
	return n
}

// Set gives ticker one close per weekday, oldest first, the last on last.
func (n *Nasdaq) Set(ticker string, last time.Time, closes ...float64) {
	series := map[string]float64{}
	d := last
	for i := len(closes) - 1; i >= 0; i-- {
		for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			d = d.AddDate(0, 0, -1)
		}
		series[d.Format("2006-01-02")] = closes[i]
		d = d.AddDate(0, 0, -1)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closes[strings.ToUpper(ticker)] = series
}

// FailNext makes the next count requests answer 500.
func (n *Nasdaq) FailNext(count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.fail = count
}

// Calls is how many requests arrived, failed ones included.
func (n *Nasdaq) Calls() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls
}

func (n *Nasdaq) serve(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	n.calls++
	if n.fail > 0 {
		n.fail--
		n.mu.Unlock()
		http.Error(w, "temporarily unavailable", http.StatusInternalServerError)
		return
	}
	series := n.closes[strings.ToUpper(r.PathValue("ticker"))]
	n.mu.Unlock()

	if len(series) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"data":   nil,
			"status": map[string]any{"bCodeMessage": []any{map[string]any{"errorMessage": "Symbol not exists."}}},
		})
		return
	}
	dates := make([]string, 0, len(series))
	for d := range series {
		dates = append(dates, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates))) // Nasdaq lists newest first
	rows := make([]map[string]string, len(dates))
	for i, d := range dates {
		t, _ := time.Parse("2006-01-02", d)
		rows[i] = map[string]string{"date": t.Format("01/02/2006"), "close": fmt.Sprintf("$%.2f", series[d])}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"tradesTable": map[string]any{"rows": rows}}})
}

/* ── Telegram ───────────────────────────────────────────────────────── */

// Telegram fakes the Bot API: sendMessage records every message with its
// chat, and getUpdates reports nothing new.
type Telegram struct {
	*httptest.Server
	mu   sync.Mutex
	sent []telegramMessage
}

type telegramMessage struct{ chat, text string }

func NewTelegram(t testing.TB) *Telegram {
	tg := &Telegram{}
	tg.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": []any{}})
			return
		}
		var b struct {
			Chat any    `json:"chat_id"`
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		tg.mu.Lock()
		tg.sent = append(tg.sent, telegramMessage{chat: fmt.Sprint(b.Chat), text: b.Text})
		tg.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	t.Cleanup(tg.Close)
	return tg
}

// Messages returns every message received so far, to any chat, oldest first.
func (tg *Telegram) Messages() []string { return tg.To("") }

// To returns the messages sent to one chat ("" for every chat), oldest first.
func (tg *Telegram) To(chat string) []string {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	var out []string
	for _, m := range tg.sent {
		if chat == "" || m.chat == chat {
			out = append(out, m.text)
		}
	}
	return out
}

// Wait returns the first message containing substr, failing the test if none
// arrives within five seconds. Notifications are asynchronous.
func (tg *Telegram) Wait(t testing.TB, substr string) string {
	t.Helper()
	return tg.WaitTo(t, "", substr)
}

// WaitTo is Wait for the messages sent to one chat.
func (tg *Telegram) WaitTo(t testing.TB, chat, substr string) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, m := range tg.To(chat) {
			if strings.Contains(m, substr) {
				return m
			}
		}
	}
	t.Fatalf("no Telegram message to %q containing %q; got %q", chat, substr, tg.To(chat))
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
