// Package scheduler runs the standing re-evaluation once per trading day,
// after the US close. It computes and publishes allocations; it never moves
// user funds — the portfolio engine's own schedule does that, inside market hours.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/brighter-di/backend/internal/config"
	"github.com/brighter-di/backend/internal/service"
	"github.com/brighter-di/backend/internal/store"
)

type Scheduler struct {
	svc   *service.Service
	store *store.Store
	cfg   config.Schedule
	log   *slog.Logger
	now   func() time.Time
}

func New(svc *service.Service, st *store.Store, cfg config.Schedule, log *slog.Logger) *Scheduler {
	return &Scheduler{svc: svc, store: st, cfg: cfg, log: log.With("component", "scheduler"), now: time.Now}
}

type Report struct {
	Trigger string                       `json:"trigger"`
	Ran     bool                         `json:"ran"`
	Reason  string                       `json:"reason,omitempty"`
	Results []service.ReevaluationResult `json:"results"`
	// Routine evaluations deleted by retention on this run.
	Pruned int64 `json:"pruned,omitempty"`
}

// Run ticks until ctx is cancelled. The weekday/time window is a cheap filter
// to avoid pointless calls, never a correctness claim: the data gates in
// Reevaluate decide whether anything is accepted.
func (s *Scheduler) Run(ctx context.Context) {
	s.log.Info("scheduler started", "check_after_utc", time.Date(0, 1, 1, s.cfg.CheckAfterHour, s.cfg.CheckAfterMinute, 0, 0, time.UTC).Format("15:04"), "tick", s.cfg.Tick)
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		if s.due(s.now()) {
			s.RunOnce(ctx, "schedule")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Scheduler) due(now time.Time) bool {
	now = now.UTC()
	if wd := now.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	return now.Hour() > s.cfg.CheckAfterHour || (now.Hour() == s.cfg.CheckAfterHour && now.Minute() >= s.cfg.CheckAfterMinute)
}

// RunOnce re-evaluates every combo anyone holds, then applies evaluation
// retention. Only one instance runs at a time, enforced by a Postgres
// advisory lock.
func (s *Scheduler) RunOnce(ctx context.Context, trigger string) Report {
	r := Report{Trigger: trigger, Results: []service.ReevaluationResult{}}
	ran, err := s.store.TryLock(ctx, "brighter:scheduler", func(ctx context.Context) error {
		combos, err := s.svc.Combos(ctx)
		if err != nil {
			return err
		}
		for _, c := range combos {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.Results = append(r.Results, s.svc.Reevaluate(ctx, c, trigger, s.cfg.MaxAttempts))
		}
		s.prune(ctx, &r)
		return nil
	})
	r.Ran = ran
	switch {
	case err != nil:
		r.Reason = err.Error()
		s.log.Error("scheduler run failed", "trigger", trigger, "err", err)
	case !ran:
		r.Reason = "another instance holds the scheduler lock"
	}
	return r
}

// prune deletes routine evaluations past retention. A failure is logged, not
// fatal: it only delays the cleanup to the next run.
func (s *Scheduler) prune(ctx context.Context, r *Report) {
	if s.cfg.RetentionDays <= 0 {
		return
	}
	n, err := s.store.PruneEvaluations(ctx, s.now().AddDate(0, 0, -s.cfg.RetentionDays))
	if err != nil {
		s.log.Warn("evaluation retention failed", "err", err)
		return
	}
	r.Pruned = n
	if n > 0 {
		s.log.Info("evaluations pruned", "deleted", n, "older_than_days", s.cfg.RetentionDays)
	}
}
