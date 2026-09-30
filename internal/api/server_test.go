package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/backtestengine"
	"github.com/brighter-di/backend/internal/config"
	"github.com/brighter-di/backend/internal/portfolioengine"
	"github.com/brighter-di/backend/internal/prices"
	"github.com/brighter-di/backend/internal/service"
	"github.com/brighter-di/backend/internal/strategy"
	"github.com/brighter-di/backend/internal/testkit"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// newTestServer serves research from a fake the backtest engine; trading is not
// configured and there is no database.
func newTestServer(t *testing.T) (*Server, *testkit.BacktestEngine) {
	t.Helper()
	fake := testkit.NewBacktestEngine(t)
	fake.Set(map[string]float64{"AAPL": 0.5, "MSFT": 0.5}, time.Now().UTC().AddDate(0, 0, -1))
	cfg := &config.Config{
		EnabledChains: []int64{56}, CORSOrigins: []string{"https://brighter.fi"}, AdminToken: "admin-token",
		MaxCutoffAgeTradingDays: 3, MaxDataLagDays: 4, PortfolioEngine: config.PortfolioEngine{ScheduleFrequency: "hourly"},
	}
	cc := backtestengine.New(backtestengine.Options{BaseURL: fake.URL, KeyID: "id", Secret: "secret", Logger: discard})
	svc := service.New(cfg, nil, cc, portfolioengine.New(portfolioengine.Options{Logger: discard}), prices.New(prices.Options{Logger: discard}), discard)
	return newServer(cfg, svc, nil, nil, discard), fake
}

func send(h http.Handler, method, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) apiError {
	t.Helper()
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error body: %s", rec.Body)
	}
	return e
}

