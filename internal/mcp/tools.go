package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/brighter-di/backend/internal/service"
	"github.com/brighter-di/backend/internal/strategy"
)

// Catalogue is everything the tools need, which is only what the website
// already serves anonymously. Naming it here rather than taking *service.Service
// keeps the tools testable and makes the blast radius obvious: there is no
// method on this interface that writes anything.
type Catalogue interface {
	Catalogue(ctx context.Context, q service.CatalogueQuery) (*service.CataloguePage, error)
	Template(ctx context.Context, id string) (*strategy.Template, error)
	Backtest(ctx context.Context, tpl *strategy.Template, params strategy.Params, rng string) (*service.BacktestView, error)
	BacktestCustom(ctx context.Context, c strategy.CustomStrategy, rng string) (*service.BacktestView, error)
	Target(ctx context.Context, tpl *strategy.Template, params strategy.Params) (*service.TargetView, error)
	Assets() []service.AssetView
	AssetCloses(ctx context.Context, ticker string, year int) (*service.ClosesView, error)
}

// Tool is one callable, in MCP's shape. run is unexported so it never reaches
// the wire.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`

	// upstream marks a tool that can reach BacktestEngine. The rest are local
	// computation and are budgeted far more loosely.
	upstream bool
	run      func(context.Context, json.RawMessage) (any, error)
}

/* ── Schema helpers ─────────────────────────────────────────────────── */

func object(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func enum(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

func integer(description string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": description, "minimum": min, "maximum": max}
}

func boolean(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func list(description string, items map[string]any, max int) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": items, "maxItems": max}
}

/* ── Calling ────────────────────────────────────────────────────────── */

func (s *Server) callTool(r *http.Request, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call needs a name and arguments."}
	}
	var tool *Tool
	for i := range s.tools {
		if s.tools[i].Name == p.Name {
			tool = &s.tools[i]
			break
		}
	}
	if tool == nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "Unknown tool " + p.Name + "."}
	}
	if s.admit != nil {
		if err := s.admit(r, tool.upstream); err != nil {
			// A budget a model has run through is something it should read and
			// wait on, not a transport failure it cannot interpret. The spec
			// is explicit that an execution problem belongs in the result.
			return toolResult("Rate limited. "+err.Error(), true), nil
		}
	}
	out, err := tool.run(r.Context(), p.Arguments)
	if err != nil {
		// A tool that refuses is a result the model should read and act on,
		// not a transport failure. Only a broken envelope is an rpcError.
		s.log.Debug("tool refused", "tool", p.Name, "err", err)
		return toolResult(err.Error(), true), nil
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, &rpcError{Code: codeInternal, Message: "That answer could not be encoded."}
	}
	return toolResult(clean(string(body)), false), nil
}

// maxResult is what one answer may carry. A model pays for every byte, and
// nothing here needs more; the tools are shaped to stay well inside it.
const maxResult = 8 << 10

func toolResult(text string, failed bool) any {
	if len(text) > maxResult {
		text = text[:maxResult] + "\n…truncated. Narrow the request."
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": failed,
	}
}

// Control, zero-width and bidi characters can hide or reorder text in a
// transcript. Nothing we answer with needs them.
var unsafeText = regexp.MustCompile(`[\x00-\x08\x0B-\x1F\x7F-\x9F\x{200B}\x{200C}\x{200E}\x{200F}\x{202A}-\x{202E}\x{2060}-\x{2069}\x{FEFF}]`)

func clean(s string) string { return unsafeText.ReplaceAllString(s, "") }

func args[T any](raw json.RawMessage) (T, error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return out, fmt.Errorf("those arguments do not match the tool's schema: %s", err)
	}
	return out, nil
}

/* ── Shaping ────────────────────────────────────────────────────────── */

