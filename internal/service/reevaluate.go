package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/strategy"
	"github.com/brighter-di/backend/internal/tokens"
)

// The backtest engine applies each template's rebalance cadence inside the calculation
// (a monthly template's weights are its holdings as of its last monthly
// rebalance). What it does not do is re-run on a clock. Reevaluate closes that
// gap: after each close it recomputes every strategy anyone holds and, only if
// the weights changed, publishes a new portfolio engine version. The portfolio engine's own schedule
// then moves enrolled portfolios during the next market session.
//
// Every gate holds; none degrades. A schedule that relaxes a guardrail to keep
// flowing is worse than no schedule.

type ReevaluationResult struct {
	TemplateID      string  `json:"templateId"`
	ParamsKey       string  `json:"paramsKey"`
	Status          string  `json:"status"` // published | unchanged | held | error | skipped
	Reason          string  `json:"reason,omitempty"`
	Cutoff          string  `json:"cutoff,omitempty"`
	PublishedChains []int64 `json:"publishedChains,omitempty"`
}

func (s *Service) Reevaluate(ctx context.Context, c store.Combo, trigger string, maxAttempts int) ReevaluationResult {
	res := ReevaluationResult{TemplateID: c.TemplateID, ParamsKey: c.ParamsKey}
	tpl, lookupErr := s.Template(ctx, c.TemplateID)
	if lookupErr != nil {
		res.Status, res.Reason = "error", lookupErr.Error()
		return res
	}
	// Use the stored parameters verbatim: if a template's grid changes, the
	// strategies people already hold keep computing what they signed up for.
	params := strategy.Params(c.Params)

	if trigger == "schedule" {
		now := s.now().UTC()
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		attempts, settled, err := s.store.EvaluationsSince(ctx, c.TemplateID, c.ParamsKey, dayStart)
		switch {
		case err != nil:
			res.Status, res.Reason = "error", err.Error()
			return res
		case settled:
			res.Status, res.Reason = "skipped", "already settled today"
			return res
		case attempts >= maxAttempts:
			res.Status, res.Reason = "skipped", fmt.Sprintf("%d attempts today; waiting for the next trading day", attempts)
			return res
		}
	}

	record := func(status, reason string, t *target) ReevaluationResult {
		res.Status, res.Reason = status, reason
		e := &store.Evaluation{TemplateID: c.TemplateID, ParamsKey: c.ParamsKey, Trigger: trigger, Status: status, Reason: reason}
		if t != nil {
			res.Cutoff = t.cutoff
			if d, err := time.Parse(dateFmt, t.cutoff); err == nil {
				e.Cutoff = &d
			}
			e.Weights, e.ChangeHash = t.bps, t.hash
		}
		if err := s.store.InsertEvaluation(ctx, e); err != nil {
			s.log.Error("evaluation not recorded", "template", c.TemplateID, "params", c.ParamsKey, "err", err)
		}
		lvl := s.log.Info
		if status == "error" {
			lvl = s.log.Warn
		}
		lvl("reevaluated", "template", c.TemplateID, "params", c.ParamsKey, "status", status, "reason", reason, "cutoff", res.Cutoff, "published", res.PublishedChains)
		switch {
		case status == "published" && t != nil:
			s.announceAll(fmt.Sprintf("Allocation updated: %s (%s), close of %s: %s. Portfolios move to it at their next rebalance.",
				tpl.Name, tpl.Label(params), t.cutoff, allocText(t.bps)))
		case status == "error":
			s.announce(fmt.Sprintf("Re-evaluation failed: %s (%s): %s", tpl.Name, tpl.Label(params), reason),
				fmt.Sprintf("Re-evaluation failed: %s (%s).", tpl.Name, tpl.Label(params)))
		}
		return res
	}

	t, err := s.evaluate(ctx, tpl, params)
	if err != nil {
		return record("error", err.Error(), nil)
	}
	if t.gateErr != nil {
		return record("held", t.gateErr.Error(), t)
	}
	if f := strategy.CheckFreshness(t.cutoff, t.lagDays, s.now(), s.cfg.MaxCutoffAgeTradingDays, s.cfg.MaxDataLagDays); !f.Fresh {
		return record("held", f.Reason, t)
	}
	// The authoritative holiday check is the data: accept a target only when
	// The backtest engine's cutoff has advanced past the last one we accepted.
	cutoff, _ := time.Parse(dateFmt, t.cutoff)
	last, err := s.store.LastAcceptedCutoff(ctx, c.TemplateID, c.ParamsKey)
	if err != nil {
		return record("error", err.Error(), t)
	}
	if last != nil && !cutoff.After(*last) {
		return record("held", fmt.Sprintf("no new close since %s", last.Format(dateFmt)), t)
	}

	strategies, err := s.store.StrategiesForCombo(ctx, c.TemplateID, c.ParamsKey)
	if err != nil {
		return record("error", err.Error(), t)
	}
	var problems []string
	for _, st := range strategies {
		if st.AllocationHash == t.hash {
			continue
		}
		chain, _ := tokens.ChainByID(st.ChainID)
		lines, err := strategy.AllocationFor(t.bps, st.ChainID)
		if err != nil {
			problems = append(problems, chain.Name+": "+err.Error())
			continue
		}
		pub, err := s.portfolioEngine.PublishVersion(ctx, st.PortfolioEngineStrategyID, toPortfolioEngine(lines),
			fmt.Sprintf("Brighter target, data cutoff %s, allocation %s", t.cutoff, t.hash))
		if err != nil {
			problems = append(problems, chain.Name+": "+err.Error())
			continue
		}
		version := pub.Version
		if version <= st.Version {
			version = st.Version + 1
		}
		if err := s.store.UpdateStrategyAllocation(ctx, st.ID, t.bps, t.hash, cutoff, version); err != nil {
			s.log.Error("published version not recorded", "portfolio_engine_strategy", st.PortfolioEngineStrategyID, "version", version, "err", err)
			problems = append(problems, chain.Name+": published but not recorded: "+err.Error())
			continue
		}
		res.PublishedChains = append(res.PublishedChains, st.ChainID)
	}

	reason := strings.Join(problems, "; ")
	switch {
	case len(res.PublishedChains) > 0:
		return record("published", reason, t)
	case len(problems) > 0:
		return record("error", reason, t)
	default:
		return record("unchanged", "", t)
	}
}

// RecentEvaluations exposes the evaluation log for operators.
func (s *Service) RecentEvaluations(ctx context.Context, limit int) ([]*store.Evaluation, error) {
	return s.store.RecentEvaluations(ctx, limit)
}

// Combos lists every (template, params) that has at least one strategy.
func (s *Service) Combos(ctx context.Context) ([]store.Combo, error) {
	return s.store.ListCombos(ctx)
}
