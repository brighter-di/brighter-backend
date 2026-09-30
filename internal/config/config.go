// Package config reads the service configuration from the environment.
//
// Every value has a safe default except the secrets. Missing secrets do not
// fail startup: the service boots, reports the gap on /health/ready, and the
// routes that need the secret answer 503 — so a misconfigured deploy is loud
// but still observable.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Host string
	Port int

	DatabaseURL         string
	HistoricalPricesURL string

	LogLevel  slog.Level
	LogFormat string // "json" | "text"

	// Browser origins allowed to call the API. Empty = same-origin only.
	CORSOrigins []string
	// Trust X-Forwarded-For / X-Real-IP for client IPs. Only enable behind a
	// proxy that overwrites those headers (nginx on the VPS does).
	TrustProxy bool

	BacktestEngine  BacktestEngine
	PortfolioEngine PortfolioEngine
	Schedule        Schedule
	Telegram        Telegram

	// Chains users may invest on, as registry chain ids. A chain can be in
	// the token registry (and keep serving existing portfolios) without being
	// open for new investment.
	EnabledChains []int64

	// Freshness gates for any target that reaches PortfolioEngine. Counted in trading
	// days so a Monday check after a Thursday holiday is not "stale".
	MaxCutoffAgeTradingDays int
	MaxDataLagDays          int

	// Bearer token for /v1/admin/*. Admin routes are disabled when empty.
	AdminToken string

	// Warm the default-parameter backtests and targets at startup and keep
	// them fresh, so the strategy list never waits on BacktestEngine.
	Warmup bool
}

type BacktestEngine struct {
	BaseURL string
	KeyID   string
	Secret  string
}

type PortfolioEngine struct {
	BaseURL string
	APIKey  string

	// Rebalance cadence for every strategy we create. An interval by default
	// (hourly, as Brighter's existing strategies use). ScheduleCron switches
	// to a cron in ScheduleTimezone — ideal for Ondo, which only mints and
	// redeems in US market hours — but the portfolio engine must enable cron schedules for
	// the tenant first; until then it rejects them.
	ScheduleFrequency string
	ScheduleCron      string
	ScheduleTimezone  string

	SlippageBps    int
	PriceImpactBps int
}

// Telegram posts events in full to an operator chat and, without portfolio
// ids or wallet addresses, to every chat that pressed Start on the bot. Both
// values are needed; without either the bot is off, so a development copy
// holding only the token never competes with production for its updates.
type Telegram struct {
	BotToken string
	ChatID   string
}

type Schedule struct {
	Enabled bool
	// Earliest UTC time of day to evaluate. A lower bound, not a deadline: if
	// The backtest engine has not processed the close yet the check holds and retries.
	CheckAfterHour   int
	CheckAfterMinute int
	Tick             time.Duration
	MaxAttempts      int
	// Days to keep routine evaluations (held, error, unchanged). Published
	// ones, and each combo's newest accepted one, are kept regardless. 0
	// keeps everything.
	RetentionDays int
}

