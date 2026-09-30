// Code generated from internal/spec/strategy-language.json. DO NOT EDIT.
package core

const LanguageVersion = 1
const IndentWidth = 2
const languageSHA = "5581beef6d2090da1b4a43784159df952208e930092c6d281dd2c689873a7aa7"
const customMaxDepth = 20
const customMaxBlocks = 2000
const customMaxList = 1000
const customMaxCount = 50
const customMinDays = 2
const customMaxDays = 2520
const customMaxName = 80
const customMaxValue = 1000000
const customMinCorridor = 1
const customMaxCorridor = 50
const customMaxConditionParts = 32
const customMaxExpandedBlocks = 6000
const customMinPercent = 1
const customMaxPercent = 100
const Version = "0.3.3"

var customRebalances = []string{"daily", "weekly", "monthly", "quarterly", "yearly", "threshold"}
var customComparators = []string{"gt", "gte", "eq", "lte", "lt"}
var comparatorCodes = map[string]string{
	"gt":  ">",
	"gte": ">=",
	"eq":  "=",
	"lte": "<=",
	"lt":  "<",
}
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
var metricCodes = map[string]string{
	"price":                      "price",
	"moving-average":             "moving_average",
	"exponential-moving-average": "ema",
	"cumulative-return":          "return",
	"moving-average-return":      "return_average",
	"rsi":                        "rsi",
	"max-drawdown":               "drawdown",
	"standard-deviation-return":  "volatility",
	"standard-deviation-price":   "price_volatility",
	"ppo":                        "ppo",
	"ppo-signal":                 "ppo_signal",
}
var metricsByCode = map[string]string{
	"price":            "price",
	"moving_average":   "moving-average",
	"ema":              "exponential-moving-average",
	"return":           "cumulative-return",
	"return_average":   "moving-average-return",
	"rsi":              "rsi",
	"drawdown":         "max-drawdown",
	"volatility":       "standard-deviation-return",
	"price_volatility": "standard-deviation-price",
	"ppo":              "ppo",
	"ppo_signal":       "ppo-signal",
}

type Node struct {
	Kind          string  `json:"kind"`
	Type          string  `json:"type,omitempty"`
	Conditions    []Node  `json:"conditions,omitempty"`
	Predicate     *Node   `json:"predicate,omitempty"`
	Name          string  `json:"name,omitempty"`
	Method        string  `json:"method,omitempty"`
	Count         int     `json:"count,omitempty"`
	Direction     string  `json:"direction,omitempty"`
	Percent       int     `json:"percent,omitempty"`
	Ticker        string  `json:"ticker,omitempty"`
	Metric        string  `json:"metric,omitempty"`
	Lookback      int     `json:"lookback,omitempty"`
	ShortWindow   int     `json:"shortWindow,omitempty"`
	LongWindow    int     `json:"longWindow,omitempty"`
	SmoothWindow  int     `json:"smoothWindow,omitempty"`
	Comparator    string  `json:"comparator,omitempty"`
	CompareMode   string  `json:"compareMode,omitempty"`
	Value         float64 `json:"value,omitempty"`
	RightMetric   string  `json:"rightMetric,omitempty"`
	RightTicker   string  `json:"rightTicker,omitempty"`
	RightLookback int     `json:"rightLookback,omitempty"`
	RightShort    int     `json:"rightShortWindow,omitempty"`
	RightLong     int     `json:"rightLongWindow,omitempty"`
	RightSmooth   int     `json:"rightSmoothWindow,omitempty"`
	Children      []Node  `json:"children,omitempty"`
	ThenChildren  []Node  `json:"thenChildren,omitempty"`
	ElseChildren  []Node  `json:"elseChildren,omitempty"`
}
type Strategy struct {
	Name      string `json:"name"`
	Rebalance string `json:"rebalance"`
	Corridor  int    `json:"corridor,omitempty"`
	Benchmark string `json:"benchmark"`
	Children  []Node `json:"children"`
}
