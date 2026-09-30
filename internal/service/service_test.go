package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/backtestengine"
	"github.com/brighter-di/backend/internal/config"
	"github.com/brighter-di/backend/internal/notify"
	"github.com/brighter-di/backend/internal/portfolioengine"
	"github.com/brighter-di/backend/internal/prices"
	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/strategy"
	"github.com/brighter-di/backend/internal/testkit"
	"github.com/brighter-di/backend/internal/tokens"
)

// testNow is a Thursday, after the US close.
var testNow = time.Date(2026, 9, 10, 22, 0, 0, 0, time.UTC)

var (
	owner      = "0x" + strings.Repeat("Aa", 20) // mixed case, as wallets report it
	ownerLower = strings.ToLower(owner)
	sig        = "0x" + strings.Repeat("1b", 65)
	mag7Alloc  = map[string]float64{"AAPL": 0.333, "MSFT": 0.333, "NVDA": 0.334}
)

type harness struct {
	svc             *Service
	store           *store.Store
	backtestEngine  *testkit.BacktestEngine
	portfolioEngine *testkit.PortfolioEngine
	nasdaq          *testkit.Nasdaq
	telegram        *testkit.Telegram
}

// newHarness builds a Service against fake upstreams. withDB adds a real
// Postgres schema (the test is skipped without TEST_DATABASE_URL).
func newHarness(t *testing.T, withDB bool) *harness {
	t.Helper()
	h := &harness{backtestEngine: testkit.NewBacktestEngine(t), portfolioEngine: testkit.NewPortfolioEngine(t), nasdaq: testkit.NewNasdaq(t), telegram: testkit.NewTelegram(t)}
	if withDB {
		h.store = testkit.Store(t)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		EnabledChains: []int64{56}, MaxCutoffAgeTradingDays: 3, MaxDataLagDays: 4,
		PortfolioEngine: config.PortfolioEngine{ScheduleFrequency: "hourly", ScheduleTimezone: "America/New_York", SlippageBps: 50, PriceImpactBps: 100},
	}
	cc := backtestengine.New(backtestengine.Options{BaseURL: h.backtestEngine.URL, KeyID: "id", Secret: "secret", Logger: log})
	gc := portfolioengine.New(portfolioengine.Options{BaseURL: h.portfolioEngine.URL, APIKey: testkit.PortfolioEngineKey, Logger: log, RetryDelay: time.Millisecond})
	pc := prices.New(prices.Options{BaseURL: h.nasdaq.URL, Logger: log, RetryDelay: time.Millisecond})
	opts := notify.Options{Token: "test", ChatID: "1", BaseURL: h.telegram.URL, Logger: log, SendGap: time.Millisecond}
	if h.store != nil {
		opts.Subscribers = h.store
	}
	notifier := notify.New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go notifier.Run(ctx)
	h.svc = New(cfg, h.store, cc, gc, pc, log).WithNotifier(notifier)
	h.svc.now = func() time.Time { return testNow }
	h.backtestEngine.Set(mag7Alloc, testNow.AddDate(0, 0, -1))
	return h
}

func mag7() InvestRequest {
	return InvestRequest{TemplateID: "mag7-rank", Params: map[string]float64{"count": 3, "lookback": 126}, ChainID: 56, Owner: owner}
}

func confirmFor(req InvestRequest, e *Enrollment) ConfirmRequest {
	return ConfirmRequest{
		InvestRequest: req, StrategyID: e.StrategyID, ChainIDs: e.ChainIDs, FlowID: e.FlowID,
		AccountIndex: e.AccountIndex, AgentAccountID: e.AgentAccountID, AccountType: e.AccountType, Signature: sig,
	}
}

