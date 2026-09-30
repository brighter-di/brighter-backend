package scheduler

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/backtestengine"
	"github.com/brighter-di/backend/internal/config"
	"github.com/brighter-di/backend/internal/portfolioengine"
	"github.com/brighter-di/backend/internal/prices"
	"github.com/brighter-di/backend/internal/service"
	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/testkit"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestDue(t *testing.T) {
	s := &Scheduler{cfg: config.Schedule{CheckAfterHour: 21, CheckAfterMinute: 30}}
	thursday := func(h, m int) time.Time { return time.Date(2026, 9, 10, h, m, 0, 0, time.UTC) }
	cases := []struct {
		at   time.Time
		want bool
	}{
		{thursday(9, 0), false},
		{thursday(21, 29), false},
		{thursday(21, 30), true},
		{thursday(23, 59), true},
		{time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC), false}, // Saturday
	}
	for _, c := range cases {
		if got := s.due(c.at); got != c.want {
			t.Errorf("due(%s) = %v", c.at.Format(time.RFC3339), got)
		}
	}
}

func TestRunOncePrunesRoutineEvaluations(t *testing.T) {
	ctx := context.Background()
	st := testkit.Store(t)
	insert := func(status string) {
		if err := st.InsertEvaluation(ctx, &store.Evaluation{TemplateID: "mag7-rank", ParamsKey: "k", Trigger: "schedule", Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"published", "held", "error"} {
		insert(status)
	}
	svc := service.New(&config.Config{}, st, backtestengine.New(backtestengine.Options{}), portfolioengine.New(portfolioengine.Options{}), prices.New(prices.Options{}), discard)
	s := New(svc, st, config.Schedule{RetentionDays: 30}, discard)

	s.now = func() time.Time { return time.Now().AddDate(0, 0, 31) }
	r := s.RunOnce(ctx, "manual")
	if !r.Ran || r.Reason != "" || r.Pruned != 2 || len(r.Results) != 0 {
		t.Fatalf("report %+v", r)
	}
	left, _ := st.RecentEvaluations(ctx, 10)
	if len(left) != 1 || left[0].Status != "published" {
		t.Fatalf("left %+v", left)
	}

	// Inside the retention window nothing is deleted.
	insert("held")
	s.now = time.Now
	if r := s.RunOnce(ctx, "manual"); r.Pruned != 0 {
		t.Fatalf("pruned %d inside retention", r.Pruned)
	}
}
