// brighterd is the Brighter strategies backend: the backtest engine research, the portfolio engine
// strategies and portfolios, and the standing daily re-evaluation.
//
//	brighterd                 serve the API (default)
//	brighterd migrate         apply database migrations and exit
//	brighterd verify-tokens   check every registry address against the chain
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/brighter-di/backend/internal/api"
	"github.com/brighter-di/backend/internal/backtestengine"
	"github.com/brighter-di/backend/internal/config"
	"github.com/brighter-di/backend/internal/notify"
	"github.com/brighter-di/backend/internal/portfolioengine"
	"github.com/brighter-di/backend/internal/prices"
	"github.com/brighter-di/backend/internal/scheduler"
	"github.com/brighter-di/backend/internal/service"
	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/tokens"
)

func main() { os.Exit(run()) }

func run() int {
	if _, err := config.LoadDotEnv(".env"); err != nil {
		fmt.Fprintln(os.Stderr, "load .env:", err)
		return 1
	}
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "verify-tokens":
		return verifyTokens()
	case "serve", "migrate":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (serve | migrate | verify-tokens)\n", cmd)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	for _, id := range cfg.EnabledChains {
		if _, ok := tokens.ChainByID(id); !ok {
			fmt.Fprintf(os.Stderr, "config: CHAINS: chain %d is not in the token registry\n", id)
			return 1
		}
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database", "err", err)
		return 1
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		log.Error("migrate", "err", err)
		return 1
	}
	if cmd == "migrate" {
		log.Info("migrations applied")
		return 0
	}

	hc := &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}}
	cc := backtestengine.New(backtestengine.Options{BaseURL: cfg.BacktestEngine.BaseURL, KeyID: cfg.BacktestEngine.KeyID, Secret: cfg.BacktestEngine.Secret, Logger: log, HTTPClient: hc})
	gc := portfolioengine.New(portfolioengine.Options{BaseURL: cfg.PortfolioEngine.BaseURL, APIKey: cfg.PortfolioEngine.APIKey, Logger: log, HTTPClient: hc})
	if !cc.Configured() {
		log.Warn("BACKTEST_ENGINE_API_KEY_ID / BACKTEST_ENGINE_API_SECRET not set — research routes will answer 503")
	}
	if !gc.Configured() {
		log.Warn("PORTFOLIO_ENGINE_API_KEY not set — investing and portfolio routes will answer 503")
	}

	pc := prices.New(prices.Options{Logger: log, HTTPClient: hc, IndexedURL: cfg.HistoricalPricesURL})
	tgOpts := notify.Options{Token: cfg.Telegram.BotToken, ChatID: cfg.Telegram.ChatID, Logger: log, HTTPClient: hc}
	if db != nil {
		tgOpts.Subscribers = db
	}
	notifier := notify.New(tgOpts)
	if notifier.Enabled() {
		go notifier.Run(ctx)
		go notifier.Poll(ctx)
	} else {
		log.Info("Telegram notifications off (TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID not set)")
	}
	svc := service.New(cfg, db, cc, gc, pc, log).WithNotifier(notifier)
	sched := scheduler.New(svc, db, cfg.Schedule, log)

	if cfg.Warmup && cc.Configured() {
		go svc.Warm(ctx)
	}
	if cfg.Warmup {
		go svc.WarmCloses(ctx)
	}
	if cfg.Schedule.Enabled && cc.Configured() && gc.Configured() {
		go sched.Run(ctx)
	} else if cfg.Schedule.Enabled {
		log.Warn("scheduler not started: it needs both BacktestEngine and PortfolioEngine credentials")
	}

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           api.New(cfg, svc, sched, db, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Cold backtests can take minutes; the write deadline must outlast them.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr(), "backtestEngine", cc.Configured(), "portfolioEngine", gc.Configured(), "scheduler", cfg.Schedule.Enabled)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			return 1
		}
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			log.Error("shutdown", "err", err)
			return 1
		}
	}
	return 0
}

func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

func verifyTokens() int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	failed := 0
	for _, c := range tokens.Verify(ctx, &http.Client{Timeout: 20 * time.Second}) {
		mark := "ok  "
		if !c.OK() {
			mark, failed = "FAIL", failed+1
		}
		line := fmt.Sprintf("%s  %-9s %-6s %s  want %-8s got %-8s decimals %d", mark, c.Chain, c.Ticker, c.Address, c.Want, c.Got, c.Decimals)
		if c.Err != nil {
			line += "  err: " + c.Err.Error()
		}
		fmt.Println(line)
	}
	if failed > 0 {
		fmt.Printf("\n%d check(s) failed\n", failed)
		return 1
	}
	fmt.Println("\nall registry entries match the chain")
	return 0
}