// invest runs prepare and confirm and returns the new portfolio.
func (h *harness) invest(t *testing.T, req InvestRequest) *PortfolioView {
	t.Helper()
	ctx := context.Background()
	prep, err := h.svc.PrepareInvest(ctx, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep.Status != "sign" {
		t.Fatalf("prepare status %q", prep.Status)
	}
	p, err := h.svc.ConfirmInvest(ctx, confirmFor(req, prep.Enrollment))
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return p
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var se *Error
	if !errors.As(err, &se) || se.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func assetID(t *testing.T, ticker string) string {
	t.Helper()
	id, ok := tokens.AssetID(ticker, 56)
	if !ok {
		t.Fatalf("no BNB Chain token for %s", ticker)
	}
	return id
}

func weightsByAsset(body map[string]any) map[string]string {
	alloc, _ := body["allocation"].(map[string]any)
	assets, _ := alloc["assets"].([]any)
	out := map[string]string{}
	for _, a := range assets {
		m, _ := a.(map[string]any)
		id, _ := m["assetId"].(string)
		w, _ := m["weight"].(string)
		out[id] = w
	}
	return out
}

/* ── Investing ──────────────────────────────────────────────────────── */

func TestInvestCreatesOneStrategyAndAPortfolio(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)

	prep, err := h.svc.PrepareInvest(ctx, mag7())
	if err != nil {
		t.Fatal(err)
	}
	if prep.Status != "sign" || !strings.HasPrefix(prep.Enrollment.Message.Raw, "0x") {
		t.Fatalf("prepare %+v", prep)
	}
	created := h.portfolioEngine.Requests("POST /strategies")
	if len(created) != 1 {
		t.Fatalf("%d strategies created", len(created))
	}
	body := created[0].Body
	want := map[string]string{assetID(t, "AAPL"): "33.3", assetID(t, "MSFT"): "33.3", assetID(t, "NVDA"): "33.4"}
	if got := weightsByAsset(body); len(got) != 3 || got[assetID(t, "AAPL")] != want[assetID(t, "AAPL")] ||
		got[assetID(t, "MSFT")] != want[assetID(t, "MSFT")] || got[assetID(t, "NVDA")] != want[assetID(t, "NVDA")] {
		t.Fatalf("allocation %v, want %v", got, want)
	}
	if sched, _ := body["schedule"].(map[string]any); sched["type"] != "interval" || sched["frequency"] != "hourly" {
		t.Fatalf("schedule %v", body["schedule"])
	}
	if body["name"] != "Mag7 Momentum Rank" || body["description"] != "Hold the strongest Mag7 names by trailing total return, rebalanced monthly." {
		t.Fatalf("strategy named %q, described %q", body["name"], body["description"])
	}

	// A second prepare reuses the strategy.
	if _, err := h.svc.PrepareInvest(ctx, mag7()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.portfolioEngine.Requests("POST /strategies")); n != 1 {
		t.Fatalf("%d strategies after a second prepare", n)
	}

	p, err := h.svc.ConfirmInvest(ctx, confirmFor(mag7(), prep.Enrollment))
	if err != nil {
		t.Fatal(err)
	}
	if p.PortfolioID != "pf-1" || p.DepositAddress != testkit.DepositAddress || p.Owner != ownerLower ||
		p.TemplateName != "Mag7 Momentum Rank" || len(p.Allocation) != 3 {
		t.Fatalf("portfolio %+v", p)
	}
	enroll := h.portfolioEngine.Requests("POST /enroll")
	if len(enroll) != 1 || enroll[0].Body["ownerAccountId"] != "eip155:0:"+ownerLower ||
		enroll[0].Body["portfolioName"] != "Brighter - Mag7 Momentum Rank" || enroll[0].Body["signature"] != sig {
		t.Fatalf("enroll %+v", enroll)
	}
	if n := len(h.portfolioEngine.Requests("POST /portfolios/{id}/start")); n != 1 {
		t.Fatalf("automation started %d times", n)
	}

	// Once enrolled, prepare points at the existing portfolio.
	again, err := h.svc.PrepareInvest(ctx, mag7())
	if err != nil || again.Status != "existing" || again.Portfolio.PortfolioID != "pf-1" {
		t.Fatalf("prepare after confirm: %+v, %v", again, err)
	}
	evals, _ := h.svc.RecentEvaluations(ctx, 10)
	if len(evals) != 1 || evals[0].Trigger != "create" || evals[0].Status != "published" {
		t.Fatalf("evaluations %+v", evals)
	}
}

func TestInvestRejectsBadRequests(t *testing.T) {
	h := newHarness(t, false)
	cases := []struct {
		name string
		edit func(*InvestRequest)
		code string
	}{
		{"template", func(r *InvestRequest) { r.TemplateID = "nope" }, "UNKNOWN_TEMPLATE"},
		{"retired template", func(r *InvestRequest) { r.TemplateID, r.Params = "all-weather", nil }, "TEMPLATE_RETIRED"},
		{"chain not open", func(r *InvestRequest) { r.ChainID = 1 }, "UNSUPPORTED_CHAIN"},
		{"owner", func(r *InvestRequest) { r.Owner = "0x123" }, "BAD_ADDRESS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := mag7()
			c.edit(&req)
			_, err := h.svc.PrepareInvest(context.Background(), req)
			wantCode(t, err, c.code)
		})
	}
}

