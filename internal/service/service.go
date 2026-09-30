// Package service is the application layer: research (backtests, current
// targets), investing (portfolio engine strategy + enrollment), portfolio operations,
// and the standing re-evaluation that keeps published allocations current.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/brighter-di/backend/internal/backtestengine"
	"github.com/brighter-di/backend/internal/config"
	"github.com/brighter-di/backend/internal/metrics"
	"github.com/brighter-di/backend/internal/notify"
	"github.com/brighter-di/backend/internal/portfolioengine"
	"github.com/brighter-di/backend/internal/prices"
	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/strategy"
	"github.com/brighter-di/backend/internal/tokens"
)

// Error is a client-facing failure with an HTTP status and a stable code.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func errf(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

const (
	dateFmt = "2006-01-02"

	// The backtest engine's inputs change once per trading day; these bound how stale a
	// cached answer can be while keeping the public API off the backtest engine's limits.
	backtestTTL = 6 * time.Hour
	targetTTL   = 20 * time.Minute
)

type Service struct {
	cfg             *config.Config
	store           *store.Store
	catalog         publicationCatalog
	backtestEngine  *backtestengine.Client
	portfolioEngine *portfolioengine.Client
	prices          *prices.Client
	notifier        *notify.Notifier
	log             *slog.Logger

	cache  *ttlCache
	flight singleflight.Group
	now    func() time.Time
}

func New(cfg *config.Config, st *store.Store, cc *backtestengine.Client, gc *portfolioengine.Client, pc *prices.Client, log *slog.Logger) *Service {
	var catalog publicationCatalog
	if st != nil {
		catalog = st
	}
	return &Service{
		cfg: cfg, store: st, catalog: catalog, backtestEngine: cc, portfolioEngine: gc, prices: pc,
		log:   log.With("component", "service"),
		cache: newTTLCache(),
		now:   time.Now,
	}
}

// WithNotifier sends events (new wallets and portfolios, allocation changes,
// rebalances, withdrawals) to n. Without one they are only logged.
func (s *Service) WithNotifier(n *notify.Notifier) *Service {
	s.notifier = n
	return s
}

// announce tells operators about an event in full, and everyone subscribed to
// the bot without portfolio ids or wallet addresses: anyone can subscribe,
// and a portfolio id opens its holdings. An empty public text keeps the
// event to operators.
func (s *Service) announce(operator, public string) {
	s.notifier.Send(notify.Message{Operator: operator, Public: public})
}

// announceAll is an event with nothing private in it.
func (s *Service) announceAll(text string) { s.announce(text, text) }

// shortAddr renders 0x1234…abcd for messages.
func shortAddr(a string) string {
	if len(a) < 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}

// allocText renders basis points as "AAPL 33.8%, MSFT 33.2%", largest first.
func allocText(bps map[string]int) string {
	lines := targetLines(bps, nil)
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = fmt.Sprintf("%s %.1f%%", l.Ticker, float64(l.Bps)/100)
	}
	return strings.Join(parts, ", ")
}

func (s *Service) today() string { return s.now().UTC().Format(dateFmt) }

/* ── Catalogue ──────────────────────────────────────────────────────── */

type ConfigView struct {
	Chains                  []tokens.Chain           `json:"chains"`
	Research                bool                     `json:"research"` // The backtest engine configured
	Trading                 bool                     `json:"trading"`  // PortfolioEngine configured
	Schedule                portfolioengine.Schedule `json:"schedule"`
	MaxCutoffAgeTradingDays int                      `json:"maxCutoffAgeTradingDays"`
}

// enabledChains are the chains open for new investment, in configured order.
func (s *Service) enabledChains() []tokens.Chain {
	out := make([]tokens.Chain, 0, len(s.cfg.EnabledChains))
	for _, id := range s.cfg.EnabledChains {
		if c, ok := tokens.ChainByID(id); ok {
			out = append(out, c)
		}
	}
	return out
}

func (s *Service) chainEnabled(id int64) (tokens.Chain, bool) {
	for _, c := range s.enabledChains() {
		if c.ID == id {
			return c, true
		}
	}
	return tokens.Chain{}, false
}

func (s *Service) Config() ConfigView {
	v := ConfigView{
		Chains:                  s.enabledChains(),
		Research:                s.backtestEngine.Configured(),
		Trading:                 s.portfolioEngine.Configured(),
		MaxCutoffAgeTradingDays: s.cfg.MaxCutoffAgeTradingDays,
	}
	v.Schedule = s.strategySchedule()
	return v
}

type AssetView struct {
	Ticker     string            `json:"ticker"`
	Name       string            `json:"name"`
	OndoSymbol string            `json:"ondoSymbol"`
	Kind       string            `json:"kind"`
	ISIN       string            `json:"isin"`
	Icon       string            `json:"icon"`
	Addresses  map[string]string `json:"addresses"`
}