func Load() (*Config, error) {
	var errs []error
	e := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	c := &Config{
		Host:                str("HOST", "127.0.0.1"),
		DatabaseURL:         str("DATABASE_URL", ""),
		HistoricalPricesURL: str("HISTORICAL_PRICES_URL", ""),
		LogFormat:           str("LOG_FORMAT", "json"),
		CORSOrigins:         list("CORS_ORIGINS"),
		AdminToken:          str("ADMIN_TOKEN", ""),
		BacktestEngine: BacktestEngine{
			BaseURL: strings.TrimRight(str("BACKTEST_ENGINE_API_BASE", "https://backtest-engine.example.com"), "/"),
			KeyID:   str("BACKTEST_ENGINE_API_KEY_ID", ""),
			Secret:  str("BACKTEST_ENGINE_API_SECRET", ""),
		},
		PortfolioEngine: PortfolioEngine{
			BaseURL:           strings.TrimRight(str("PORTFOLIO_ENGINE_BASE_URL", "https://portfolio-engine.example.com/v2"), "/"),
			APIKey:            str("PORTFOLIO_ENGINE_API_KEY", ""),
			ScheduleFrequency: str("PORTFOLIO_ENGINE_SCHEDULE_FREQUENCY", "hourly"),
			ScheduleCron:      str("PORTFOLIO_ENGINE_SCHEDULE_CRON", ""),
			ScheduleTimezone:  str("PORTFOLIO_ENGINE_SCHEDULE_TZ", "America/New_York"),
		},
		Telegram: Telegram{
			BotToken: str("TELEGRAM_BOT_TOKEN", ""),
			ChatID:   str("TELEGRAM_CHAT_ID", ""),
		},
	}

	var err error
	c.Port, err = integer("PORT", 3200)
	e(err)
	c.TrustProxy, err = boolean("TRUST_PROXY", false)
	e(err)
	c.Warmup, err = boolean("WARMUP", true)
	e(err)
	c.PortfolioEngine.SlippageBps, err = integer("PORTFOLIO_ENGINE_SLIPPAGE_BPS", 50)
	e(err)
	c.PortfolioEngine.PriceImpactBps, err = integer("PORTFOLIO_ENGINE_PRICE_IMPACT_BPS", 100)
	e(err)
	c.MaxCutoffAgeTradingDays, err = integer("MAX_CUTOFF_AGE_TRADING_DAYS", 3)
	e(err)
	c.MaxDataLagDays, err = integer("MAX_DATA_LAG_DAYS", 4)
	e(err)

	c.Schedule.Enabled, err = boolean("SCHEDULER", true)
	e(err)
	c.Schedule.CheckAfterHour, c.Schedule.CheckAfterMinute, err = clock("SCHEDULE_CHECK_AFTER", "21:30")
	e(err)
	c.Schedule.Tick, err = duration("SCHEDULE_TICK", 30*time.Minute)
	e(err)
	c.Schedule.MaxAttempts, err = integer("SCHEDULE_MAX_ATTEMPTS", 6)
	e(err)
	c.Schedule.RetentionDays, err = integer("EVALUATION_RETENTION_DAYS", 90)
	e(err)

	c.LogLevel, err = level(str("LOG_LEVEL", "info"))
	e(err)

	// BNB Chain only until Ethereum is switched on for the tenant.
	for _, v := range strings.Split(str("CHAINS", "56"), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("CHAINS: %q is not a chain id", v))
			continue
		}
		c.EnabledChains = append(c.EnabledChains, id)
	}

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.PortfolioEngine.SlippageBps < 0 || c.PortfolioEngine.SlippageBps > 1000 {
		errs = append(errs, fmt.Errorf("PORTFOLIO_ENGINE_SLIPPAGE_BPS must be 0–1000, got %d", c.PortfolioEngine.SlippageBps))
	}
	if c.PortfolioEngine.PriceImpactBps < 0 || c.PortfolioEngine.PriceImpactBps > 1000 {
		errs = append(errs, fmt.Errorf("PORTFOLIO_ENGINE_PRICE_IMPACT_BPS must be 0–1000, got %d", c.PortfolioEngine.PriceImpactBps))
	}
	if _, err := time.LoadLocation(c.PortfolioEngine.ScheduleTimezone); err != nil {
		errs = append(errs, fmt.Errorf("PORTFOLIO_ENGINE_SCHEDULE_TZ: %w", err))
	}
	if c.PortfolioEngine.ScheduleCron != "" && len(strings.Fields(c.PortfolioEngine.ScheduleCron)) != 5 {
		errs = append(errs, fmt.Errorf("PORTFOLIO_ENGINE_SCHEDULE_CRON must be empty or a five-field cron expression, got %q", c.PortfolioEngine.ScheduleCron))
	}
	switch c.PortfolioEngine.ScheduleFrequency {
	case "hourly", "daily", "weekly", "monthly":
	default:
		errs = append(errs, fmt.Errorf("PORTFOLIO_ENGINE_SCHEDULE_FREQUENCY must be hourly, daily, weekly or monthly, got %q", c.PortfolioEngine.ScheduleFrequency))
	}
	if c.MaxCutoffAgeTradingDays < 1 {
		errs = append(errs, errors.New("MAX_CUTOFF_AGE_TRADING_DAYS must be >= 1"))
	}
	if c.Schedule.Tick < time.Minute {
		errs = append(errs, errors.New("SCHEDULE_TICK must be >= 1m"))
	}
	if c.Schedule.MaxAttempts < 1 {
		errs = append(errs, errors.New("SCHEDULE_MAX_ATTEMPTS must be >= 1"))
	}
	if c.Schedule.RetentionDays < 0 {
		errs = append(errs, errors.New("EVALUATION_RETENTION_DAYS must be >= 0 (0 keeps everything)"))
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be json or text, got %q", c.LogFormat))
	}

	return c, errors.Join(errs...)
}

func (c *Config) Addr() string { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

func str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func integer(key string, def int) (int, error) {
	v := str(key, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("%s: %q is not an integer", key, v)
	}
	return n, nil
}

func boolean(key string, def bool) (bool, error) {
	v := strings.ToLower(str(key, ""))
	switch v {
	case "":
		return def, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return def, fmt.Errorf("%s: %q is not a boolean", key, v)
}

func duration(key string, def time.Duration) (time.Duration, error) {
	v := str(key, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func clock(key, def string) (int, int, error) {
	v := str(key, def)
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %q is not HH:MM", key, v)
	}
	return t.Hour(), t.Minute(), nil
}

func list(key string) []string {
	var out []string
	for _, p := range strings.Split(str(key, ""), ",") {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func level(v string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		return slog.LevelInfo, fmt.Errorf("LOG_LEVEL: %w", err)
	}
	return l, nil
}