func TestConfirmValidatesTheEnrollment(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	prep, err := h.svc.PrepareInvest(ctx, mag7())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		edit func(*ConfirmRequest)
		code string
	}{
		{"signature", func(r *ConfirmRequest) { r.Signature = "0x1234" }, "BAD_SIGNATURE"},
		{"flow", func(r *ConfirmRequest) { r.FlowID = "" }, "BAD_ENROLLMENT"},
		{"chain", func(r *ConfirmRequest) { r.ChainIDs = []int64{1} }, "BAD_ENROLLMENT"},
		{"unknown strategy", func(r *ConfirmRequest) { r.StrategyID = "strat-999" }, "UNKNOWN_STRATEGY"},
		{"other parameters", func(r *ConfirmRequest) { r.Params = map[string]float64{"count": 2, "lookback": 126} }, "STRATEGY_MISMATCH"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := confirmFor(mag7(), prep.Enrollment)
			c.edit(&req)
			_, err := h.svc.ConfirmInvest(ctx, req)
			wantCode(t, err, c.code)
		})
	}
	if n := len(h.portfolioEngine.Requests("POST /enroll")); n != 0 {
		t.Fatalf("an invalid confirm reached PortfolioEngine %d times", n)
	}
}

func TestPrepareHoldsTargetsThatFailAGate(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	var gate *strategy.GateError

	h.backtestEngine.Set(map[string]float64{"XOM": 1}, testNow.AddDate(0, 0, -1))
	if _, err := h.svc.PrepareInvest(ctx, mag7()); !errors.As(err, &gate) || !strings.Contains(gate.Reason, "outside its universe") {
		t.Fatalf("got %v", err)
	}

	// Another parameter set, so the target is not cached: a stale close.
	h.backtestEngine.Set(map[string]float64{"AAPL": 1}, testNow.AddDate(0, 0, -10))
	req := mag7()
	req.Params = map[string]float64{"count": 1, "lookback": 126}
	if _, err := h.svc.PrepareInvest(ctx, req); !errors.As(err, &gate) || !strings.Contains(gate.Reason, "stale") {
		t.Fatalf("got %v", err)
	}
	if n := len(h.portfolioEngine.Requests("POST /strategies")); n != 0 {
		t.Fatalf("a gated target created %d strategies", n)
	}
}

/* ── Re-evaluation ──────────────────────────────────────────────────── */

func TestReevaluatePublishesOnlyNewAllocationsOnNewCloses(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	pf := h.invest(t, mag7())
	combos, err := h.svc.Combos(ctx)
	if err != nil || len(combos) != 1 {
		t.Fatalf("combos %+v, %v", combos, err)
	}
	reevaluate := func() ReevaluationResult { return h.svc.Reevaluate(ctx, combos[0], "manual", 6) }
	publishes := func() []testkit.PortfolioEngineRequest {
		return h.portfolioEngine.Requests("POST /strategies/{id}/versions")
	}

	if r := reevaluate(); r.Status != "held" || !strings.Contains(r.Reason, "no new close") {
		t.Fatalf("same close: %+v", r)
	}

	h.backtestEngine.Set(map[string]float64{"AAPL": 0.5, "BIL": 0.5}, testNow)
	r := reevaluate()
	if r.Status != "published" || len(r.PublishedChains) != 1 || r.PublishedChains[0] != 56 || r.Cutoff != "2026-09-10" {
		t.Fatalf("new weights: %+v", r)
	}
	pubs := publishes()
	if len(pubs) != 1 || !strings.Contains(pubs[0].Body["changeLog"].(string), "data cutoff 2026-09-10") {
		t.Fatalf("publishes %+v", pubs)
	}
	if got := weightsByAsset(pubs[0].Body); got[assetID(t, "AAPL")] != "50" || got[assetID(t, "BIL")] != "50" {
		t.Fatalf("published %v", got)
	}
	p, err := h.svc.Portfolio(ctx, pf.PortfolioID)
	if err != nil || p.Version != 2 || p.Cutoff != "2026-09-10" || len(p.Allocation) != 2 {
		t.Fatalf("portfolio after publish %+v, %v", p, err)
	}

	// The next close with the same weights changes nothing upstream.
	h.svc.now = func() time.Time { return testNow.AddDate(0, 0, 1) }
	h.backtestEngine.Set(map[string]float64{"AAPL": 0.5, "BIL": 0.5}, testNow.AddDate(0, 0, 1))
	if r := reevaluate(); r.Status != "unchanged" || len(publishes()) != 1 {
		t.Fatalf("same weights: %+v", r)
	}

	// An allocation outside the universe is held, never published.
	h.svc.now = func() time.Time { return testNow.AddDate(0, 0, 4) }
	h.backtestEngine.Set(map[string]float64{"XOM": 1}, testNow.AddDate(0, 0, 4))
	if r := reevaluate(); r.Status != "held" || !strings.Contains(r.Reason, "outside its universe") || len(publishes()) != 1 {
		t.Fatalf("bad universe: %+v", r)
	}

	evals, _ := h.svc.RecentEvaluations(ctx, 20)
	count := map[string]int{}
	for _, e := range evals {
		count[e.Status]++
	}
	if count["published"] != 2 || count["held"] != 2 || count["unchanged"] != 1 {
		t.Fatalf("evaluations %v", count)
	}
}