func assetView(a tokens.Asset) AssetView {
	return AssetView{
		Ticker: a.Ticker, Name: a.Name, OndoSymbol: a.OndoSymbol, Kind: a.Kind, ISIN: a.ISIN,
		Icon: a.Icon(), Addresses: a.Addresses(),
	}
}

type TemplateView struct {
	*strategy.Template
	Defaults strategy.Params `json:"defaults"`
	Assets   []AssetView     `json:"assets"` // everything the template can hold
}

func (s *Service) Templates() []TemplateView {
	out := make([]TemplateView, 0, len(strategy.Templates))
	for _, t := range strategy.Templates {
		v := s.templateView(t)
		out = append(out, v)
	}
	return out
}

func (s *Service) templateView(t *strategy.Template) TemplateView {
	if t.Author == "" {
		copy := *t
		copy.Author = "Brighter"
		t = &copy
	}
	v := TemplateView{Template: t, Defaults: t.Defaults(), Assets: []AssetView{}}
	for _, ticker := range t.Holdable() {
		if a, ok := tokens.Lookup(ticker); ok {
			v.Assets = append(v.Assets, assetView(a))
		} else {
			v.Assets = append(v.Assets, AssetView{Ticker: ticker, Name: ticker, OndoSymbol: ticker, Addresses: map[string]string{}})
		}
	}
	return v
}

/* ── Backtests ──────────────────────────────────────────────────────── */

var Ranges = map[string]int{"1y": 1, "3y": 3, "5y": 5, "10y": 10}

type SeriesView struct {
	Dates     []string   `json:"dates"`
	Strategy  []float64  `json:"strategy"`
	Benchmark []*float64 `json:"benchmark"`
}

type BenchmarkView struct {
	Ticker  string           `json:"ticker"`
	Metrics *metrics.Metrics `json:"metrics"`
}

// HoldingsView is what the model held on the last day of a backtest. It is
// HISTORICAL — a property of the backtest, never an executable target.
type HoldingsView struct {
	Date    string             `json:"date"`
	Weights map[string]float64 `json:"weights"`
}

type BacktestView struct {
	Source         string          `json:"source"`
	TemplateID     string          `json:"templateId"`
	Params         strategy.Params `json:"params"`
	ParamsKey      string          `json:"paramsKey"`
	Range          string          `json:"range"`
	StartDate      string          `json:"startDate"`
	EndDate        string          `json:"endDate"`
	Metrics        metrics.Metrics `json:"metrics"`
	Benchmark      BenchmarkView   `json:"benchmark"`
	Series         SeriesView      `json:"series"`
	EndingHoldings *HoldingsView   `json:"endingHoldings"`
	ComputedAt     time.Time       `json:"computedAt"`
}

// Downsample keeps at most n points (always including the last), for cards
// and sparklines that do not need a daily series.
func (b BacktestView) Downsample(n int) BacktestView {
	total := len(b.Series.Dates)
	if n <= 1 || total <= n {
		return b
	}
	step := (total + n - 1) / n
	var s SeriesView
	for i := 0; i < total; i += step {
		s.Dates = append(s.Dates, b.Series.Dates[i])
		s.Strategy = append(s.Strategy, b.Series.Strategy[i])
		s.Benchmark = append(s.Benchmark, b.Series.Benchmark[i])
	}
	if last := total - 1; s.Dates[len(s.Dates)-1] != b.Series.Dates[last] {
		s.Dates = append(s.Dates, b.Series.Dates[last])
		s.Strategy = append(s.Strategy, b.Series.Strategy[last])
		s.Benchmark = append(s.Benchmark, b.Series.Benchmark[last])
	}
	b.Series = s
	return b
}

// Since keeps the days on or after date (YYYY-MM-DD). The client keeps the
// earlier days; the first one returned overlaps what it holds, so it can
// line the new days up with its own series.
func (b BacktestView) Since(date string) BacktestView {
	i, _ := slices.BinarySearch(b.Series.Dates, date)
	b.Series = SeriesView{Dates: b.Series.Dates[i:], Strategy: b.Series.Strategy[i:], Benchmark: b.Series.Benchmark[i:]}
	return b
}

func (s *Service) Backtest(ctx context.Context, tpl *strategy.Template, params strategy.Params, rng string) (*BacktestView, error) {
	years, ok := Ranges[rng]
	if !ok {
		return nil, errf(400, "BAD_RANGE", "range must be one of 1y, 3y, 5y, 10y")
	}
	key := strings.Join([]string{"bt", tpl.ID, tpl.Key(params), rng, s.today()}, "|")
	base := BacktestView{Source: "backtest", TemplateID: tpl.ID, Params: params, ParamsKey: tpl.Key(params), Range: rng}
	tree, err := tpl.TreeChecked(params)
	if err != nil {
		return nil, customStrategyError(err)
	}
	return s.backtestTree(ctx, key, years, func() map[string]any { return tree }, tpl.Benchmark, base)
}

