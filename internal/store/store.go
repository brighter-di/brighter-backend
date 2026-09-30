// Package store is the Postgres persistence layer.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

var ErrNotFound = errors.New("not found")

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if cfg.MaxConns < 4 {
		cfg.MaxConns = 4
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies embedded migrations in order, once, under an advisory lock
// so two instances starting together cannot race.
func (s *Store) Migrate(ctx context.Context) error {
	return s.WithLock(ctx, "brighter:migrate", func(ctx context.Context) error {
		if _, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INT PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		entries, err := fs.ReadDir(migrations, "migrations")
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			prefix, _, _ := strings.Cut(e.Name(), "_")
			version, err := strconv.Atoi(prefix)
			if err != nil {
				return fmt.Errorf("migration %s: name must start with a number", e.Name())
			}
			var applied bool
			if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
				return err
			}
			if applied {
				continue
			}
			sql, err := migrations.ReadFile("migrations/" + e.Name())
			if err != nil {
				return err
			}
			err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, string(sql)); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, version, e.Name())
				return err
			})
			if err != nil {
				return fmt.Errorf("migration %s: %w", e.Name(), err)
			}
		}
		return nil
	})
}

// WithLock runs fn while holding a session-level advisory lock on key. The
// lock lives on one pooled connection for the duration.
func (s *Store) WithLock(ctx context.Context, key string, fn func(ctx context.Context) error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, key); err != nil {
		return err
	}
	defer func() {
		// Unlock on a fresh context: the caller's may already be cancelled,
		// and a lock left behind on a pooled connection would outlive us.
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
			conn.Conn().Close(uctx)
		}
	}()
	return fn(ctx)
}

// TryLock is WithLock that returns (false, nil) immediately when another
// session holds the lock.
func (s *Store) TryLock(ctx context.Context, key string, fn func(ctx context.Context) error) (bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&got); err != nil {
		return false, err
	}
	if !got {
		return false, nil
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
			conn.Conn().Close(uctx)
		}
	}()
	return true, fn(ctx)
}

/* ── Strategies ─────────────────────────────────────────────────────── */