/* ── Portfolios ─────────────────────────────────────────────────────── */

func TestPortfolioReadsAndOperations(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	pf := h.invest(t, mag7())

	d, err := h.svc.Portfolio(ctx, pf.PortfolioID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Schedule == nil || d.Schedule.Status != "active" || d.Schedule.Frequency == nil || *d.Schedule.Frequency != "hourly" || d.Schedule.NextDueAt == nil {
		t.Fatalf("schedule %+v", d.Schedule)
	}
	b, _ := json.Marshal(d)
	for _, leak := range []string{"strategyName", "ownerAccountId", "intervalMs"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("portfolio detail leaks the upstream record (%s): %s", leak, b)
		}
	}

	op, err := h.svc.Deploy(ctx, pf.PortfolioID)
	if err != nil || op.OperationID != "op-1" {
		t.Fatalf("deploy %+v, %v", op, err)
	}
	if n := len(h.portfolioEngine.Requests("POST /portfolios/{id}/rebalance")); n != 1 {
		t.Fatalf("%d rebalances", n)
	}
	if raw, err := h.svc.Operation(ctx, pf.PortfolioID, "op-1"); err != nil || !strings.Contains(string(raw), "completed") {
		t.Fatalf("operation %s, %v", raw, err)
	}
	if _, err := h.svc.Positions(ctx, pf.PortfolioID); err != nil {
		t.Fatal(err)
	}
	if list, err := h.svc.ListPortfolios(ctx, owner, false); err != nil || len(list) != 1 {
		t.Fatalf("list %+v, %v", list, err)
	}

	_, err = h.svc.Portfolio(ctx, "pf-missing")
	wantCode(t, err, "PORTFOLIO_NOT_FOUND")
	_, err = h.svc.ListPortfolios(ctx, "bob", false)
	wantCode(t, err, "BAD_ADDRESS")
}

