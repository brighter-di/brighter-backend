package core

import (
	"crypto/rand"
	"fmt"
	"strconv"
)

type node = map[string]any

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func asset(t string) node {
	return node{"id": newID(), "step": "asset", "ticker": t, "name": t}
}

func assets(ts ...string) []node {
	out := make([]node, len(ts))
	for i, t := range ts {
		out[i] = asset(t)
	}
	return out
}

func wtEqual(children ...node) node {
	return node{"id": newID(), "step": "wt-cash-equal", "children": children}
}

func wtInverseVol(window int, children ...node) node {
	return node{"id": newID(), "step": "wt-inverse-vol", "window-days": strconv.Itoa(window), "children": children}
}

// wtSpecified splits the capital by a percentage per child. The backtest engine carries
// the share on the child itself, as a fraction of 100.
func wtSpecified(percents []int, children ...node) node {
	for i, c := range children {
		c["weight"] = node{"num": strconv.Itoa(percents[i]), "den": 100}
	}
	return node{"id": newID(), "step": "wt-cash-specified", "children": children}
}

// filter keeps the n children ranked highest ("top") or lowest ("bottom") by
// fn over the window.
func filter(selectFn string, n int, fn string, window int, children ...node) node {
	return node{
		"id": newID(), "step": "filter",
		"select?": true, "select-fn": selectFn, "select-n": strconv.Itoa(n),
		"sort-by-fn": fn, "sort-by-window-days": strconv.Itoa(window),
		"children": children,
	}
}

func group(name string, children ...node) node {
	return node{"id": newID(), "step": "group", "name": name, "collapsed?": false, "children": children}
}

// operand is one side of a condition: an indicator over a ticker, or a fixed
// number (fixedVal set).
type operand struct {
	fn     string
	val    string
	window int // 0 = the indicator takes no window
	// Named windows, for the oscillators that take more than one
	// (short-window, long-window, smooth-window).
	params   map[string]int
	fixedVal *string // The backtest engine's numeric fields are strings
}

func fixed(v int) operand {
	s := strconv.Itoa(v)
	return operand{fixedVal: &s}
}

// ifElse builds the nested if/if-child shape, which the PoC's gate confirmed
// is what the backtest engine accepts.
func ifElse(lhs operand, comparator string, rhs operand, then, otherwise []node) node {
	cond := node{
		"id": newID(), "step": "if-child", "is-else-condition?": false,
		"lhs-fn": lhs.fn, "lhs-val": lhs.val, "comparator": comparator,
		"children": then,
	}
	if lhs.window > 0 {
		cond["lhs-window-days"] = strconv.Itoa(lhs.window)
	}
	if len(lhs.params) > 0 {
		cond["lhs-fn-params"] = lhs.params
	}
	if rhs.fixedVal != nil {
		cond["rhs-fixed-value?"] = true
		cond["rhs-val"] = *rhs.fixedVal
	} else {
		cond["rhs-fixed-value?"] = false
		cond["rhs-fn"] = rhs.fn
		cond["rhs-val"] = rhs.val
		if rhs.window > 0 {
			cond["rhs-window-days"] = strconv.Itoa(rhs.window)
		}
		if len(rhs.params) > 0 {
			cond["rhs-fn-params"] = rhs.params
		}
	}
	return node{
		"id": newID(), "step": "if",
		"children": []node{
			cond,
			{"id": newID(), "step": "if-child", "is-else-condition?": true, "children": otherwise},
		},
	}
}

func root(name, rebalance string, children ...node) node {
	return node{
		"id": newID(), "step": "root", "name": name,
		"description": "Brighter strategy DSL. Generated payload — not a saved strategyTree.",
		"rebalance":   rebalance,
		"children":    children,
	}
}

// rootCorridor rebalances on drift instead of a calendar: the backtest engine reads no
// calendar plus a corridor width as "whenever a weight strays this far".
func rootCorridor(name string, corridor float64, children ...node) node {
	r := root(name, "none", children...)
	r["rebalance-corridor-width"] = corridor
	return r
}
