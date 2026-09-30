// Generated from language/strategy-language.json (5581beef6d2090da1b4a43784159df952208e930092c6d281dd2c689873a7aa7). Do not edit.
package strategy

const (
	customMaxDepth          = 20
	customMaxBlocks         = 2000
	customMaxList           = 1000
	customMaxCount          = 50
	customMinDays           = 2
	customMaxDays           = 2520
	customMaxName           = 80
	customMaxValue          = 1000000
	customMinCorridor       = 1
	customMaxCorridor       = 50
	customMaxConditionParts = 32
	customMaxExpandedBlocks = 6000
	customMinPercent        = 1
	customMaxPercent        = 100
)

var customRebalances = []string{"daily", "weekly", "monthly", "quarterly", "yearly", "threshold"}
var customComparators = []string{"gt", "gte", "eq", "lte", "lt"}
var measures = map[string]measureSpec{
	"price":                      {fn: "current-price", windowed: false, oscillator: false, smooth: false},
	"moving-average":             {fn: "moving-average-price", windowed: true, oscillator: false, smooth: false},
	"exponential-moving-average": {fn: "exponential-moving-average-price", windowed: true, oscillator: false, smooth: false},
	"cumulative-return":          {fn: "cumulative-return", windowed: true, oscillator: false, smooth: false},
	"moving-average-return":      {fn: "moving-average-return", windowed: true, oscillator: false, smooth: false},
	"rsi":                        {fn: "relative-strength-index", windowed: true, oscillator: false, smooth: false},
	"max-drawdown":               {fn: "max-drawdown", windowed: true, oscillator: false, smooth: false},
	"standard-deviation-return":  {fn: "standard-deviation-return", windowed: true, oscillator: false, smooth: false},
	"standard-deviation-price":   {fn: "standard-deviation-price", windowed: true, oscillator: false, smooth: false},
	"ppo":                        {fn: "percentage-price-oscillator", windowed: false, oscillator: true, smooth: false},
	"ppo-signal":                 {fn: "percentage-price-oscillator-signal", windowed: false, oscillator: true, smooth: true},
}

type CustomNode struct {
	Kind          string       `json:"kind"`
	Type          string       `json:"type,omitempty"`
	Conditions    []CustomNode `json:"conditions,omitempty"`
	Predicate     *CustomNode  `json:"predicate,omitempty"`
	Name          string       `json:"name,omitempty"`
	Method        string       `json:"method,omitempty"`
	Count         int          `json:"count,omitempty"`
	Direction     string       `json:"direction,omitempty"`
	Percent       int          `json:"percent,omitempty"`
	Ticker        string       `json:"ticker,omitempty"`
	Metric        string       `json:"metric,omitempty"`
	Lookback      int          `json:"lookback,omitempty"`
	ShortWindow   int          `json:"shortWindow,omitempty"`
	LongWindow    int          `json:"longWindow,omitempty"`
	SmoothWindow  int          `json:"smoothWindow,omitempty"`
	Comparator    string       `json:"comparator,omitempty"`
	CompareMode   string       `json:"compareMode,omitempty"`
	Value         float64      `json:"value,omitempty"`
	RightMetric   string       `json:"rightMetric,omitempty"`
	RightTicker   string       `json:"rightTicker,omitempty"`
	RightLookback int          `json:"rightLookback,omitempty"`
	RightShort    int          `json:"rightShortWindow,omitempty"`
	RightLong     int          `json:"rightLongWindow,omitempty"`
	RightSmooth   int          `json:"rightSmoothWindow,omitempty"`
	Children      []CustomNode `json:"children,omitempty"`
	ThenChildren  []CustomNode `json:"thenChildren,omitempty"`
	ElseChildren  []CustomNode `json:"elseChildren,omitempty"`
}
type CustomStrategy struct {
	Name      string       `json:"name"`
	Rebalance string       `json:"rebalance"`
	Corridor  int          `json:"corridor,omitempty"`
	Benchmark string       `json:"benchmark"`
	Children  []CustomNode `json:"children"`
}

const studioLanguageSHA = "5581beef6d2090da1b4a43784159df952208e930092c6d281dd2c689873a7aa7"