func TestWithdrawalsPayOnlyTheOwnerInTheChainsCash(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	id := h.invest(t, mag7()).PortfolioID
	bsc, _ := tokens.ChainByID(56)

	raw, err := h.svc.WithdrawSignature(ctx, id, WithdrawRequest{Mode: "liquidate"})
	if err != nil || !strings.Contains(string(raw), "typedData") {
		t.Fatalf("liquidate signature %s, %v", raw, err)
	}
	body := h.portfolioEngine.Requests("POST /portfolios/{id}/liquidate-all/signature")[0].Body
	if body["recipientAccountId"] != "eip155:56:"+ownerLower || body["settlementAssetId"] != bsc.CashAssetID() {
		t.Fatalf("liquidation %v", body)
	}

	if _, err := h.svc.WithdrawSignature(ctx, id, WithdrawRequest{Mode: "withdraw", Assets: []portfolioengine.WithdrawAsset{{AssetID: assetID(t, "AAPL"), AmountRaw: "1000"}}}); err != nil {
		t.Fatal(err)
	}
	body = h.portfolioEngine.Requests("POST /portfolios/{id}/withdraw/signature")[0].Body
	if body["recipientAccountId"] != "eip155:56:"+ownerLower || body["liquidate"] != false {
		t.Fatalf("in-kind withdrawal %v", body)
	}

	ethAAPL, _ := tokens.AssetID("AAPL", 1)
	bad := map[string]WithdrawRequest{
		"no assets":       {Mode: "withdraw"},
		"other chain":     {Mode: "withdraw", Assets: []portfolioengine.WithdrawAsset{{AssetID: ethAAPL, AmountRaw: "1"}}},
		"zero amount":     {Mode: "withdraw", Assets: []portfolioengine.WithdrawAsset{{AssetID: assetID(t, "AAPL"), AmountRaw: "0"}}},
		"fraction amount": {Mode: "withdraw", Assets: []portfolioengine.WithdrawAsset{{AssetID: assetID(t, "AAPL"), AmountRaw: "1.5"}}},
		"unknown mode":    {Mode: "burn"},
	}
	for name, req := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.WithdrawSignature(ctx, id, req)
			wantCode(t, err, "BAD_WITHDRAWAL")
		})
	}

	op, err := h.svc.Withdraw(ctx, id, WithdrawSubmit{Mode: "liquidate", Message: json.RawMessage(`{"nonce":"1"}`), Signature: sig})
	if err != nil || op.OperationID != "op-1" {
		t.Fatalf("withdraw %+v, %v", op, err)
	}
	if got := h.portfolioEngine.Requests("POST /portfolios/{id}/liquidate-all"); len(got) != 1 || got[0].Body["signature"] != sig {
		t.Fatalf("liquidate-all %+v", got)
	}
	_, err = h.svc.Withdraw(ctx, id, WithdrawSubmit{Mode: "liquidate", Message: json.RawMessage(`{}`), Signature: "nope"})
	wantCode(t, err, "BAD_WITHDRAWAL")
	_, err = h.svc.Withdraw(ctx, id, WithdrawSubmit{Mode: "burn", Message: json.RawMessage(`{}`), Signature: sig})
	wantCode(t, err, "BAD_WITHDRAWAL")
	_, err = h.svc.WithdrawSignature(ctx, "pf-missing", WithdrawRequest{Mode: "liquidate"})
	wantCode(t, err, "PORTFOLIO_NOT_FOUND")
}

func TestPortfolioGoneUpstreamIsDropped(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	id := h.invest(t, mag7()).PortfolioID
	h.portfolioEngine.Handle("GET /portfolios/{id}/positions", func(r *http.Request, _ map[string]any) (int, any) {
		return http.StatusNotFound, testkit.PortfolioEngineError{Code: "PORTFOLIO_NOT_FOUND", Message: "Unknown portfolio " + r.PathValue("id")}
	})

	_, err := h.svc.Positions(ctx, id)
	var ge *portfolioengine.Error
	if !errors.As(err, &ge) || !ge.NotFound() {
		t.Fatalf("got %v", err)
	}
	if list, _ := h.svc.ListPortfolios(ctx, owner, false); len(list) != 0 {
		t.Fatalf("a dropped portfolio is still listed: %+v", list)
	}
	_, err = h.svc.Portfolio(ctx, id)
	wantCode(t, err, "PORTFOLIO_NOT_FOUND")
	if prep, err := h.svc.PrepareInvest(ctx, mag7()); err != nil || prep.Status != "sign" {
		t.Fatalf("the owner cannot enroll again: %+v, %v", prep, err)
	}
}

func TestStrategyNameShowsOnlyNonDefaultParams(t *testing.T) {
	tpl, _ := strategy.ByID("mag7-rank")
	target := func(p strategy.Params) *investTarget { return &investTarget{tpl: tpl, params: p, key: tpl.Key(p)} }
	if got := strategyName(target(tpl.Defaults())); got != "Mag7 Momentum Rank" {
		t.Fatalf("defaults named %q", got)
	}
	tuned := tpl.Defaults()
	tuned["lookback"] = 91
	if got := strategyName(target(tuned)); got != "Mag7 Momentum Rank (91d · 3)" {
		t.Fatalf("tuned named %q", got)
	}
}