// BacktestCustom backtests a strategy composed in Brighter Studio. It uses the
// same inline backtest engine backtest as the templates, which is read-only: nothing
// is saved to BacktestEngine.
func (s *Service) BacktestCustom(ctx context.Context, c strategy.CustomStrategy, rng string) (*BacktestView, error) {
	years, ok := Ranges[rng]
	if !ok {
		return nil, errf(400, "BAD_RANGE", "range must be one of 1y, 3y, 5y, 10y")
	}
	compiled, err := strategy.CompileCustom(c)
	if err != nil {
		var invalid *strategy.CustomError
		if errors.As(err, &invalid) {
			return nil, errf(422, "INVALID_STRATEGY", "%s", invalid.Reason)
		}
		return nil, err
	}
	key := strings.Join([]string{"bt", "custom", compiled.Key, rng, s.today()}, "|")
	base := BacktestView{Source: "backtest", TemplateID: "custom", ParamsKey: compiled.Key, Range: rng}
	v, err := s.backtestTree(ctx, key, years, func() map[string]any { return compiled.Tree }, compiled.Benchmark, base)
	var upstream *backtestengine.Error
	if errors.As(err, &upstream) && upstream.Status >= 400 && upstream.Status < 500 &&
		upstream.Status != 401 && upstream.Status != 403 && upstream.Status != 429 {
		s.log.Warn("backtestEngine rejected a custom strategy", "key", compiled.Key, "err", err)
		return nil, customBacktestRejection(upstream)
	}
	return v, err
}

