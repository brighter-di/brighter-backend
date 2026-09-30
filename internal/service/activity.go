package service

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/brighter-di/backend/internal/tokens"
)

// ChangeLine is one asset whose weight moved in an allocation change.
type ChangeLine struct {
	Ticker     string `json:"ticker"`
	OndoSymbol string `json:"ondoSymbol"`
	Icon       string `json:"icon"`
	FromBps    int    `json:"fromBps"`
	ToBps      int    `json:"toBps"`
}

// AllocationChange is an allocation Brighter published for a portfolio's
// strategy, set against the one it replaced. The portfolio moves to it at its
// next rebalance.
type AllocationChange struct {
	At time.Time `json:"at"`
	// The close the new allocation was computed from.
	Cutoff string `json:"cutoff"`
	// The assets whose weight moved, largest move first.
	Lines []ChangeLine `json:"lines"`
}

type ActivityView struct {
	// Newest first.
	Changes []AllocationChange `json:"changes"`
}

// How many changes a caller may ask for at most.
const maxActivityChanges = 20

// What a caller gets without asking. Weights drift with prices every day, so
// the history is mostly the same holdings re-weighted — a monthly rule ends up
// recording a change most days. Only the allocation in force is worth drawing,
// and a caller that wants the history asks for it by name.
const defaultActivityChanges = 1

// Activity lists the allocation changes published for a portfolio's strategy
// since the portfolio was created, newest first, each against the allocation
// before it. The portfolio engine reports an operation's state but not its trades; Brighter
// keeps every allocation it publishes, so the change itself is known exactly.
//
// limit is how many to return: zero or less means defaultActivityChanges, and
// more than maxActivityChanges is capped there.
func (s *Service) Activity(ctx context.Context, id string, limit int) (*ActivityView, error) {
	if limit < 1 {
		limit = defaultActivityChanges
	}
	limit = min(limit, maxActivityChanges)
	p, err := s.knownPortfolio(ctx, id)
	if err != nil {
		return nil, err
	}
	evals, err := s.store.PublishedEvaluations(ctx, p.Strategy.TemplateID, p.Strategy.ParamsKey)
	if err != nil {
		return nil, err
	}
	view := &ActivityView{Changes: []AllocationChange{}}
	var prev map[string]int
	for _, e := range evals {
		// The allocation in force when the portfolio was created is where it
		// started, not something that happened to it.
		if prev != nil && e.EvaluatedAt.After(p.CreatedAt) {
			if lines := changeLines(prev, e.Weights); len(lines) > 0 {
				c := AllocationChange{At: e.EvaluatedAt, Lines: lines}
				if e.Cutoff != nil {
					c.Cutoff = e.Cutoff.Format(dateFmt)
				}
				view.Changes = append(view.Changes, c)
			}
		}
		prev = e.Weights
	}
	slices.Reverse(view.Changes)
	if len(view.Changes) > limit {
		view.Changes = view.Changes[:limit]
	}
	return view, nil
}

// changeLines lists the assets whose weight differs between two allocations,
// largest move first.
func changeLines(from, to map[string]int) []ChangeLine {
	seen := map[string]bool{}
	var out []ChangeLine
	for _, m := range []map[string]int{to, from} {
		for t := range m {
			if seen[t] || from[t] == to[t] {
				continue
			}
			seen[t] = true
			l := ChangeLine{Ticker: t, OndoSymbol: t, FromBps: from[t], ToBps: to[t]}
			if a, ok := tokens.Lookup(t); ok {
				l.OndoSymbol, l.Icon = a.OndoSymbol, a.Icon()
			}
			out = append(out, l)
		}
	}
	move := func(l ChangeLine) int {
		if d := l.ToBps - l.FromBps; d > 0 {
			return d
		} else {
			return -d
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if mi, mj := move(out[i]), move(out[j]); mi != mj {
			return mi > mj
		}
		return out[i].Ticker < out[j].Ticker
	})
	return out
}
