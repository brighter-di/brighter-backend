package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/service"
	"github.com/brighter-di/backend/internal/strategy"
)

const studioStrategy = `{"strategy":{"id":"starter-momentum","name":"Momentum with a safety net","rebalance":"monthly","benchmark":"SPY","children":[
 {"id":"a","kind":"condition","ticker":"SPY","metric":"price","lookback":200,"comparator":"gt","compareMode":"indicator","value":0,
  "rightMetric":"moving-average","rightTicker":"SPY","rightLookback":200,
  "thenChildren":[{"id":"b","kind":"weight","method":"equal","lookback":63,"children":[
   {"id":"c","kind":"filter","direction":"top","count":3,"metric":"cumulative-return","lookback":90,"children":[
    {"id":"d","kind":"asset","ticker":"NVDAon","name":"NVIDIA"},{"id":"e","kind":"asset","ticker":"MSFT"},{"id":"f","kind":"asset","ticker":"AAPL"}]}]}],
  "elseChildren":[{"id":"g","kind":"asset","ticker":"BIL"}]}]},"range":"1y","points":20}`

func postJSON(h http.Handler, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCustomBacktestRunsStudioCode(t *testing.T) {
	s, fake := newTestServer(t)
	h := s.handler()

	rec := postJSON(h, "/v1/backtests", studioStrategy)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var v service.BacktestView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Source != "backtest" || v.TemplateID != "custom" || v.ParamsKey == "" || len(v.Series.Dates) == 0 || len(v.Series.Dates) > 20 {
		t.Fatalf("unexpected view %+v (%v)", v, err)
	}
	if fake.Calls() != 1 {
		t.Fatalf("%d upstream backtests, want 1", fake.Calls())
	}

	// New block ids and display names are the same rules: served from cache.
	same := strings.NewReplacer(`"id":"a"`, `"id":"z"`, `"name":"NVIDIA"`, `"name":"Nvidia"`).Replace(studioStrategy)
	if rec := postJSON(h, "/v1/backtests", same); rec.Code != http.StatusOK || fake.Calls() != 1 {
		t.Fatalf("same rules should hit the cache: %d, %d calls", rec.Code, fake.Calls())
	}

	if rec := postJSON(h, "/v1/backtests", strings.Replace(studioStrategy, `"ticker":"BIL"`, `"ticker":"FAKE"`, 1)); rec.Code != http.StatusUnprocessableEntity ||
		errorBody(t, rec).Code != "INVALID_STRATEGY" || !strings.Contains(errorBody(t, rec).Error, "FAKE") {
		t.Fatalf("unknown ticker: %d %s", rec.Code, rec.Body)
	}
	if rec := postJSON(h, "/v1/backtests", "{"); rec.Code != http.StatusBadRequest || errorBody(t, rec).Code != "BAD_REQUEST" {
		t.Fatalf("bad JSON: %d %s", rec.Code, rec.Body)
	}
	if fake.Calls() != 1 {
		t.Fatalf("invalid requests must not reach BacktestEngine: %d calls", fake.Calls())
	}
}

func TestCustomBacktestBacktestEngineRejection(t *testing.T) {
	s, fake := newTestServer(t)
	fake.Fail(http.StatusBadRequest)
	rec := postJSON(s.handler(), "/v1/backtests", studioStrategy)
	if rec.Code != http.StatusUnprocessableEntity || errorBody(t, rec).Code != "STRATEGY_NOT_BACKTESTABLE" {
		t.Fatalf("backtestEngine 400: %d %s", rec.Code, rec.Body)
	}
}

func TestCustomBacktestLargeSelect(t *testing.T) {
	s, fake := newTestServer(t)
	children := make([]strategy.CustomNode, 450)
	for i := range children {
		children[i] = strategy.CustomNode{Kind: "asset", Ticker: "AALon"}
	}
	body, err := json.Marshal(customBacktestBody{
		Strategy: strategy.CustomStrategy{Rebalance: "weekly", Benchmark: "SPYon", Children: []strategy.CustomNode{{
			Kind: "filter", Direction: "top", Count: 5, Metric: "cumulative-return", Lookback: 90, Children: children,
		}}}, Range: "1y", Points: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := postJSON(s.handler(), "/v1/backtests", string(body))
	if rec.Code != http.StatusOK || fake.Calls() != 1 {
		t.Fatalf("large SELECT: %d %s, %d upstream calls", rec.Code, rec.Body, fake.Calls())
	}
}

func TestCustomBacktestGlobalLimit(t *testing.T) {
	s, fake := newTestServer(t)
	s.custom = newGlobalLimiter(1, 1)
	h := s.handler()
	if rec := postJSON(h, "/v1/backtests", studioStrategy); rec.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	// Cached answers never count against the limit.
	if rec := postJSON(h, "/v1/backtests", studioStrategy); rec.Code != http.StatusOK {
		t.Fatalf("cached: %d %s", rec.Code, rec.Body)
	}
	other := strings.Replace(studioStrategy, `"count":3`, `"count":2`, 1)
	rec := postJSON(h, "/v1/backtests", other)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("retry-after") == "" || fake.Calls() != 1 {
		t.Fatalf("second miss: %d %s, %d calls", rec.Code, rec.Body, fake.Calls())
	}
}

func withoutScoreIDs(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, child := range v {
			if key != "id" {
				out[key] = withoutScoreIDs(child)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = withoutScoreIDs(child)
		}
		return out
	default:
		return value
	}
}

// The fixtures are JSON produced by Studio's real DSL parser. Exercise the
// public API, cache/service layer and backtest engine client, then inspect the exact
// wire payload. The separate source-oracle tests verify compiler semantics.
func TestEveryStudioFixtureReachesBacktestEngineIntact(t *testing.T) {
	data, err := os.ReadFile("../strategy/testdata/studio-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name     string
		Valid    bool
		Strategy json.RawMessage
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for i, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			s, fake := newTestServer(t)
			rangeName := []string{"1y", "3y", "5y", "10y"}[i%4]
			body, err := json.Marshal(map[string]any{"strategy": fixture.Strategy, "range": rangeName, "points": 120})
			if err != nil {
				t.Fatal(err)
			}
			rec := postJSON(s.handler(), "/v1/backtests", string(body))
			if !fixture.Valid {
				if rec.Code != http.StatusUnprocessableEntity || fake.Calls() != 0 {
					t.Fatalf("invalid rules reached upstream: %d %s (%d calls)", rec.Code, rec.Body, fake.Calls())
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("backtest: %d %s", rec.Code, rec.Body)
			}
			requests := fake.Requests()
			if len(requests) != 1 {
				t.Fatalf("want one upstream call, got %d", len(requests))
			}
			var payload map[string]any
			if err := json.Unmarshal(requests[0], &payload); err != nil {
				t.Fatal(err)
			}
			var input strategy.CustomStrategy
			if err := json.Unmarshal(fixture.Strategy, &input); err != nil {
				t.Fatal(err)
			}
			compiled, err := strategy.CompileCustom(input)
			if err != nil {
				t.Fatal(err)
			}
			expectedJSON, err := json.Marshal(compiled.Tree)
			if err != nil {
				t.Fatal(err)
			}
			var expected any
			if err := json.Unmarshal(expectedJSON, &expected); err != nil {
				t.Fatal(err)
			}
			strategyTree := payload["strategyTree"].(map[string]any)
			if !reflect.DeepEqual(withoutScoreIDs(strategyTree["raw_value"]), withoutScoreIDs(expected)) {
				t.Fatal("BacktestEngine wire score differs from compiled Studio rules")
			}
			if strategyTree["encoding_type"] != "none" || strategyTree["encoded_value"] != "" {
				t.Fatal("incorrect inline strategyTree envelope")
			}
			if !reflect.DeepEqual(payload["benchmark_tickers"], []any{compiled.Benchmark}) {
				t.Fatalf("lost benchmark: %v", payload["benchmark_tickers"])
			}
			for key, want := range map[string]any{"capital": float64(100000), "apply_reg_fee": true, "apply_taf_fee": true, "apply_cat_fee": true, "apply_subscription": "none", "slippage_percent": .0005} {
				if payload[key] != want {
					t.Fatalf("%s: got %v, want %v", key, payload[key], want)
				}
			}
			end, err := time.Parse("2006-01-02", payload["end_date"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if payload["start_date"] != end.AddDate(-service.Ranges[rangeName], 0, 0).Format("2006-01-02") {
				t.Fatalf("lost range %s: %v to %v", rangeName, payload["start_date"], payload["end_date"])
			}
		})
	}
}

func TestCustomCacheSeparatesRuleChangesAndRanges(t *testing.T) {
	base := `{"name":"Cache audit","rebalance":"monthly","benchmark":"SPY","children":[{"kind":"condition","metric":"rsi","ticker":"SPY","lookback":14,"comparator":"lt","compareMode":"indicator","rightMetric":"rsi","rightTicker":"QQQ","rightLookback":20,"thenChildren":[{"kind":"filter","direction":"top","count":1,"metric":"cumulative-return","lookback":126,"children":[{"kind":"asset","ticker":"QCOM"},{"kind":"asset","ticker":"SPY"},{"kind":"asset","ticker":"BIL"}]}],"elseChildren":[{"kind":"weight","method":"specified","children":[{"kind":"asset","ticker":"SPY","percent":60},{"kind":"asset","ticker":"BIL","percent":40}]}]}]}`
	changes := [][2]string{
		{`"monthly"`, `"weekly"`}, {`"benchmark":"SPY"`, `"benchmark":"QQQ"`},
		{`"lookback":14`, `"lookback":30`}, {`"comparator":"lt"`, `"comparator":"gte"`},
		{`"rightTicker":"QQQ"`, `"rightTicker":"BIL"`}, {`"rightLookback":20`, `"rightLookback":63`},
		{`"count":1`, `"count":2`}, {`"direction":"top"`, `"direction":"bottom"`},
		{`"metric":"cumulative-return"`, `"metric":"standard-deviation-return"`}, {`"lookback":126`, `"lookback":63`},
		{`"ticker":"QCOM"`, `"ticker":"QQQ"`}, {`"percent":60`, `"percent":70`},
	}
	for _, change := range changes {
		t.Run(change[0]+" to "+change[1], func(t *testing.T) {
			s, fake := newTestServer(t)
			h := s.handler()
			run := func(rules, rng string) string {
				rec := postJSON(h, "/v1/backtests", `{"strategy":`+rules+`,"range":"`+rng+`"}`)
				if rec.Code != http.StatusOK {
					t.Fatalf("backtest: %d %s", rec.Code, rec.Body)
				}
				var view service.BacktestView
				if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
					t.Fatal(err)
				}
				return view.ParamsKey
			}
			old := run(base, "5y")
			changed := strings.Replace(base, change[0], change[1], 1)
			if change[0] == `"percent":60` {
				changed = strings.Replace(changed, `"percent":40`, `"percent":30`, 1)
			}
			if next := run(changed, "5y"); next == old || fake.Calls() != 2 {
				t.Fatalf("rule change reused old backtest: %s (%d calls)", next, fake.Calls())
			}
			run(changed, "5y")
			if fake.Calls() != 2 {
				t.Fatal("identical rules missed the cache")
			}
			run(changed, "1y")
			if fake.Calls() != 3 {
				t.Fatal("range change reused old backtest")
			}
		})
	}
}

func TestCustomBacktestRejectsTrailingContent(t *testing.T) {
	s, fake := newTestServer(t)
	h := s.handler()
	for _, suffix := range []string{studioStrategy, "garbage", "null"} {
		rec := postJSON(h, "/v1/backtests", studioStrategy+suffix)
		if rec.Code != http.StatusBadRequest || fake.Calls() != 0 {
			t.Fatalf("trailing data was ignored: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := postJSON(h, "/v1/backtests", studioStrategy+" \n\t"); rec.Code != http.StatusOK {
		t.Fatalf("valid JSON whitespace rejected: %d %s", rec.Code, rec.Body)
	}
}