// backtestTree runs one inline backtest per key per day and shapes the result.
func (s *Service) backtestTree(ctx context.Context, key string, years int, tree func() map[string]any, benchmark string, base BacktestView) (*BacktestView, error) {
	v, err := s.cached(ctx, key, cachePolicy{ttl: backtestTTL}, func(ctx context.Context) (any, error) {
		end := s.now().UTC()
		start := end.AddDate(-years, 0, 0)
		bt, err := s.backtestEngine.Backtest(ctx, backtestengine.BacktestRequest{
			Tree: tree(), StartDate: start.Format(dateFmt), EndDate: end.Format(dateFmt), Benchmark: benchmark,
		})
		if err != nil {
			return nil, err
		}
		curve, err := bt.Curve()
		if err != nil {
			return nil, &backtestengine.Error{Status: 502, Method: "POST", Path: "/v1/backtest", Detail: err.Error()}
		}
		view := base
		view.StartDate = backtestengine.EpochDayToISO(curve.Days[0])
		view.EndDate = backtestengine.EpochDayToISO(curve.Days[len(curve.Days)-1])
		view.Benchmark = BenchmarkView{Ticker: benchmark}
		view.ComputedAt = s.now().UTC()
		view.Metrics, _ = metrics.Compute(curve.Days, curve.Strategy)
		var bDays []int
		var bVals []float64
		for i, v := range curve.Benchmark {
			if v != nil {
				bDays = append(bDays, curve.Days[i])
				bVals = append(bVals, *v)
			}
		}
		if m, ok := metrics.Compute(bDays, bVals); ok {
			view.Benchmark.Metrics = &m
		}
		view.Series = SeriesView{Dates: make([]string, len(curve.Days)), Strategy: curve.Strategy, Benchmark: curve.Benchmark}
		for i, d := range curve.Days {
			view.Series.Dates[i] = backtestengine.EpochDayToISO(d)
		}
		if a, ok := bt.LatestAllocation(); ok {
			view.EndingHoldings = &HoldingsView{Date: a.Date, Weights: a.Weights}
		}
		return &view, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*BacktestView), nil
}

/* ── Current targets ────────────────────────────────────────────────── */

// target is one evaluation of a (template, params) against the latest close.
type target struct {
	tpl         *strategy.Template
	params      strategy.Params
	key         string
	cutoff      string
	marketDay   string
	lagDays     int
	weights     map[string]float64
	bps         map[string]int
	hash        string
	gateErr     error // incomplete, or names an asset outside the universe
	evaluatedAt time.Time
}

// evaluate derives the current target account-independently: backtest through
// the latest close and read the model's allocation on the final day the backtest engine
// has weights for. No account, no order, no funding. The target is dated by
// the data behind it, never later.
func (s *Service) evaluate(ctx context.Context, tpl *strategy.Template, params strategy.Params) (*target, error) {
	tree, err := tpl.TreeChecked(params)
	if err != nil {
		return nil, customStrategyError(err)
	}
	end := s.now().UTC()
	bt, err := s.backtestEngine.Backtest(ctx, backtestengine.BacktestRequest{
		Tree: tree, StartDate: end.AddDate(-strategy.TargetWindowYears(tree), 0, 0).Format(dateFmt),
		EndDate: end.Format(dateFmt), Benchmark: tpl.Benchmark,
	})
	if err != nil {
		return nil, err
	}
	alloc, ok := bt.LatestAllocation()
	if !ok {
		return nil, &backtestengine.Error{Status: 502, Method: "POST", Path: "/v1/backtest",
			Detail: "backtest has no allocation series, so a current target cannot be derived"}
	}
	t := &target{
		tpl: tpl, params: params, key: tpl.Key(params),
		cutoff: alloc.Date, marketDay: alloc.MarketDay, lagDays: alloc.LagDays,
		weights: alloc.Weights, evaluatedAt: s.now().UTC(),
	}
	if t.bps, t.gateErr = strategy.ToBasisPoints(alloc.Weights); t.gateErr == nil {
		t.hash = strategy.ChangeHash(t.bps)
		t.gateErr = tpl.CheckHoldable(t.bps)
	}
	s.cache.set(targetCacheKey(tpl, params), t, targetTTL, 0)
	return t, nil
}

func targetCacheKey(tpl *strategy.Template, params strategy.Params) string {
	return "target|" + tpl.ID + "|" + tpl.Key(params)
}

// currentTarget has no grace period on purpose: a target that failed to
// refresh must not stand in for one that reaches a strategy.
func (s *Service) currentTarget(ctx context.Context, tpl *strategy.Template, params strategy.Params) (*target, error) {
	v, err := s.cached(ctx, targetCacheKey(tpl, params), cachePolicy{ttl: targetTTL}, func(ctx context.Context) (any, error) {
		return s.evaluate(ctx, tpl, params)
	})
	if err != nil {
		return nil, err
	}
	return v.(*target), nil
}

// executable is every gate a target must pass before it reaches a portfolio engine
// strategy on a chain. Each failure is reported verbatim; none degrades.
func (s *Service) executable(t *target, chainID int64) error {
	if t.gateErr != nil {
		return t.gateErr
	}
	f := strategy.CheckFreshness(t.cutoff, t.lagDays, s.now(), s.cfg.MaxCutoffAgeTradingDays, s.cfg.MaxDataLagDays)
	if !f.Fresh {
		return &strategy.GateError{Reason: f.Reason}
	}
	_, err := strategy.AllocationFor(t.bps, chainID)
	return err
}

type TargetLine struct {
	Ticker     string  `json:"ticker"`
	Name       string  `json:"name"`
	OndoSymbol string  `json:"ondoSymbol"`
	Icon       string  `json:"icon"`
	Bps        int     `json:"bps"`
	Weight     float64 `json:"weight"` // The backtest engine's raw weight, before rounding
}

type ChainReadiness struct {
	ChainID    int64  `json:"chainId"`
	Executable bool   `json:"executable"`
	Reason     string `json:"reason,omitempty"`
}

type TargetView struct {
	Source         string           `json:"source"`
	TemplateID     string           `json:"templateId"`
	Params         strategy.Params  `json:"params"`
	ParamsKey      string           `json:"paramsKey"`
	Cutoff         string           `json:"cutoff"`
	MarketDay      string           `json:"marketDay,omitempty"`
	DataLagDays    int              `json:"dataLagDays"`
	AgeTradingDays int              `json:"ageTradingDays"`
	Fresh          bool             `json:"fresh"`
	Problem        string           `json:"problem,omitempty"`
	Lines          []TargetLine     `json:"lines"`
	ChangeHash     string           `json:"changeHash,omitempty"`
	Chains         []ChainReadiness `json:"chains"`
	EvaluatedAt    time.Time        `json:"evaluatedAt"`
}

func (s *Service) Target(ctx context.Context, tpl *strategy.Template, params strategy.Params) (*TargetView, error) {
	t, err := s.currentTarget(ctx, tpl, params)
	if err != nil {
		return nil, err
	}
	f := strategy.CheckFreshness(t.cutoff, t.lagDays, s.now(), s.cfg.MaxCutoffAgeTradingDays, s.cfg.MaxDataLagDays)
	v := &TargetView{
		Source: "backtest", TemplateID: tpl.ID, Params: params, ParamsKey: t.key,
		Cutoff: t.cutoff, MarketDay: t.marketDay, DataLagDays: t.lagDays,
		AgeTradingDays: f.AgeTradingDays, Fresh: f.Fresh, ChangeHash: t.hash,
		Lines: targetLines(t.bps, t.weights), EvaluatedAt: t.evaluatedAt,
	}
	switch {
	case t.gateErr != nil:
		v.Problem = t.gateErr.Error()
	case !f.Fresh:
		v.Problem = f.Reason
	}
	for _, c := range s.enabledChains() {
		r := ChainReadiness{ChainID: c.ID, Executable: true}
		if err := s.executable(t, c.ID); err != nil {
			r.Executable, r.Reason = false, err.Error()
		}
		v.Chains = append(v.Chains, r)
	}
	return v, nil
}

func targetLines(bps map[string]int, raw map[string]float64) []TargetLine {
	lines := make([]TargetLine, 0, len(bps))
	for ticker, b := range bps {
		l := TargetLine{Ticker: ticker, Name: tokens.DisplayName(ticker), Bps: b, Weight: raw[ticker]}
		if a, ok := tokens.Lookup(ticker); ok {
			l.OndoSymbol, l.Icon = a.OndoSymbol, a.Icon()
		}
		lines = append(lines, l)
	}
	sortLines(lines)
	return lines
}

func sortLines(lines []TargetLine) {
	for i := 1; i < len(lines); i++ {
		for j := i; j > 0 && (lines[j].Bps > lines[j-1].Bps || (lines[j].Bps == lines[j-1].Bps && lines[j].Ticker < lines[j-1].Ticker)); j-- {
			lines[j], lines[j-1] = lines[j-1], lines[j]
		}
	}
}

// cachePolicy is how long a result is fresh, and for how long after that it
// may stand in as the last good copy when a refresh fails. Zero grace means an
// expired result is never served.
type cachePolicy struct {
	ttl   time.Duration
	grace time.Duration
}

type admitKey struct{}

// WithAdmit attaches an admission check to ctx. The service calls it before a
// request's work goes upstream (a cache miss), so a rate limit built on it
// never counts answers that are already cached. A non-nil error is returned
// to the caller as is.
func WithAdmit(ctx context.Context, admit func() error) context.Context {
	return context.WithValue(ctx, admitKey{}, admit)
}

// cached returns a cached value or computes it once for all concurrent
// callers. The computation is detached from any single request: one user
// closing their tab must not fail everyone else waiting on the same backtest.
func (s *Service) cached(ctx context.Context, key string, p cachePolicy, fn func(context.Context) (any, error)) (any, error) {
	if v, ok := s.cache.get(key); ok {
		return v, nil
	}
	if admit, ok := ctx.Value(admitKey{}).(func() error); ok {
		if err := admit(); err != nil {
			if v, ok := s.lastGood(key, p, err); ok {
				return v, nil
			}
			return nil, err
		}
	}
	ch := s.flight.DoChan(key, func() (any, error) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Minute)
		defer cancel()
		v, err := fn(cctx)
		if err == nil {
			s.cache.set(key, v, p.ttl, p.grace)
		}
		return v, err
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			if v, ok := s.lastGood(key, p, r.Err); ok {
				return v, nil
			}
		}
		return r.Val, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// lastGood is the expired copy of key, when its policy allows serving one in
// place of a failed refresh.
func (s *Service) lastGood(key string, p cachePolicy, cause error) (any, bool) {
	if p.grace <= 0 {
		return nil, false
	}
	v, ok := s.cache.lastGood(key)
	if ok {
		s.log.Warn("refresh failed; serving the last good copy", "key", key, "err", cause)
	}
	return v, ok
}

// warmBackoff is the pause before retrying a warm-up call the backtest engine refused.
const warmBackoff = 20 * time.Second

// warmRetry retries one warm-up call when the backtest engine answers 429, which it does
// when a restart starts warming every strategy with an empty cache. Failures
// are never cached, so a retry really asks again; any other error returns.
func (s *Service) warmRetry(ctx context.Context, call func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = call(); err == nil {
			return nil
		}
		var upstream *backtestengine.Error
		if !errors.As(err, &upstream) || upstream.Status != 429 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(warmBackoff):
		}
	}
	return err
}