// compactMetrics is what a backtest is worth to a model: the figures, never
// the curve. A hundred dated points would tell it nothing it can use and cost
// it most of its context.
type compactMetrics struct {
	Range       string  `json:"range"`
	StartDate   string  `json:"startDate"`
	EndDate     string  `json:"endDate"`
	TotalReturn float64 `json:"totalReturn"`
	CAGR        float64 `json:"cagr"`
	MaxDrawdown float64 `json:"maxDrawdown"`
	Volatility  float64 `json:"volatility"`
	Sharpe      float64 `json:"sharpe"`
	Sortino     float64 `json:"sortino"`
	Benchmark   *struct {
		Ticker      string  `json:"ticker"`
		TotalReturn float64 `json:"totalReturn"`
	} `json:"benchmark,omitempty"`
	Note string `json:"note"`
}

func shapeBacktest(v *service.BacktestView) compactMetrics {
	out := compactMetrics{
		Range: v.Range, StartDate: v.StartDate, EndDate: v.EndDate,
		TotalReturn: v.Metrics.TotalReturn, CAGR: v.Metrics.CAGR, MaxDrawdown: v.Metrics.MaxDrawdown,
		Volatility: v.Metrics.Volatility, Sharpe: v.Metrics.Sharpe, Sortino: v.Metrics.Sortino,
		Note: "Returns and drawdown are fractions, not percentages. A backtest is history, not a forecast.",
	}
	if m := v.Benchmark.Metrics; m != nil {
		out.Benchmark = &struct {
			Ticker      string  `json:"ticker"`
			TotalReturn float64 `json:"totalReturn"`
		}{Ticker: v.Benchmark.Ticker, TotalReturn: m.TotalReturn}
	}
	return out
}

