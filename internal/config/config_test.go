package config

import (
	"strings"
	"testing"
)

var envKeys = []string{
	"HOST", "PORT", "DATABASE_URL", "LOG_LEVEL", "LOG_FORMAT", "CORS_ORIGINS", "TRUST_PROXY", "ADMIN_TOKEN", "WARMUP",
	"BACKTEST_ENGINE_API_BASE", "BACKTEST_ENGINE_API_KEY_ID", "BACKTEST_ENGINE_API_SECRET",
	"PORTFOLIO_ENGINE_BASE_URL", "PORTFOLIO_ENGINE_API_KEY", "PORTFOLIO_ENGINE_SCHEDULE_FREQUENCY", "PORTFOLIO_ENGINE_SCHEDULE_CRON", "PORTFOLIO_ENGINE_SCHEDULE_TZ",
	"PORTFOLIO_ENGINE_SLIPPAGE_BPS", "PORTFOLIO_ENGINE_PRICE_IMPACT_BPS",
	"MAX_CUTOFF_AGE_TRADING_DAYS", "MAX_DATA_LAG_DAYS", "SCHEDULER", "SCHEDULE_CHECK_AFTER", "SCHEDULE_TICK",
	"SCHEDULE_MAX_ATTEMPTS", "EVALUATION_RETENTION_DAYS", "CHAINS",
}

// env blanks every variable Load reads (blank counts as unset), then applies set.
func env(t *testing.T, set map[string]string) {
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
	for k, v := range set {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	env(t, map[string]string{"DATABASE_URL": "postgres://localhost/brighter"})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case c.Addr() != "127.0.0.1:3200":
		t.Errorf("addr %s", c.Addr())
	case len(c.EnabledChains) != 1 || c.EnabledChains[0] != 56:
		t.Errorf("chains %v", c.EnabledChains)
	case c.PortfolioEngine.ScheduleFrequency != "hourly" || c.PortfolioEngine.ScheduleCron != "":
		t.Errorf("portfolioEngine schedule %+v", c.PortfolioEngine)
	case c.Schedule.CheckAfterHour != 21 || c.Schedule.CheckAfterMinute != 30 || c.Schedule.MaxAttempts != 6:
		t.Errorf("schedule %+v", c.Schedule)
	case c.Schedule.RetentionDays != 90:
		t.Errorf("retention %d", c.Schedule.RetentionDays)
	case !c.Schedule.Enabled || !c.Warmup || c.TrustProxy || c.LogFormat != "json":
		t.Errorf("flags %+v", c)
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	env(t, map[string]string{
		"PORT": "x", "PORTFOLIO_ENGINE_SCHEDULE_FREQUENCY": "yearly", "PORTFOLIO_ENGINE_SCHEDULE_CRON": "30 10 * *",
		"CHAINS": "56,abc", "EVALUATION_RETENTION_DAYS": "-1",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"DATABASE_URL is required", "PORT", "PORTFOLIO_ENGINE_SCHEDULE_FREQUENCY", "PORTFOLIO_ENGINE_SCHEDULE_CRON", `CHAINS: "abc"`, "EVALUATION_RETENTION_DAYS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}
}