func TestResearchLimitCountsOnlyCacheMisses(t *testing.T) {
	s, fake := newTestServer(t)
	s.research = newIPLimiter(0.0001, 2)
	h := s.handler()
	get := func(query string) *httptest.ResponseRecorder {
		return send(h, http.MethodGet, "/v1/templates/mag7-rank/backtest?range=1y"+query, nil)
	}

	for i := range 5 {
		if rec := get(""); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := get("&count=2"); rec.Code != http.StatusOK {
		t.Fatalf("second miss: %d", rec.Code)
	}
	rec := get("&count=4")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("retry-after") == "" || errorBody(t, rec).Code != "RATE_LIMITED" {
		t.Fatalf("third miss: %d %s", rec.Code, rec.Body)
	}
	if rec := get(""); rec.Code != http.StatusOK {
		t.Fatalf("a cached answer was refused: %d", rec.Code)
	}
	if n := fake.Calls(); n != 2 {
		t.Fatalf("%d upstream backtests, want 2", n)
	}
}

func TestBacktestRoute(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.handler()
	rec := send(h, http.MethodGet, "/v1/templates/mag7-rank/backtest?range=1y&points=20", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("cache-control"), "public") {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var v service.BacktestView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Source != "backtest" || len(v.Series.Dates) > 20 {
		t.Fatalf("body %s", rec.Body)
	}

	// since trims the same daily series; metrics still cover the whole range.
	var full service.BacktestView
	if err := json.Unmarshal(send(h, http.MethodGet, "/v1/templates/mag7-rank/backtest?range=1y", nil).Body.Bytes(), &full); err != nil || len(full.Series.Dates) < 3 {
		t.Fatalf("full: %v %d", err, len(full.Series.Dates))
	}
	from := full.Series.Dates[len(full.Series.Dates)-2]
	rec = send(h, http.MethodGet, "/v1/templates/mag7-rank/backtest?range=1y&points=20&since="+from, nil)
	var tail service.BacktestView
	if err := json.Unmarshal(rec.Body.Bytes(), &tail); err != nil || len(tail.Series.Dates) != 2 || tail.Series.Dates[0] != from ||
		tail.StartDate != full.StartDate || tail.Metrics != full.Metrics {
		t.Fatalf("since: %d %s", rec.Code, rec.Body)
	}
	if rec := send(h, http.MethodGet, "/v1/templates/mag7-rank/backtest?since=yesterday", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad since: %d %s", rec.Code, rec.Body)
	}
	if rec := send(h, http.MethodGet, "/v1/templates/mag7-rank/rules", nil); rec.Code != http.StatusNotFound || errorBody(t, rec).Code != "NO_STUDIO_RULES" {
		t.Fatalf("built-in rules: %d %s", rec.Code, rec.Body)
	}
	if rec := send(h, http.MethodGet, "/v1/templates/nope/backtest", nil); rec.Code != http.StatusNotFound || errorBody(t, rec).Code != "UNKNOWN_TEMPLATE" {
		t.Fatalf("unknown template: %d %s", rec.Code, rec.Body)
	}
}

func TestSummariesAnswerEveryListedStrategyInOneRequest(t *testing.T) {
	s, _ := newTestServer(t)
	s.research = newIPLimiter(1000, 1000)
	h := s.handler()
	rec := send(h, http.MethodGet, "/v1/templates/summaries?range=1y&points=20&targets=1", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("cache-control"), "public") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var body struct {
		Summaries []service.SummaryView `json:"summaries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, tpl := range strategy.Templates {
		if !tpl.Retired {
			listed = append(listed, tpl.ID)
		}
	}
	if len(body.Summaries) != len(listed) {
		t.Fatalf("%d summaries, want %d", len(body.Summaries), len(listed))
	}
	for i, sm := range body.Summaries {
		if sm.ID != listed[i] || sm.Backtest == nil || len(sm.Backtest.Series.Dates) > 21 {
			t.Fatalf("summary %d: %+v", i, sm)
		}
	}
	if body.Summaries[0].Target == nil || len(body.Summaries[0].Target.Lines) == 0 {
		t.Fatalf("no target for %s", body.Summaries[0].ID)
	}
	if rec := send(h, http.MethodGet, "/v1/templates/summaries?range=2y", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad range: %d", rec.Code)
	}
}

func TestResponsesAreGzippedOnlyWhenAccepted(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.handler()
	rec := send(h, http.MethodGet, "/v1/templates", map[string]string{"accept-encoding": "gzip, br"})
	if rec.Header().Get("content-encoding") != "gzip" {
		t.Fatalf("not compressed: %v", rec.Header())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Templates []json.RawMessage `json:"templates"`
	}
	if err := json.NewDecoder(zr).Decode(&body); err != nil || len(body.Templates) == 0 {
		t.Fatalf("compressed body: %v %d", err, len(body.Templates))
	}
	if plain := send(h, http.MethodGet, "/v1/templates", nil); plain.Header().Get("content-encoding") != "" || !json.Valid(plain.Body.Bytes()) {
		t.Fatalf("plain request: %v", plain.Header())
	}
	preflight := send(h, http.MethodOptions, "/v1/config", map[string]string{"origin": "https://brighter.fi", "access-control-request-method": "GET", "accept-encoding": "gzip"})
	if preflight.Code != http.StatusNoContent || preflight.Body.Len() != 0 || preflight.Header().Get("content-encoding") != "" {
		t.Fatalf("preflight: %d %v %q", preflight.Code, preflight.Header(), preflight.Body)
	}
}

func TestFailMapsErrorsWithoutNamingVendors(t *testing.T) {
	s, _ := newTestServer(t)
	cases := []struct {
		name       string
		err        error
		status     int
		code       string
		msg        string
		retryAfter string
	}{
		{"service", &service.Error{Status: 404, Code: "UNKNOWN_ASSET", Message: "Unknown asset"}, 404, "UNKNOWN_ASSET", "Unknown asset", ""},
		{"gate", &strategy.GateError{Reason: "Market data is stale"}, 409, "TARGET_NOT_EXECUTABLE", "Market data is stale", ""},
		{"rate limited", &rateLimited{wait: 2 * time.Second}, 429, "RATE_LIMITED", "slow down", "2"},
		{"trading cooldown", &portfolioengine.Error{Status: 429, Code: "COOLDOWN", Message: "Rebalance cooldown", RetryAfter: 30 * time.Second}, 429, "COOLDOWN", "Rebalance cooldown", "30"},
		{"trading rejects our key", &portfolioengine.Error{Status: 401, Message: "bad key"}, 502, "TRADING_ERROR", "bad key", ""},
		{"trading rejects the request", &portfolioengine.Error{Status: 400, Code: "API_221", Message: "Unsupported settlement asset", Details: []string{"use USDT"}}, 400, "API_221", "Unsupported settlement asset: use USDT", ""},
		{"trading off", portfolioengine.ErrNotConfigured, 503, "TRADING_UNAVAILABLE", "not configured", ""},
		{"prices", &prices.Error{Status: 500, Detail: "nasdaq is down"}, 502, "PRICES_ERROR", "temporarily unavailable", ""},
		{"research detail stays in the log", &backtestengine.Error{Status: 500, Detail: "internal stack trace"}, 502, "RESEARCH_ERROR", "temporarily unavailable", ""},
		{"research timeout", &backtestengine.Error{Status: 504}, 504, "RESEARCH_ERROR", "", ""},
		{"research off", backtestengine.ErrNotConfigured, 503, "RESEARCH_UNAVAILABLE", "not configured", ""},
		{"unexpected", errors.New("boom"), 500, "INTERNAL", "Internal error.", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.fail(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil), c.err)
			e := errorBody(t, rec)
			if rec.Code != c.status || e.Code != c.code || !strings.Contains(e.Error, c.msg) || rec.Header().Get("retry-after") != c.retryAfter {
				t.Fatalf("%d %+v retry-after %q", rec.Code, e, rec.Header().Get("retry-after"))
			}
			body := strings.ToLower(rec.Body.String())
			for _, word := range []string{"backtestEngine", "portfolioEngine", "nasdaq", "stack trace"} {
				if strings.Contains(body, word) {
					t.Fatalf("response names %q: %s", word, body)
				}
			}
		})
	}

	rec := httptest.NewRecorder()
	s.fail(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil), context.Canceled)
	if rec.Body.Len() != 0 {
		t.Fatalf("wrote to a client that left: %s", rec.Body)
	}
}

func TestCORSAllowsOnlyListedOrigins(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.handler()
	preflight := func(origin string) *httptest.ResponseRecorder {
		return send(h, http.MethodOptions, "/v1/config", map[string]string{"origin": origin, "access-control-request-method": "GET"})
	}
	if rec := preflight("https://brighter.fi"); rec.Code != http.StatusNoContent || rec.Header().Get("access-control-allow-origin") != "https://brighter.fi" {
		t.Fatalf("allowed origin: %d %v", rec.Code, rec.Header())
	}
	if rec := preflight("https://evil.example"); rec.Code != http.StatusForbidden || rec.Header().Get("access-control-allow-origin") != "" {
		t.Fatalf("other origin: %d %v", rec.Code, rec.Header())
	}
	if rec := send(h, http.MethodGet, "/v1/config", map[string]string{"origin": "https://evil.example"}); rec.Code != http.StatusOK || rec.Header().Get("access-control-allow-origin") != "" {
		t.Fatalf("simple request from another origin: %d %v", rec.Code, rec.Header())
	}
}

func TestCORSOpensOnlyMCPToEveryOrigin(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.handler()
	header := map[string]string{"origin": "https://inspector.example", "access-control-request-method": "POST", "access-control-request-headers": "content-type, mcp-protocol-version"}
	rec := send(h, http.MethodOptions, "/mcp", header)
	if rec.Code != http.StatusNoContent || rec.Header().Get("access-control-allow-origin") != "*" ||
		!strings.Contains(rec.Header().Get("access-control-allow-headers"), "mcp-protocol-version") {
		t.Fatalf("mcp preflight: %d %v", rec.Code, rec.Header())
	}
	if rec.Header().Get("access-control-allow-credentials") != "" {
		t.Fatal("mcp must not allow credentials")
	}
	if rec := send(h, http.MethodOptions, "/v1/config", header); rec.Code != http.StatusForbidden {
		t.Fatalf("the rest of the API stays allowlisted: %d", rec.Code)
	}
}

func TestAdminRoutesNeedTheToken(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.handler()
	for _, header := range []map[string]string{nil, {"authorization": "Bearer wrong"}} {
		if rec := send(h, http.MethodGet, "/v1/admin/evaluations", header); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%v: %d", header, rec.Code)
		}
	}
}

func TestUnknownRoute(t *testing.T) {
	s, _ := newTestServer(t)
	if rec := send(s.handler(), http.MethodGet, "/v2/nothing", nil); rec.Code != http.StatusNotFound || errorBody(t, rec).Code != "NOT_FOUND" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func catalogue(t *testing.T, h http.Handler, query string) service.CataloguePage {
	t.Helper()
	rec := send(h, http.MethodGet, "/v1/strategies"+query, nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("cache-control"), "public") {
		t.Fatalf("%s: %d %s", query, rec.Code, rec.Body)
	}
	var page service.CataloguePage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func ids(page service.CataloguePage) []string {
	out := make([]string, 0, len(page.Strategies))
	for _, c := range page.Strategies {
		out = append(out, c.ID)
	}
	return out
}

func TestStrategyListFiltersSortsAndPages(t *testing.T) {
	s, _ := newTestServer(t)
	s.research = newIPLimiter(1000, 1000)
	h := s.handler()

	listed := 0
	for _, tpl := range strategy.Templates {
		if !tpl.Retired {
			listed++
		}
	}

	all := catalogue(t, h, "?limit=50")
	if all.Total != listed || len(all.Strategies) != listed {
		t.Fatalf("%d of %d strategies, want %d", len(all.Strategies), all.Total, listed)
	}
	first := all.Strategies[0]
	if first.Name == "" || first.Cadence == "" || len(first.Assets) == 0 || first.Backtest == nil {
		t.Fatalf("thin card: %+v", first)
	}
	if first.Assets[0].Icon == "" || first.Assets[0].Ticker == "" {
		t.Fatalf("a logo needs its icon and ticker: %+v", first.Assets[0])
	}
	if first.Backtest.BenchmarkTicker == "" || len(first.Backtest.Series.Dates) == 0 {
		t.Fatalf("no benchmark or series: %+v", first.Backtest)
	}
	if first.Target == nil || len(first.Target.Lines) == 0 {
		t.Fatalf("no current allocation for %s", first.ID)
	}

	// Every listed preset is unleveraged today, so the default and the toggle
	// agree — but the toggle must never hide anything.
	if geared := catalogue(t, h, "?leveraged=1&limit=50"); geared.Total < all.Total {
		t.Fatalf("leveraged=1 hid %d strategies", all.Total-geared.Total)
	}
	for _, c := range all.Strategies {
		if c.Leveraged {
			t.Fatalf("%s is leveraged and was listed by default", c.ID)
		}
	}

	daily := catalogue(t, h, "?cadence=daily&limit=50")
	if daily.Total == 0 || daily.Total >= all.Total {
		t.Fatalf("cadence=daily returned %d of %d", daily.Total, all.Total)
	}
	for _, c := range daily.Strategies {
		if c.Cadence != "daily" {
			t.Fatalf("%s is %s", c.ID, c.Cadence)
		}
	}

	// "Holds any of": the union is at least as large as either ticker alone.
	nvda := catalogue(t, h, "?assets=NVDA&limit=50")
	tlt := catalogue(t, h, "?assets=TLT&limit=50")
	union := catalogue(t, h, "?assets=NVDA,TLT&limit=50")
	if nvda.Total == 0 || union.Total < nvda.Total || union.Total < tlt.Total || union.Total > nvda.Total+tlt.Total {
		t.Fatalf("any-of is wrong: NVDA %d, TLT %d, union %d", nvda.Total, tlt.Total, union.Total)
	}

	if beats := catalogue(t, h, "?beatsBenchmark=1&limit=50"); beats.Total > all.Total {
		t.Fatalf("beatsBenchmark widened the list: %d of %d", beats.Total, all.Total)
	} else {
		for _, c := range beats.Strategies {
			if c.Backtest == nil || !c.Backtest.BeatsBenchmark || c.Backtest.TotalReturn <= c.Backtest.BenchmarkReturn {
				t.Fatalf("%s does not beat its benchmark: %+v", c.ID, c.Backtest)
			}
		}
	}

	// Paging partitions the list: no repeats, no gaps, and a total that holds
	// still while the reader scrolls.
	seen := map[string]bool{}
	for offset := 0; offset < all.Total; offset += 2 {
		page := catalogue(t, h, fmt.Sprintf("?limit=2&offset=%d", offset))
		if page.Total != all.Total {
			t.Fatalf("total moved from %d to %d at offset %d", all.Total, page.Total, offset)
		}
		for _, id := range ids(page) {
			if seen[id] {
				t.Fatalf("%s appeared on two pages", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != all.Total {
		t.Fatalf("paged over %d strategies, want %d", len(seen), all.Total)
	}

	// Newest reverses the catalogue; the fake prices every strategy alike, so
	// return order is only meaningful as "not the newest order".
	newest := catalogue(t, h, "?sort=newest&limit=50")
	if newest.Total != all.Total {
		t.Fatalf("sort changed the total: %d", newest.Total)
	}
	var lastListed string
	for _, tpl := range strategy.Templates {
		if !tpl.Retired {
			lastListed = tpl.ID
		}
	}
	if got := ids(newest)[0]; got != lastListed {
		t.Fatalf("newest starts at %s, want %s", got, lastListed)
	}
}

func TestStrategyListRejectsFiltersItCannotHonour(t *testing.T) {
	s, _ := newTestServer(t)
	s.research = newIPLimiter(1000, 1000)
	h := s.handler()
	for query, code := range map[string]string{
		// 10y is not pre-warmed: honouring it would fan a cold backtest engine call
		// out across the whole catalogue on one page view.
		"?period=10y":           "BAD_PERIOD",
		"?period=nonsense":      "BAD_PERIOD",
		"?cadence=fortnightly":  "BAD_CADENCE",
		"?cadence=daily,hourly": "BAD_CADENCE",
		"?assets=NOTATICKER":    "BAD_ASSET",
		"?sort=cheapest":        "BAD_SORT",
	} {
		rec := send(h, http.MethodGet, "/v1/strategies"+query, nil)
		if rec.Code != http.StatusBadRequest || errorBody(t, rec).Code != code {
			t.Fatalf("%s: %d %s, want 400 %s", query, rec.Code, rec.Body, code)
		}
	}
	// A trailing comma from a hand-edited URL is not an unknown ticker.
	if rec := send(h, http.MethodGet, "/v1/strategies?assets=NVDA,&cadence=", nil); rec.Code != http.StatusOK {
		t.Fatalf("trailing comma: %d %s", rec.Code, rec.Body)
	}
}

func TestNewestReadsOnlyThePageItReturns(t *testing.T) {
	s, fake := newTestServer(t)
	s.research = newIPLimiter(1000, 1000)
	h := s.handler()

	before := fake.Calls()
	page := catalogue(t, h, "?sort=newest&limit=2")
	newestCalls := fake.Calls() - before
	if len(page.Strategies) != 2 {
		t.Fatalf("%d strategies", len(page.Strategies))
	}

	// Sorting by return has to read every match before it can order them, so
	// it costs the whole catalogue even for one page. Newest does not.
	s2, fake2 := newTestServer(t)
	s2.research = newIPLimiter(1000, 1000)
	before2 := fake2.Calls()
	catalogue(t, s2.handler(), "?sort=return&limit=2")
	returnCalls := fake2.Calls() - before2

	if newestCalls >= returnCalls {
		t.Fatalf("newest read %d backtests, return read %d — the cheap path is not cheap", newestCalls, returnCalls)
	}
}

func TestStrategyFacetsOfferWhatTheFilterRowNeeds(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.handler()
	rec := send(h, http.MethodGet, "/v1/strategies/facets", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("cache-control"), "public") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var facets service.CatalogueFacets
	if err := json.Unmarshal(rec.Body.Bytes(), &facets); err != nil {
		t.Fatal(err)
	}
	if len(facets.Assets) == 0 || len(facets.Cadences) == 0 || facets.Total == 0 {
		t.Fatalf("empty facets: %+v", facets)
	}
	kinds := map[string]bool{}
	for _, a := range facets.Assets {
		if a.Ticker == "" || a.Kind == "" || a.Icon == "" || a.Count < 1 {
			t.Fatalf("a picker row needs all of it: %+v", a)
		}
		kinds[a.Kind] = true
	}
	// The picker groups by kind, so more than one must actually appear.
	if len(kinds) < 2 {
		t.Fatalf("one kind only: %v", kinds)
	}
	counted := 0
	for _, c := range facets.Cadences {
		counted += c.Count
	}
	if counted != facets.Total {
		t.Fatalf("cadences count %d of %d strategies", counted, facets.Total)
	}
	// Every listed preset is unleveraged today; the count must say so rather
	// than guess.
	if facets.Leveraged != 0 {
		t.Fatalf("leveraged=%d, want 0 until one is published", facets.Leveraged)
	}
}
