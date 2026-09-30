// Package api is the HTTP surface. It holds no business logic: it decodes,
// calls the service, and encodes — and it is the only place errors become
// status codes.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/brighter-di/backend/internal/backtestengine"
	"github.com/brighter-di/backend/internal/config"
	"github.com/brighter-di/backend/internal/portfolioengine"
	"github.com/brighter-di/backend/internal/prices"
	"github.com/brighter-di/backend/internal/scheduler"
	"github.com/brighter-di/backend/internal/service"
	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/strategy"
)

type Server struct {
	cfg        *config.Config
	svc        *service.Service
	sched      *scheduler.Scheduler
	db         *store.Store
	log        *slog.Logger
	research   *ipLimiter // research work sent upstream (cache misses only)
	writes     *ipLimiter // anything that reaches the portfolio engine
	publishing publishingRepository
	authLimit  *ipLimiter
	custom     *globalLimiter // Studio strategy backtests, all clients together
	// The MCP endpoint answers models rather than people, and a model's loop
	// is tireless. It carries its own budget so it can never crowd the website
	// out of the backtest engine, and so the website's limits never answer for it.
	mcpIP     *ipLimiter
	mcpGlobal *globalLimiter
	mcp       http.Handler
}

func New(cfg *config.Config, svc *service.Service, sched *scheduler.Scheduler, db *store.Store, log *slog.Logger) http.Handler {
	return newServer(cfg, svc, sched, db, log).handler()
}

func newServer(cfg *config.Config, svc *service.Service, sched *scheduler.Scheduler, db *store.Store, log *slog.Logger) *Server {
	return &Server{
		cfg: cfg, svc: svc, sched: sched, db: db, publishing: db, authLimit: newIPLimiter(0.25, 8), log: log.With("component", "api"),
		// Counted on cache misses only, so cached answers — most of any page
		// load — cost nothing: 15 upstream computations a minute, bursts of 20.
		research: newIPLimiter(0.25, 20),
		writes:   newIPLimiter(1.0/3.0, 15), // 20/min
		// New Studio strategies are cache misses by nature; keep them well
		// inside the backtest engine's backtest limit, which the scheduler also needs.
		custom: newGlobalLimiter(30, 10),
		mcpIP:  newIPLimiter(0.5, 12), mcpGlobal: newGlobalLimiter(60, 8),
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health/live", s.live)
	mux.HandleFunc("GET /health/ready", s.ready)

	mux.HandleFunc("GET /v1/config", s.config)
	mux.HandleFunc("GET /v1/templates", s.templates)
	mux.Handle("GET /v1/templates/{id}/backtest", s.meter(s.research, s.backtest))
	mux.Handle("GET /v1/templates/{id}/target", s.meter(s.research, s.target))
	mux.HandleFunc("GET /v1/templates/{id}/rules", s.studioRules)
	mux.Handle("GET /v1/templates/summaries", s.meter(s.research, s.summaries))
	mux.Handle("GET /v1/strategies", s.meter(s.research, s.strategies))
	mux.HandleFunc("GET /v1/strategies/facets", s.strategyFacets)
	mux.Handle("/mcp", s.mcpHandler())
	mux.HandleFunc("POST /v1/backtests", s.backtestCustom)
	mux.Handle("POST /v1/auth/challenge", s.limit(s.authLimit, s.authChallenge))
	mux.Handle("POST /v1/auth/verify", s.limit(s.authLimit, s.authVerify))
	mux.HandleFunc("GET /v1/auth/me", s.authMe)
	mux.HandleFunc("POST /v1/auth/logout", s.authLogout)
	mux.Handle("POST /v1/strategies", s.limit(s.authLimit, s.publishStrategy))
	mux.HandleFunc("GET /v1/assets", s.assets)
	mux.HandleFunc("GET /v1/assets/{ticker}", s.asset)
	mux.Handle("GET /v1/assets/{ticker}/closes", s.meter(s.research, s.assetCloses))
	mux.Handle("GET /v1/market", s.meter(s.research, s.market))

	mux.Handle("POST /v1/invest/prepare", s.limit(s.writes, s.prepare))
	mux.Handle("POST /v1/invest/confirm", s.limit(s.writes, s.confirm))

	mux.HandleFunc("GET /v1/portfolios", s.portfolios)
	mux.HandleFunc("GET /v1/portfolios/{id}", s.portfolio)
	mux.HandleFunc("GET /v1/portfolios/{id}/positions", s.positions)
	mux.HandleFunc("GET /v1/portfolios/{id}/performance", s.performance)
	mux.HandleFunc("GET /v1/portfolios/{id}/activity", s.activity)
	mux.HandleFunc("GET /v1/portfolios/{id}/operations/{opId}", s.operation)
	mux.Handle("POST /v1/portfolios/{id}/deploy", s.limit(s.writes, s.deploy))
	mux.Handle("POST /v1/portfolios/{id}/withdraw/signature", s.limit(s.writes, s.withdrawSignature))
	mux.Handle("POST /v1/portfolios/{id}/withdraw", s.limit(s.writes, s.withdraw))
	mux.Handle("POST /v1/portfolios/{id}/archive", s.limit(s.writes, s.archive))
	mux.Handle("POST /v1/portfolios/{id}/deposits/report", s.limit(s.writes, s.reportDeposit))

	mux.HandleFunc("POST /v1/admin/reevaluate", s.admin(s.reevaluate))
	mux.HandleFunc("GET /v1/admin/evaluations", s.admin(s.evaluations))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, apiError{Error: "No such endpoint: " + r.Method + " " + r.URL.Path, Code: "NOT_FOUND"})
	})

	return s.recoverer(s.accessLog(withGzip(s.cors(mux))))
}