// Warm keeps every listed strategy's default backtests and target cached,
// published ones included, so the strategy list and the landing page render
// without waiting on BacktestEngine.
// warmRanges are the backtest windows kept hot for every listed strategy: the
// ones the strategy list offers, plus the five years the landing page draws.
// CataloguePeriods may only name ranges from this list.
var warmRanges = []string{"1y", "3y", "5y"}

func (s *Service) Warm(ctx context.Context) {
	warm := func() {
		templates, err := s.PublicTemplates(ctx)
		if err != nil {
			s.log.Warn("warmup could not list published strategies", "err", err)
			templates = s.Templates()
		}
		for _, t := range templates {
			if ctx.Err() != nil {
				return
			}
			// Nobody browses a retired template; its portfolios are kept
			// current by the scheduler.
			if t.Retired {
				continue
			}
			for _, rng := range warmRanges {
				if err := s.warmRetry(ctx, func() error { _, err := s.Backtest(ctx, t.Template, t.Defaults, rng); return err }); err != nil {
					s.log.Warn("warmup backtest failed", "template", t.ID, "range", rng, "err", err)
				}
			}
			if err := s.warmRetry(ctx, func() error { _, err := s.currentTarget(ctx, t.Template, t.Defaults); return err }); err != nil {
				s.log.Warn("warmup target failed", "template", t.ID, "err", err)
			}
		}
		s.log.Info("warmup complete")
	}
	warm()
	tick := time.NewTicker(targetTTL - time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			warm()
		}
	}
}