// coerce runs caller-supplied numbers through the template's own grid, so a
// value out of range is snapped exactly as it is for the website rather than
// reaching BacktestEngine.
func coerce(tpl *strategy.Template, given map[string]float64) strategy.Params {
	return tpl.CoerceStrings(func(key string) string {
		if v, ok := given[key]; ok {
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	})
}

func paramGrid(tpl *strategy.Template) []map[string]any {
	out := make([]map[string]any, 0, len(tpl.Params))
	for _, p := range tpl.Params {
		out = append(out, map[string]any{
			"key": p.Key, "label": p.Label, "min": p.Min, "max": p.Max,
			"step": p.Step, "default": p.Default, "unit": p.Unit, "hint": p.Hint,
		})
	}
	return out
}

/* ── Catalogue tools ────────────────────────────────────────────────── */

func catalogueTools(cat Catalogue) []Tool {
	rangeEnum := enum("Backtest window. Only these are kept warm; anything else is refused.", "1y", "3y", "5y")

	return []Tool{{
		Name: "list_strategies", upstream: true,
		Description: "List the strategies published on brighter.fi, newest or best-performing first. " +
			"Each carries its rebalance cadence, the assets it can hold, and its return over the chosen period " +
			"against its benchmark. Strategies that can hold leveraged or inverse funds are excluded unless asked for.",
		InputSchema: object(map[string]any{
			"period":         enum("Window the returns cover.", "1y", "3y", "5y"),
			"assets":         list("Only strategies that can hold any of these tickers.", map[string]any{"type": "string"}, 30),
			"cadence":        list("Only these rebalance cadences.", map[string]any{"type": "string", "enum": []string{"daily", "weekly", "monthly"}}, 3),
			"beatsBenchmark": boolean("Only strategies ahead of their benchmark over the period."),
			"leveraged":      boolean("Include strategies that can hold leveraged or inverse funds. Off by default."),
			"sort":           enum("Order of the list.", "return", "newest"),
			"limit":          integer("How many to return.", 1, 24),
			"offset":         integer("Where to start, for paging.", 0, 10000),
		}),
		run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Period         string   `json:"period"`
				Assets         []string `json:"assets"`
				Cadence        []string `json:"cadence"`
				BeatsBenchmark bool     `json:"beatsBenchmark"`
				Leveraged      bool     `json:"leveraged"`
				Sort           string   `json:"sort"`
				Limit          int      `json:"limit"`
				Offset         int      `json:"offset"`
			}](raw)
			if err != nil {
				return nil, err
			}
			q := service.CatalogueQuery{
				Period: or(a.Period, "1y"), Assets: upper(a.Assets), Cadences: lower(a.Cadence),
				BeatsBenchmark: a.BeatsBenchmark, Leveraged: a.Leveraged,
				Sort: or(a.Sort, service.SortReturn), Offset: a.Offset, Limit: clamp(a.Limit, 1, 24, 8), Points: 20,
			}
			page, err := cat.Catalogue(ctx, q)
			if err != nil {
				return nil, err
			}
			rows := make([]map[string]any, 0, len(page.Strategies))
			for _, c := range page.Strategies {
				row := map[string]any{"id": c.ID, "name": c.Name, "thesis": c.Thesis, "cadence": c.Cadence, "author": c.Author}
				holds := make([]string, 0, len(c.Assets))
				for _, asset := range c.Assets {
					holds = append(holds, asset.Ticker)
				}
				row["canHold"] = holds
				if b := c.Backtest; b != nil {
					row["totalReturn"] = b.TotalReturn
					row["benchmarkReturn"] = b.BenchmarkReturn
					row["beatsBenchmark"] = b.BeatsBenchmark
				}
				rows = append(rows, row)
			}
			return map[string]any{"total": page.Total, "offset": page.Offset, "period": q.Period, "strategies": rows}, nil
		},
	}, {
		Name: "get_strategy",
		Description: "One strategy in full: what its rule does, the parameters you can tune with their ranges " +
			"and defaults, every asset it can hold, and its benchmark. Use the parameter keys with backtest_strategy.",
		InputSchema: object(map[string]any{"id": str("Strategy id, as list_strategies returns it.")}, "id"),
		run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				ID string `json:"id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			tpl, err := cat.Template(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"id": tpl.ID, "name": tpl.Name, "thesis": tpl.Thesis, "cadence": tpl.Cadence,
				"benchmark": tpl.Benchmark, "canHold": tpl.Holdable(), "fallback": tpl.Fallback,
				"author": tpl.Author, "publishedAt": tpl.PublishedAt, "retired": tpl.Retired,
				"parameters": paramGrid(tpl), "defaults": tpl.Defaults(),
			}, nil
		},
	}, {
		Name: "backtest_strategy", upstream: true,
		Description: "Run a published strategy over real market history with parameters of your choosing. " +
			"Returns its figures against its benchmark. Values outside a parameter's range are snapped into it. " +
			"This is a real backtest, so make each one count.",
		InputSchema: object(map[string]any{
			"id":     str("Strategy id."),
			"params": map[string]any{"type": "object", "description": "Parameter values by key, from get_strategy. Omitted keys use their default.", "additionalProperties": map[string]any{"type": "number"}},
			"range":  rangeEnum,
		}, "id"),
		run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				ID     string             `json:"id"`
				Params map[string]float64 `json:"params"`
				Range  string             `json:"range"`
			}](raw)
			if err != nil {
				return nil, err
			}
			tpl, err := cat.Template(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			params := coerce(tpl, a.Params)
			v, err := cat.Backtest(ctx, tpl, params, or(a.Range, "5y"))
			if err != nil {
				return nil, err
			}
			out := shapeBacktest(v)
			return map[string]any{"strategy": tpl.Name, "id": tpl.ID, "params": params, "metrics": out}, nil
		},
	}, {
		Name: "get_allocation", upstream: true,
		Description: "What a published strategy's rule holds right now, in basis points of the portfolio. " +
			"This is the live target, computed from the latest close.",
		InputSchema: object(map[string]any{
			"id":     str("Strategy id."),
			"params": map[string]any{"type": "object", "description": "Parameter values by key. Omitted keys use their default.", "additionalProperties": map[string]any{"type": "number"}},
		}, "id"),
		run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				ID     string             `json:"id"`
				Params map[string]float64 `json:"params"`
			}](raw)
			if err != nil {
				return nil, err
			}
			tpl, err := cat.Template(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			v, err := cat.Target(ctx, tpl, coerce(tpl, a.Params))
			if err != nil {
				return nil, err
			}
			holds := make([]map[string]any, 0, len(v.Lines))
			for _, l := range v.Lines {
				holds = append(holds, map[string]any{"ticker": l.Ticker, "bps": l.Bps})
			}
			return map[string]any{"id": tpl.ID, "asOf": v.Cutoff, "fresh": v.Fresh, "holdings": holds}, nil
		},
	}, {
		Name: "find_assets",
		Description: "Search the assets a Brighter strategy can hold — tokenized US stocks, ETFs and T-bills. " +
			"A rule may only name a ticker that appears here.",
		InputSchema: object(map[string]any{
			"query":   str("Match against ticker or company name."),
			"tickers": list("Look up these exact tickers.", map[string]any{"type": "string"}, 30),
			"limit":   integer("How many to return.", 1, 30),
			"offset":  integer("Where to start.", 0, 1000),
		}),
		run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Query   string   `json:"query"`
				Tickers []string `json:"tickers"`
				Limit   int      `json:"limit"`
				Offset  int      `json:"offset"`
			}](raw)
			if err != nil {
				return nil, err
			}
			needle := strings.ToLower(strings.TrimSpace(a.Query))
			wanted := map[string]bool{}
			for _, t := range upper(a.Tickers) {
				wanted[t] = true
			}
			var found []map[string]any
			for _, asset := range cat.Assets() {
				if len(wanted) > 0 && !wanted[strings.ToUpper(asset.Ticker)] {
					continue
				}
				if needle != "" && !strings.Contains(strings.ToLower(asset.Ticker+" "+asset.Name), needle) {
					continue
				}
				found = append(found, map[string]any{"ticker": asset.Ticker, "name": asset.Name, "kind": asset.Kind})
			}
			limit := clamp(a.Limit, 1, 30, 20)
			total := len(found)
			if a.Offset < len(found) {
				found = found[a.Offset:]
			} else {
				found = nil
			}
			if len(found) > limit {
				found = found[:limit]
			}
			return map[string]any{"total": total, "offset": a.Offset, "assets": found}, nil
		},
	}, {
		Name:        "asset_history",
		upstream:    true,
		Description: "Daily closing prices for one asset over a calendar year, for checking a rule's premise against what actually happened.",
		InputSchema: object(map[string]any{
			"ticker": str("Asset ticker, as find_assets returns it."),
			"year":   integer("Calendar year.", 2000, 2100),
		}, "ticker", "year"),
		run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Ticker string `json:"ticker"`
				Year   int    `json:"year"`
			}](raw)
			if err != nil {
				return nil, err
			}
			v, err := cat.AssetCloses(ctx, a.Ticker, a.Year)
			if err != nil {
				return nil, err
			}
			// Month ends only: a year of daily closes is 250 numbers a model
			// cannot use, and the shape is what it is actually asking about.
			months := map[string]float64{}
			for i, date := range v.Dates {
				if i < len(v.Closes) && len(date) >= 7 {
					months[date[:7]] = v.Closes[i]
				}
			}
			return map[string]any{"ticker": v.Ticker, "year": v.Year, "tradingDays": len(v.Dates), "monthEndCloses": months}, nil
		},
	}}
}

/* ── Helpers ────────────────────────────────────────────────────────── */

func or(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func clamp(value, low, high, fallback int) int {
	if value == 0 {
		return fallback
	}
	return min(max(value, low), high)
}

func upper(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.ToUpper(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func lower(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}