/* ── Health ─────────────────────────────────────────────────────────── */

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	cfg := s.svc.Config()
	out := map[string]any{"db": true, "backtestEngine": cfg.Research, "portfolioEngine": cfg.Trading, "status": "ready"}
	if err := s.db.Ping(ctx); err != nil {
		out["db"], out["status"] = false, "down"
		writeJSON(w, http.StatusServiceUnavailable, out)
		return
	}
	if !cfg.Research || !cfg.Trading {
		out["status"] = "degraded"
	}
	writeJSON(w, http.StatusOK, out)
}

/* ── Research ───────────────────────────────────────────────────────── */

func (s *Server) config(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("cache-control", "public, max-age=300")
	writeJSON(w, http.StatusOK, s.svc.Config())
}

func (s *Server) templates(w http.ResponseWriter, r *http.Request) {
	views, err := s.svc.PublicTemplates(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("cache-control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"templates": views})
}

func (s *Server) templateAndParams(r *http.Request) (*strategy.Template, strategy.Params, error) {
	tpl, err := s.svc.Template(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, nil, err
	}
	q := r.URL.Query()
	return tpl, tpl.CoerceStrings(q.Get), nil
}

func (s *Server) backtest(w http.ResponseWriter, r *http.Request) {
	tpl, params, err := s.templateAndParams(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "5y"
	}
	since, ok := s.since(w, r)
	if !ok {
		return
	}
	v, err := s.svc.Backtest(r.Context(), tpl, params, rng)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := *v
	if since != "" {
		// A client that already holds the series up to a day asks only for
		// what follows. Trimmed from the same cached backtest, so metrics and
		// startDate still describe the whole range.
		out = out.Since(since)
	} else if n, err := strconv.Atoi(r.URL.Query().Get("points")); err == nil && n > 0 {
		out = out.Downsample(min(max(n, 20), 5000))
	}
	w.Header().Set("cache-control", "public, max-age=300")
	writeJSON(w, http.StatusOK, out)
}

// summaries answers every listed strategy's card in one request: its
// downsampled backtest and, with targets=1, its current allocation.
func (s *Server) summaries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rng := q.Get("range")
	if rng == "" {
		rng = "1y"
	}
	points := 90
	if n, err := strconv.Atoi(q.Get("points")); err == nil && n > 0 {
		points = min(max(n, 20), 500)
	}
	v, err := s.svc.Summaries(r.Context(), rng, points, q.Get("targets") == "1")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("cache-control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{"summaries": v})
}

// reportDeposit takes a browser's word for nothing. A successful deposit is a
// transaction hash, checked against the chain before anything is announced; a
// failed one cannot be checked against anything, so it reaches operators only.
func (s *Server) reportDeposit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
		TxHash string `json:"txHash"`
		Reason string `json:"reason"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	switch body.Status {
	case "succeeded":
		v, err := s.svc.ReportDeposit(r.Context(), id, body.TxHash)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	case "failed":
		if err := s.svc.ReportDepositProblem(r.Context(), id, body.Reason); err != nil {
			s.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusBadRequest, apiError{Error: "status must be succeeded or failed", Code: "BAD_STATUS"})
	}
}

// strategies answers one page of the strategy list: the cards that match the
// reader's filters, in the order they asked for. The filters are applied here
// rather than in the browser so the page stays small as the catalogue grows.
func (s *Server) strategies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := service.CatalogueQuery{
		Period:         valueOr(q.Get("period"), "1y"),
		Assets:         commaList(q.Get("assets")),
		Cadences:       commaList(q.Get("cadence")),
		BeatsBenchmark: q.Get("beatsBenchmark") == "1",
		Leveraged:      q.Get("leveraged") == "1",
		Sort:           valueOr(q.Get("sort"), service.SortReturn),
		Limit:          12,
		Points:         40,
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		query.Limit = min(max(n, 1), 48)
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		query.Offset = n
	}
	if n, err := strconv.Atoi(q.Get("points")); err == nil && n > 0 {
		query.Points = min(max(n, 20), 500)
	}
	page, err := s.svc.Catalogue(r.Context(), query)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("cache-control", "public, max-age=60")
	writeJSON(w, http.StatusOK, page)
}

// strategyFacets answers what the filter row can offer: the tickers the listed
// strategies can hold, grouped by kind, and the cadences in use. Metadata
// only, so it reads no backtest and is not metered.
func (s *Server) strategyFacets(w http.ResponseWriter, r *http.Request) {
	facets, err := s.svc.CatalogueFacets(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("cache-control", "public, max-age=60")
	writeJSON(w, http.StatusOK, facets)
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// commaList reads a repeated filter value, "NVDA,AAPL". Blanks are dropped so
// a trailing comma from a hand-edited URL is not an unknown ticker.
func commaList(value string) []string {
	if value == "" {
		return nil
	}
	out := []string{}
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// studioRules answers a published strategy's rules with the requested values
// applied, so Studio opens exactly what the strategy page shows.
func (s *Server) studioRules(w http.ResponseWriter, r *http.Request) {
	tpl, params, err := s.templateAndParams(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rules, ok, err := tpl.StudioRules(params)
	if !ok {
		s.fail(w, r, &service.Error{Status: http.StatusNotFound, Code: "NO_STUDIO_RULES", Message: "This strategy cannot be opened in Studio."})
		return
	}
	if invalid, bad := err.(*strategy.CustomError); bad {
		s.fail(w, r, &service.Error{Status: http.StatusUnprocessableEntity, Code: "INVALID_STRATEGY", Message: invalid.Reason})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("cache-control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]any{"strategy": rules, "description": tpl.Thesis})
}

func (s *Server) target(w http.ResponseWriter, r *http.Request) {
	tpl, params, err := s.templateAndParams(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.svc.Target(r.Context(), tpl, params)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("cache-control", "public, max-age=60")
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) assets(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("cache-control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]any{"assets": s.svc.Assets()})
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.Asset(r.PathValue("ticker"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("cache-control", "public, max-age=60")
	writeJSON(w, http.StatusOK, v)
}

// market answers where the named assets closed and how far they have moved.
// Closes, not ticks: every figure is as of the last close, and asOf says which.
func (s *Server) market(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.Market(r.Context(), commaList(r.URL.Query().Get("tickers")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The running year's closes are cached 15 minutes; there is nothing fresher
	// to be had, so a minute on the edge costs nothing and spares the origin.
	w.Header().Set("cache-control", "public, max-age=60")
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) assetCloses(w http.ResponseWriter, r *http.Request) {
	year, err := strconv.Atoi(r.URL.Query().Get("year"))
	if err != nil {
		s.fail(w, r, &service.Error{Status: http.StatusBadRequest, Code: "BAD_YEAR", Message: "year is required, e.g. ?year=2026"})
		return
	}
	v, err := s.svc.AssetCloses(r.Context(), r.PathValue("ticker"), year)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// A past year is final; the current one grows after each close.
	if year < time.Now().UTC().Year() {
		w.Header().Set("cache-control", "public, max-age=86400")
	} else {
		w.Header().Set("cache-control", "public, max-age=300")
	}
	writeJSON(w, http.StatusOK, v)
}

/* ── Investing ──────────────────────────────────────────────────────── */

func (s *Server) prepare(w http.ResponseWriter, r *http.Request) {
	var req service.InvestRequest
	if !s.decode(w, r, &req) {
		return
	}
	res, err := s.svc.PrepareInvest(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	var req service.ConfirmRequest
	if !s.decode(w, r, &req) {
		return
	}
	res, err := s.svc.ConfirmInvest(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

/* ── Portfolios ─────────────────────────────────────────────────────── */

func (s *Server) portfolios(w http.ResponseWriter, r *http.Request) {
	// ?archived=1 asks for the ones this wallet retired instead of the ones it
	// is running. They are separate lists, never mixed.
	q := r.URL.Query()
	res, err := s.svc.ListPortfolios(r.Context(), q.Get("owner"), q.Get("archived") == "1")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"portfolios": res})
}

func (s *Server) portfolio(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Portfolio(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) positions(w http.ResponseWriter, r *http.Request) {
	raw, err := s.svc.Positions(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, raw)
}

// since reads the optional since=YYYY-MM-DD a client sends when it already
// holds a series up to that day. It answers 400 itself when the date is bad.
func (s *Server) since(w http.ResponseWriter, r *http.Request) (string, bool) {
	since := r.URL.Query().Get("since")
	if since == "" {
		return "", true
	}
	if _, err := time.Parse(time.DateOnly, since); err != nil {
		s.fail(w, r, &service.Error{Status: http.StatusBadRequest, Code: "BAD_REQUEST", Message: "since must be a date as YYYY-MM-DD."})
		return "", false
	}
	return since, true
}

func (s *Server) performance(w http.ResponseWriter, r *http.Request) {
	since, ok := s.since(w, r)
	if !ok {
		return
	}
	v, err := s.svc.Performance(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := *v
	if since != "" {
		out = out.Since(since)
	}
	w.Header().Set("cache-control", "private, max-age=30")
	writeJSON(w, http.StatusOK, out)
}

// activity answers a portfolio's allocation changes, newest first. One by
// default — ?limit=N asks for more, up to the service's own cap.
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	v, err := s.svc.Activity(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("cache-control", "private, max-age=60")
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) operation(w http.ResponseWriter, r *http.Request) {
	raw, err := s.svc.Operation(r.Context(), r.PathValue("id"), r.PathValue("opId"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, raw)
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	op, err := s.svc.Deploy(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

func (s *Server) withdrawSignature(w http.ResponseWriter, r *http.Request) {
	var req service.WithdrawRequest
	if !s.decode(w, r, &req) {
		return
	}
	raw, err := s.svc.WithdrawSignature(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, raw)
}

func (s *Server) withdraw(w http.ResponseWriter, r *http.Request) {
	var req service.WithdrawSubmit
	if !s.decode(w, r, &req) {
		return
	}
	op, err := s.svc.Withdraw(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	var req service.ArchiveRequest
	if !s.decode(w, r, &req) {
		return
	}
	if err := s.svc.Archive(r.Context(), r.PathValue("id"), req); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"archived": true})
}

/* ── Admin ──────────────────────────────────────────────────────────── */

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("authorization"), "Bearer ")
		if s.cfg.AdminToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AdminToken)) != 1 {
			writeJSON(w, http.StatusUnauthorized, apiError{Error: "admin token required", Code: "UNAUTHORIZED"})
			return
		}
		next(w, r)
	}
}

func (s *Server) reevaluate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sched.RunOnce(r.Context(), "manual"))
}

func (s *Server) evaluations(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	res, err := s.svc.RecentEvaluations(r.Context(), min(max(limit, 1), 500))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evaluations": res})
}

/* ── Encoding and errors ────────────────────────────────────────────── */

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, status int, raw json.RawMessage) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	_, _ = w.Write(raw)
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid JSON body: " + err.Error(), Code: "BAD_REQUEST"})
		return false
	}
	return true
}

// fail maps every error to a status and a stable code. Upstream messages are
// passed through verbatim: a hidden error is how a wrong allocation or a
// stuck withdrawal goes unnoticed.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := http.StatusInternalServerError, "INTERNAL", "Internal error."

	var rl *rateLimited
	var se *service.Error
	var ge *portfolioengine.Error
	var ce *backtestengine.Error
	var gate *strategy.GateError
	var pe *prices.Error
	switch {
	case errors.As(err, &rl):
		writeRateLimited(w, rl.wait)
		return
	case errors.As(err, &se):
		status, code, msg = se.Status, se.Code, se.Message
	case errors.As(err, &gate):
		status, code, msg = http.StatusConflict, "TARGET_NOT_EXECUTABLE", gate.Reason
	case errors.Is(err, backtestengine.ErrNotConfigured):
		status, code, msg = http.StatusServiceUnavailable, "RESEARCH_UNAVAILABLE", "Strategy research is not configured on this server."
	case errors.Is(err, portfolioengine.ErrNotConfigured):
		status, code, msg = http.StatusServiceUnavailable, "TRADING_UNAVAILABLE", "Trading is not configured on this server."
	case errors.As(err, &ge):
		// Upstream reason verbatim, without the vendor/method/path prefix —
		// that goes to the log, not to the user.
		code, msg = ge.Code, ge.Message
		if len(ge.Details) > 0 {
			msg += ": " + strings.Join(ge.Details, "; ")
		}
		if code == "" {
			code = "TRADING_ERROR"
		}
		if ge.Status >= 500 || ge.Status == http.StatusUnauthorized || ge.Status == http.StatusForbidden {
			s.log.Error("trading upstream failed", "method", r.Method, "path", r.URL.Path, "err", err)
		}
		switch {
		case ge.Status == http.StatusTooManyRequests:
			status = http.StatusTooManyRequests
			if ge.RetryAfter > 0 {
				w.Header().Set("retry-after", strconv.Itoa(int(ge.RetryAfter.Seconds())))
			}
		case ge.Status == http.StatusUnauthorized || ge.Status == http.StatusForbidden:
			// Our credential, not the user's request.
			status = http.StatusBadGateway
		case ge.Status >= 400 && ge.Status < 500:
			status = ge.Status
		default:
			status = http.StatusBadGateway
		}
	case errors.As(err, &pe):
		status, code, msg = http.StatusBadGateway, "PRICES_ERROR", "Price data is temporarily unavailable. Try again in a moment."
	case errors.As(err, &ce):
		// The research provider is an implementation detail; its raw error
		// is logged below (status >= 500), never shown.
		status, code, msg = http.StatusBadGateway, "RESEARCH_ERROR", "Strategy data is temporarily unavailable. Try again in a moment."
		if ce.Status == http.StatusGatewayTimeout {
			status = http.StatusGatewayTimeout
		}
	case errors.Is(err, context.Canceled):
		return // client went away
	case errors.Is(err, context.DeadlineExceeded):
		status, code, msg = http.StatusGatewayTimeout, "TIMEOUT", "Upstream took too long to answer."
	}
	if status >= 500 {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "status", status, "err", err)
	}
	writeJSON(w, status, apiError{Error: msg, Code: code})
}