/* ── Investing ──────────────────────────────────────────────────────── */

var evmAddress = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
var hexSig = regexp.MustCompile(`^0x[0-9a-fA-F]{130,}$`)

type InvestRequest struct {
	TemplateID string             `json:"templateId"`
	Params     map[string]float64 `json:"params"`
	ChainID    int64              `json:"chainId"`
	Owner      string             `json:"owner"`
}

type investTarget struct {
	tpl    *strategy.Template
	params strategy.Params
	key    string
	chain  tokens.Chain
	owner  string
}

func (s *Service) resolve(ctx context.Context, r InvestRequest) (*investTarget, error) {
	tpl, err := s.Template(ctx, r.TemplateID)
	if err != nil {
		return nil, err
	}
	if tpl.Retired {
		return nil, errf(410, "TEMPLATE_RETIRED", "%s is closed to new investment.", tpl.Name)
	}
	chain, ok := s.chainEnabled(r.ChainID)
	if !ok {
		return nil, errf(400, "UNSUPPORTED_CHAIN", "Chain %d is not open for investment.", r.ChainID)
	}
	owner, err := normaliseOwner(r.Owner)
	if err != nil {
		return nil, err
	}
	params := tpl.Coerce(func(k string) (float64, bool) { v, ok := r.Params[k]; return v, ok })
	return &investTarget{tpl: tpl, params: params, key: tpl.Key(params), chain: chain, owner: owner}, nil
}

func normaliseOwner(s string) (string, error) {
	if !evmAddress.MatchString(s) {
		return "", errf(400, "BAD_ADDRESS", "owner must be a 0x-prefixed EVM address")
	}
	return strings.ToLower(s), nil
}

type Enrollment struct {
	StrategyID     string                        `json:"strategyId"`
	ChainIDs       []int64                       `json:"chainIds"`
	FlowID         string                        `json:"flowId"`
	AccountIndex   string                        `json:"accountIndex"`
	AgentAccountID string                        `json:"agentAccountId"`
	AccountType    string                        `json:"accountType,omitempty"`
	Message        portfolioengine.EnrollMessage `json:"message"`
}

type PrepareResult struct {
	// "sign": the wallet must sign enrollment.message.raw (EIP-191) and send
	// it to /v1/invest/confirm. "existing": the user already holds this
	// strategy on this chain — fund the same portfolio.
	Status     string         `json:"status"`
	Portfolio  *PortfolioView `json:"portfolio,omitempty"`
	Enrollment *Enrollment    `json:"enrollment,omitempty"`
}