type Strategy struct {
	ID                        int64
	TemplateID                string
	ParamsKey                 string
	Params                    map[string]float64
	ChainID                   int64
	PortfolioEngineStrategyID string
	Allocation                map[string]int // basis points by model ticker
	AllocationHash            string
	Cutoff                    time.Time
	Version                   int
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

const strategyCols = `id, template_id, params_key, params, chain_id, portfolio_engine_strategy_id,
	allocation, allocation_hash, cutoff, version, created_at, updated_at`

func scanStrategy(row pgx.Row) (*Strategy, error) {
	var st Strategy
	var params, alloc []byte
	err := row.Scan(&st.ID, &st.TemplateID, &st.ParamsKey, &params, &st.ChainID, &st.PortfolioEngineStrategyID,
		&alloc, &st.AllocationHash, &st.Cutoff, &st.Version, &st.CreatedAt, &st.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(params, &st.Params); err != nil {
		return nil, fmt.Errorf("strategy %d params: %w", st.ID, err)
	}
	if err := json.Unmarshal(alloc, &st.Allocation); err != nil {
		return nil, fmt.Errorf("strategy %d allocation: %w", st.ID, err)
	}
	return &st, nil
}

func (s *Store) GetStrategy(ctx context.Context, templateID, paramsKey string, chainID int64) (*Strategy, error) {
	return scanStrategy(s.pool.QueryRow(ctx, `SELECT `+strategyCols+` FROM strategies
		WHERE template_id = $1 AND params_key = $2 AND chain_id = $3`, templateID, paramsKey, chainID))
}

func (s *Store) GetStrategyByPortfolioEngineID(ctx context.Context, portfolioengineID string) (*Strategy, error) {
	return scanStrategy(s.pool.QueryRow(ctx, `SELECT `+strategyCols+` FROM strategies
		WHERE portfolio_engine_strategy_id = $1`, portfolioengineID))
}

func (s *Store) InsertStrategy(ctx context.Context, st *Strategy) error {
	params, _ := json.Marshal(st.Params)
	alloc, _ := json.Marshal(st.Allocation)
	return s.pool.QueryRow(ctx, `INSERT INTO strategies
		(template_id, params_key, params, chain_id, portfolio_engine_strategy_id, allocation, allocation_hash, cutoff, version)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6::jsonb, $7, $8, $9)
		RETURNING id, created_at, updated_at`,
		st.TemplateID, st.ParamsKey, string(params), st.ChainID, st.PortfolioEngineStrategyID,
		string(alloc), st.AllocationHash, st.Cutoff, st.Version,
	).Scan(&st.ID, &st.CreatedAt, &st.UpdatedAt)
}

func (s *Store) UpdateStrategyAllocation(ctx context.Context, id int64, alloc map[string]int, hash string, cutoff time.Time, version int) error {
	b, _ := json.Marshal(alloc)
	tag, err := s.pool.Exec(ctx, `UPDATE strategies
		SET allocation = $2::jsonb, allocation_hash = $3, cutoff = $4, version = $5, updated_at = now()
		WHERE id = $1`, id, string(b), hash, cutoff, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Combo is a distinct (template, parameter set) with at least one strategy.
type Combo struct {
	TemplateID string
	ParamsKey  string
	Params     map[string]float64
}

func (s *Store) ListCombos(ctx context.Context) ([]Combo, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (template_id, params_key) template_id, params_key, params
		FROM strategies ORDER BY template_id, params_key, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Combo, error) {
		var c Combo
		var params []byte
		if err := r.Scan(&c.TemplateID, &c.ParamsKey, &params); err != nil {
			return c, err
		}
		return c, json.Unmarshal(params, &c.Params)
	})
}

func (s *Store) StrategiesForCombo(ctx context.Context, templateID, paramsKey string) ([]*Strategy, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+strategyCols+` FROM strategies
		WHERE template_id = $1 AND params_key = $2 ORDER BY chain_id`, templateID, paramsKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Strategy
	for rows.Next() {
		st, err := scanStrategy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

/* ── Portfolios ─────────────────────────────────────────────────────── */

type Portfolio struct {
	PortfolioID    string
	Owner          string
	ChainID        int64
	DepositAddress string
	CreatedAt      time.Time
	// Set when the owner archived it. The row and its history stay; only the
	// active lists drop it.
	DroppedAt *time.Time
	Strategy  *Strategy
}

const portfolioCols = `p.portfolio_id, p.owner, p.chain_id, p.deposit_address, p.created_at, p.dropped_at,
	s.id, s.template_id, s.params_key, s.params, s.chain_id, s.portfolio_engine_strategy_id,
	s.allocation, s.allocation_hash, s.cutoff, s.version, s.created_at, s.updated_at`

func scanPortfolio(row pgx.Row) (*Portfolio, error) {
	var p Portfolio
	var st Strategy
	var params, alloc []byte
	err := row.Scan(&p.PortfolioID, &p.Owner, &p.ChainID, &p.DepositAddress, &p.CreatedAt, &p.DroppedAt,
		&st.ID, &st.TemplateID, &st.ParamsKey, &params, &st.ChainID, &st.PortfolioEngineStrategyID,
		&alloc, &st.AllocationHash, &st.Cutoff, &st.Version, &st.CreatedAt, &st.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(params, &st.Params); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(alloc, &st.Allocation); err != nil {
		return nil, err
	}
	p.Strategy = &st
	return &p, nil
}

func (s *Store) GetPortfolio(ctx context.Context, portfolioID string) (*Portfolio, error) {
	return scanPortfolio(s.pool.QueryRow(ctx, `SELECT `+portfolioCols+`
		FROM portfolios p JOIN strategies s ON s.id = p.strategy_id
		WHERE p.portfolio_id = $1 AND p.dropped_at IS NULL`, portfolioID))
}

func (s *Store) FindPortfolio(ctx context.Context, owner string, strategyRowID int64) (*Portfolio, error) {
	return scanPortfolio(s.pool.QueryRow(ctx, `SELECT `+portfolioCols+`
		FROM portfolios p JOIN strategies s ON s.id = p.strategy_id
		WHERE p.owner = $1 AND p.strategy_id = $2 AND p.dropped_at IS NULL`, owner, strategyRowID))
}

// ListPortfolios returns an owner's portfolios, newest first. Archived ones are
// a separate list rather than a flag on one: they are read on their own tab,
// and mixing them would put a retired portfolio among the live ones for any
// client that has not been taught the difference.
func (s *Store) ListPortfolios(ctx context.Context, owner string, archived bool) ([]*Portfolio, error) {
	where := "p.dropped_at IS NULL"
	order := "p.created_at DESC"
	if archived {
		where = "p.dropped_at IS NOT NULL"
		// Most recently retired first: that is the one being looked for.
		order = "p.dropped_at DESC"
	}
	rows, err := s.pool.Query(ctx, `SELECT `+portfolioCols+`
		FROM portfolios p JOIN strategies s ON s.id = p.strategy_id
		WHERE p.owner = $1 AND `+where+`
		ORDER BY `+order, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Portfolio
	for rows.Next() {
		p, err := scanPortfolio(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// InsertPortfolio records an enrollment. Replaying the same portfolio id is a
// no-op, so a retried confirm cannot create a duplicate row.
func (s *Store) InsertPortfolio(ctx context.Context, portfolioID, owner string, strategyRowID, chainID int64, deposit string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO portfolios (portfolio_id, owner, strategy_id, chain_id, deposit_address)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (portfolio_id) DO NOTHING`,
		portfolioID, owner, strategyRowID, chainID, deposit)
	return err
}

func (s *Store) MarkPortfolioDropped(ctx context.Context, portfolioID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE portfolios SET dropped_at = now() WHERE portfolio_id = $1 AND dropped_at IS NULL`, portfolioID)
	return err
}

// RecordWallet notes that a wallet has used the app. It reports true only the
// first time.
func (s *Store) RecordWallet(ctx context.Context, address string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO wallets (address) VALUES ($1) ON CONFLICT DO NOTHING`, address)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

/* ── Telegram subscribers ───────────────────────────────────────────── */

// AddSubscriber records a chat that pressed Start on the bot; false when it
// was already subscribed.
func (s *Store) AddSubscriber(ctx context.Context, chatID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO telegram_subscribers (chat_id) VALUES ($1) ON CONFLICT DO NOTHING`, chatID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RemoveSubscriber forgets a chat that sent /stop or blocked the bot.
func (s *Store) RemoveSubscriber(ctx context.Context, chatID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM telegram_subscribers WHERE chat_id = $1`, chatID)
	return err
}

// Subscribers lists every subscribed chat, oldest first.
func (s *Store) Subscribers(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT chat_id FROM telegram_subscribers ORDER BY subscribed_at, chat_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

/* ── Daily closes ───────────────────────────────────────────────────── */

// Close is one trading day's closing price.
type Close struct {
	Day   string // YYYY-MM-DD
	Close float64
}

// SaveCloses stores one (ticker, year) of closes, replacing any stored value
// for the same day. complete marks the year as stored in full, which is only
// true once the year has ended.
func (s *Store) SaveCloses(ctx context.Context, ticker string, year int, closes []Close, complete bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for _, c := range closes {
			day, err := time.Parse("2006-01-02", c.Day)
			if err != nil {
				return fmt.Errorf("close for %s: %w", ticker, err)
			}
			batch.Queue(`INSERT INTO daily_closes (ticker, day, close) VALUES ($1, $2, $3)
				ON CONFLICT (ticker, day) DO UPDATE SET close = EXCLUDED.close`, ticker, day, c.Close)
		}
		if complete {
			batch.Queue(`INSERT INTO close_years (ticker, year) VALUES ($1, $2)
				ON CONFLICT (ticker, year) DO UPDATE SET fetched_at = now()`, ticker, year)
		}
		return tx.SendBatch(ctx, batch).Close()
	})
}

// LoadCloses returns one (ticker, year) of stored closes, oldest first, and
// whether that year is stored in full.
func (s *Store) LoadCloses(ctx context.Context, ticker string, year int) ([]Close, bool, error) {
	var complete bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM close_years WHERE ticker = $1 AND year = $2)`,
		ticker, year).Scan(&complete); err != nil {
		return nil, false, err
	}
	rows, err := s.pool.Query(ctx, `SELECT day, close FROM daily_closes
		WHERE ticker = $1 AND day >= make_date($2, 1, 1) AND day < make_date($2 + 1, 1, 1)
		ORDER BY day`, ticker, year)
	if err != nil {
		return nil, false, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Close, error) {
		var c Close
		var day time.Time
		if err := r.Scan(&day, &c.Close); err != nil {
			return c, err
		}
		c.Day = day.Format("2006-01-02")
		return c, nil
	})
	return out, complete, err
}

/* ── Evaluations ────────────────────────────────────────────────────── */

type Evaluation struct {
	ID          int64
	TemplateID  string
	ParamsKey   string
	Trigger     string
	Status      string
	Cutoff      *time.Time
	Weights     map[string]int
	ChangeHash  string
	Reason      string
	EvaluatedAt time.Time
}

func (s *Store) InsertEvaluation(ctx context.Context, e *Evaluation) error {
	var weights any
	if e.Weights != nil {
		b, _ := json.Marshal(e.Weights)
		weights = string(b)
	}
	return s.pool.QueryRow(ctx, `INSERT INTO evaluations
		(template_id, params_key, trigger, status, cutoff, weights, change_hash, reason)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, NULLIF($7, ''), NULLIF($8, ''))
		RETURNING id, evaluated_at`,
		e.TemplateID, e.ParamsKey, e.Trigger, e.Status, e.Cutoff, weights, e.ChangeHash, e.Reason,
	).Scan(&e.ID, &e.EvaluatedAt)
}

// PruneEvaluations deletes routine evaluations (held, error, unchanged) made
// before the given time. Published rows are the record of every allocation
// that reached the portfolio engine and are kept, as is each combo's newest accepted row,
// which LastAcceptedCutoff depends on.
func (s *Store) PruneEvaluations(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM evaluations
		WHERE evaluated_at < $1
		  AND status IN ('held', 'error', 'unchanged')
		  AND id NOT IN (
		    SELECT DISTINCT ON (template_id, params_key) id FROM evaluations
		    WHERE status IN ('published', 'unchanged')
		    ORDER BY template_id, params_key, cutoff DESC NULLS LAST, id DESC)`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// LastAcceptedCutoff is the newest data cutoff a combo was published or
// confirmed unchanged at. Nil when the combo has never been accepted.
func (s *Store) LastAcceptedCutoff(ctx context.Context, templateID, paramsKey string) (*time.Time, error) {
	var c *time.Time
	err := s.pool.QueryRow(ctx, `SELECT max(cutoff) FROM evaluations
		WHERE template_id = $1 AND params_key = $2 AND status IN ('published', 'unchanged')`,
		templateID, paramsKey).Scan(&c)
	return c, err
}

// EvaluationsSince counts a combo's scheduled evaluations since t, and whether
// any of them settled it (published or unchanged).
func (s *Store) EvaluationsSince(ctx context.Context, templateID, paramsKey string, since time.Time) (attempts int, settled bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT count(*), coalesce(bool_or(status IN ('published', 'unchanged')), false)
		FROM evaluations WHERE template_id = $1 AND params_key = $2 AND trigger = 'schedule' AND evaluated_at >= $3`,
		templateID, paramsKey, since).Scan(&attempts, &settled)
	return
}

const evaluationColumns = `id, template_id, params_key, trigger, status, cutoff, weights,
	coalesce(change_hash, ''), coalesce(reason, ''), evaluated_at`

func (s *Store) RecentEvaluations(ctx context.Context, limit int) ([]*Evaluation, error) {
	return s.queryEvaluations(ctx, `SELECT `+evaluationColumns+` FROM evaluations
		ORDER BY evaluated_at DESC LIMIT $1`, limit)
}

// PublishedEvaluations is every allocation published for a combo, oldest
// first. Published rows are never pruned, so this is the combo's full history.
func (s *Store) PublishedEvaluations(ctx context.Context, templateID, paramsKey string) ([]*Evaluation, error) {
	return s.queryEvaluations(ctx, `SELECT `+evaluationColumns+` FROM evaluations
		WHERE template_id = $1 AND params_key = $2 AND status = 'published'
		ORDER BY evaluated_at, id`, templateID, paramsKey)
}

func (s *Store) queryEvaluations(ctx context.Context, sql string, args ...any) ([]*Evaluation, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Evaluation
	for rows.Next() {
		var e Evaluation
		var weights []byte
		if err := rows.Scan(&e.ID, &e.TemplateID, &e.ParamsKey, &e.Trigger, &e.Status, &e.Cutoff, &weights,
			&e.ChangeHash, &e.Reason, &e.EvaluatedAt); err != nil {
			return nil, err
		}
		if len(weights) > 0 {
			if err := json.Unmarshal(weights, &e.Weights); err != nil {
				return nil, err
			}
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