func (s *Service) PrepareInvest(ctx context.Context, req InvestRequest) (*PrepareResult, error) {
	it, err := s.resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	if !s.portfolioEngine.Configured() {
		return nil, portfolioengine.ErrNotConfigured
	}
	st, err := s.ensureStrategy(ctx, it)
	if err != nil {
		return nil, err
	}
	if p, err := s.store.FindPortfolio(ctx, it.owner, st.ID); err == nil {
		return &PrepareResult{Status: "existing", Portfolio: s.portfolioView(ctx, p)}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	sig, err := s.portfolioEngine.EnrollSignature(ctx, portfolioengine.EnrollSignatureRequest{
		OwnerAccountID: "eip155:0:" + it.owner,
		StrategyID:     st.PortfolioEngineStrategyID,
		ChainIDs:       []int64{it.chain.ID},
		AccountType:    "ECDSA",
	})
	if err != nil {
		return nil, err
	}
	if sig.Message.Kind != "ecdsa" || !strings.HasPrefix(sig.Message.Raw, "0x") {
		return nil, errf(502, "UNEXPECTED_SIGNABLE", "PortfolioEngine returned a %q enrollment message; only ECDSA wallets are supported.", sig.Message.Kind)
	}
	return &PrepareResult{Status: "sign", Enrollment: &Enrollment{
		StrategyID: st.PortfolioEngineStrategyID, ChainIDs: []int64{it.chain.ID},
		FlowID: sig.FlowID, AccountIndex: sig.AccountIndex, AgentAccountID: sig.AgentAccountID,
		AccountType: sig.AccountType, Message: sig.Message,
	}}, nil
}

type ConfirmRequest struct {
	InvestRequest
	StrategyID     string  `json:"strategyId"`
	ChainIDs       []int64 `json:"chainIds"`
	FlowID         string  `json:"flowId"`
	AccountIndex   string  `json:"accountIndex"`
	AgentAccountID string  `json:"agentAccountId"`
	AccountType    string  `json:"accountType"`
	Signature      string  `json:"signature"`
}

func (s *Service) ConfirmInvest(ctx context.Context, req ConfirmRequest) (*PortfolioView, error) {
	it, err := s.resolve(ctx, req.InvestRequest)
	if err != nil {
		return nil, err
	}
	if !s.portfolioEngine.Configured() {
		return nil, portfolioengine.ErrNotConfigured
	}
	switch {
	case !hexSig.MatchString(req.Signature):
		return nil, errf(400, "BAD_SIGNATURE", "signature must be a 0x-prefixed hex ECDSA signature")
	case req.FlowID == "" || req.AccountIndex == "" || req.AgentAccountID == "":
		return nil, errf(400, "BAD_ENROLLMENT", "flowId, accountIndex and agentAccountId from /v1/invest/prepare are required")
	case len(req.ChainIDs) != 1 || req.ChainIDs[0] != it.chain.ID:
		return nil, errf(400, "BAD_ENROLLMENT", "chainIds must be exactly [%d]", it.chain.ID)
	}
	st, err := s.store.GetStrategyByPortfolioEngineID(ctx, req.StrategyID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errf(400, "UNKNOWN_STRATEGY", "Strategy %s is not a Brighter strategy.", req.StrategyID)
	} else if err != nil {
		return nil, err
	}
	if st.TemplateID != it.tpl.ID || st.ParamsKey != it.key || st.ChainID != it.chain.ID {
		return nil, errf(400, "STRATEGY_MISMATCH", "The signed enrollment is for a different strategy than requested.")
	}
	if p, err := s.store.FindPortfolio(ctx, it.owner, st.ID); err == nil {
		return s.portfolioView(ctx, p), nil
	}

	// Past this point the portfolio engine may create a portfolio, so a client hanging up
	// must not stop us recording it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	res, err := s.portfolioEngine.Enroll(ctx, portfolioengine.EnrollRequest{
		OwnerAccountID: "eip155:0:" + it.owner,
		StrategyID:     st.PortfolioEngineStrategyID,
		ChainIDs:       []int64{it.chain.ID},
		AccountType:    req.AccountType,
		AccountIndex:   req.AccountIndex,
		AgentAccountID: req.AgentAccountID,
		FlowID:         req.FlowID,
		Signature:      req.Signature,
		PortfolioName:  portfolioName(it.tpl),
	})
	if err != nil {
		return nil, err
	}
	deposit := ""
	for _, a := range res.SmartAccounts {
		id := a.DepositAccountID
		if id == "" {
			id = a.AccountID
		}
		if addr, chainID, ok := parseAccountID(id); ok && chainID == it.chain.ID {
			deposit = addr
			break
		}
	}
	if deposit == "" {
		s.log.Error("enrollment returned no smart account on the requested chain", "portfolio", res.PortfolioID, "chain", it.chain.ID)
		return nil, errf(502, "ENROLL_NO_ACCOUNT", "PortfolioEngine created portfolio %s but returned no %s smart account.", res.PortfolioID, it.chain.Name)
	}
	if err := s.store.InsertPortfolio(ctx, res.PortfolioID, it.owner, st.ID, it.chain.ID, deposit); err != nil {
		s.log.Error("enrolled portfolio could not be recorded", "portfolio", res.PortfolioID, "owner", it.owner, "err", err)
		return nil, err
	}
	s.log.Info("portfolio enrolled", "portfolio", res.PortfolioID, "template", it.tpl.ID, "params", it.key, "chain", it.chain.ID)
	s.announce(fmt.Sprintf("New portfolio: %s (%s) on %s by %s.", it.tpl.Name, it.tpl.Label(it.params), it.chain.Name, shortAddr(it.owner)),
		fmt.Sprintf("New portfolio: %s (%s) on %s.", it.tpl.Name, it.tpl.Label(it.params), it.chain.Name))
	// Automation is idempotent to start; a failure here only delays the first
	// rebalance to the moment the user funds and presses Deploy.
	if err := s.portfolioEngine.Start(ctx, res.PortfolioID); err != nil {
		s.log.Warn("start automation failed", "portfolio", res.PortfolioID, "err", err)
	}
	p, err := s.store.GetPortfolio(ctx, res.PortfolioID)
	if err != nil {
		return nil, err
	}
	return s.portfolioView(ctx, p), nil
}

// ensureStrategy returns the portfolio engine strategy for (template, params, chain),
// creating it from a fresh, fully-gated target on first use.
func (s *Service) ensureStrategy(ctx context.Context, it *investTarget) (*store.Strategy, error) {
	st, err := s.store.GetStrategy(ctx, it.tpl.ID, it.key, it.chain.ID)
	if err == nil {
		return st, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	lockKey := "strategy:" + it.tpl.ID + "|" + it.key + "|" + strconv.FormatInt(it.chain.ID, 10)
	err = s.store.WithLock(ctx, lockKey, func(ctx context.Context) error {
		if st, err = s.store.GetStrategy(ctx, it.tpl.ID, it.key, it.chain.ID); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		t, err := s.currentTarget(ctx, it.tpl, it.params)
		if err != nil {
			return err
		}
		if err := s.executable(t, it.chain.ID); err != nil {
			return err
		}
		lines, err := strategy.AllocationFor(t.bps, it.chain.ID)
		if err != nil {
			return err
		}

		// Creating on the portfolio engine and recording it here must not be split by a
		// client disconnect, or the strategy is orphaned upstream.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		created, err := s.portfolioEngine.CreateStrategy(ctx, portfolioengine.CreateStrategyRequest{
			Name:        strategyName(it),
			Description: it.tpl.Thesis,
			IsPublic:    true, // discoverable by other portfolio engine tenants, who can mirror it
			Allocation:  toPortfolioEngine(lines),
			Schedule:    s.strategySchedule(),
			Preferences: &portfolioengine.Preferences{Swap: portfolioengine.SwapPreferences{SlippageBps: s.cfg.PortfolioEngine.SlippageBps, PriceImpactBps: s.cfg.PortfolioEngine.PriceImpactBps}},
		})
		if err != nil {
			return err
		}
		cutoff, _ := time.Parse(dateFmt, t.cutoff)
		st = &store.Strategy{
			TemplateID: it.tpl.ID, ParamsKey: it.key, Params: it.params, ChainID: it.chain.ID,
			PortfolioEngineStrategyID: created.StrategyID, Allocation: t.bps, AllocationHash: t.hash,
			Cutoff: cutoff, Version: max(created.Version, 1),
		}
		if err := s.store.InsertStrategy(ctx, st); err != nil {
			s.log.Error("portfolioEngine strategy created but not recorded", "portfolio_engine_strategy", created.StrategyID, "err", err)
			return err
		}
		s.log.Info("strategy created", "portfolio_engine_strategy", created.StrategyID, "template", it.tpl.ID, "params", it.key, "chain", it.chain.ID, "cutoff", t.cutoff)
		s.announceAll(fmt.Sprintf("New strategy: %s (%s) on %s, starting at %s.", it.tpl.Name, it.tpl.Label(it.params), it.chain.Name, allocText(t.bps)))
		if err := s.store.InsertEvaluation(ctx, &store.Evaluation{
			TemplateID: it.tpl.ID, ParamsKey: it.key, Trigger: "create", Status: "published",
			Cutoff: &cutoff, Weights: t.bps, ChangeHash: t.hash,
			Reason: "strategy created on " + it.chain.Name,
		}); err != nil {
			s.log.Warn("evaluation not recorded", "err", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// strategySchedule is the rebalance cadence new strategies get: the cron when
// one is configured (the tenant must have cron schedules enabled), otherwise
// an interval.
func (s *Service) strategySchedule() portfolioengine.Schedule {
	if s.cfg.PortfolioEngine.ScheduleCron != "" {
		return portfolioengine.Schedule{Type: "cron", Cron: s.cfg.PortfolioEngine.ScheduleCron, Timezone: s.cfg.PortfolioEngine.ScheduleTimezone}
	}
	return portfolioengine.Schedule{Type: "interval", Frequency: s.cfg.PortfolioEngine.ScheduleFrequency}
}

// strategyName is the name the portfolio engine shows: the template's own, with the
// parameters appended only when they are not the defaults.
func strategyName(it *investTarget) string {
	if it.key == it.tpl.Key(it.tpl.Defaults()) {
		return it.tpl.Name
	}
	return fmt.Sprintf("%s (%s)", it.tpl.Name, it.tpl.Label(it.params))
}

var portfolioNameDisallowed = regexp.MustCompile(`[^\p{L}\p{N} \-_.':()&/]`)

func portfolioName(tpl *strategy.Template) string {
	name := strings.TrimSpace(portfolioNameDisallowed.ReplaceAllString("Brighter - "+tpl.Name, " "))
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func toPortfolioEngine(lines []strategy.AllocationLine) portfolioengine.Allocation {
	a := portfolioengine.Allocation{Assets: make([]portfolioengine.AllocationAsset, len(lines))}
	for i, l := range lines {
		a.Assets[i] = portfolioengine.AllocationAsset{AssetID: l.AssetID, Weight: l.Weight()}
	}
	return a
}

// parseAccountID splits a chain-bound CAIP-10 "eip155:<chain>:<0x…>".
func parseAccountID(id string) (string, int64, bool) {
	parts := strings.Split(id, ":")
	if len(parts) != 3 || parts[0] != "eip155" || !evmAddress.MatchString(parts[2]) {
		return "", 0, false
	}
	chainID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return strings.ToLower(parts[2]), chainID, true
}
