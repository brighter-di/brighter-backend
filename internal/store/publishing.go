package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrPublicationLimit = errors.New("published strategy limit reached")
var ErrDuplicateStrategy = errors.New("strategy already published")

type AuthChallenge struct {
	ID, Author, Origin, Message string
	ExpiresAt                   time.Time
}
type AuthSession struct {
	TokenHash, Author, Origin string
	ExpiresAt                 time.Time
}
type PublishedStrategy struct {
	ID          string          `json:"id"`
	Author      string          `json:"author"`
	RulesKey    string          `json:"-"`
	Document    json.RawMessage `json:"-"`
	PublishedAt time.Time       `json:"publishedAt"`
}

func (s *Store) CreateAuthChallenge(ctx context.Context, c AuthChallenge) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO auth_challenges(id,author,origin,message,expires_at) VALUES($1,$2,$3,$4,$5)`, c.ID, c.Author, c.Origin, c.Message, c.ExpiresAt)
	return err
}
func (s *Store) AuthChallenge(ctx context.Context, id, origin string) (*AuthChallenge, error) {
	c := &AuthChallenge{}
	err := s.pool.QueryRow(ctx, `SELECT id,author,origin,message,expires_at FROM auth_challenges WHERE id=$1 AND origin=$2 AND used_at IS NULL AND expires_at>now()`, id, origin).Scan(&c.ID, &c.Author, &c.Origin, &c.Message, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// Challenge consumption and session creation commit together; a replay cannot mint a session.
func (s *Store) CompleteAuthChallenge(ctx context.Context, id string, session AuthSession) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE auth_challenges SET used_at=now() WHERE id=$1 AND author=$2 AND origin=$3 AND used_at IS NULL AND expires_at>now()`, id, session.Author, session.Origin)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrNotFound
	}
	_, err = tx.Exec(ctx, `INSERT INTO auth_sessions(token_hash,author,origin,expires_at) VALUES($1,$2,$3,$4)`, session.TokenHash, session.Author, session.Origin, session.ExpiresAt)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) AuthSession(ctx context.Context, hash, origin string) (*AuthSession, error) {
	session := &AuthSession{}
	err := s.pool.QueryRow(ctx, `SELECT token_hash,author,origin,expires_at FROM auth_sessions WHERE token_hash=$1 AND origin=$2 AND expires_at>now()`, hash, origin).Scan(&session.TokenHash, &session.Author, &session.Origin, &session.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return session, err
}
func (s *Store) RevokeAuthSession(ctx context.Context, hash string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM auth_sessions WHERE token_hash=$1`, hash)
	return err
}
func (s *Store) PublicationCount(ctx context.Context, author string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM published_strategies WHERE author=$1`, author).Scan(&count)
	return count, err
}
func (s *Store) PublishStrategy(ctx context.Context, p *PublishedStrategy) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 711))`, p.Author); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM published_strategies WHERE rules_key=$1)`, p.RulesKey).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrDuplicateStrategy
	}
	var slot int
	err = tx.QueryRow(ctx, `SELECT n FROM generate_series(1,10) n WHERE NOT EXISTS (SELECT 1 FROM published_strategies WHERE author=$1 AND author_slot=n) ORDER BY n LIMIT 1`, p.Author).Scan(&slot)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPublicationLimit
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `INSERT INTO published_strategies(id,author,author_slot,rules_key,document) VALUES($1,$2,$3,$4,$5) RETURNING published_at`, p.ID, p.Author, slot, p.RulesKey, p.Document).Scan(&p.PublishedAt)
	var constraint *pgconn.PgError
	if errors.As(err, &constraint) && constraint.Code == "23505" && constraint.ConstraintName == "published_strategies_rules_key_key" {
		return ErrDuplicateStrategy
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) PublishedStrategy(ctx context.Context, id string) (*PublishedStrategy, error) {
	p := &PublishedStrategy{}
	err := s.pool.QueryRow(ctx, `SELECT id,author,rules_key,document,published_at FROM published_strategies WHERE id=$1`, id).Scan(&p.ID, &p.Author, &p.RulesKey, &p.Document, &p.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}
func (s *Store) PublishedStrategies(ctx context.Context) ([]PublishedStrategy, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,author,rules_key,document,published_at FROM published_strategies ORDER BY published_at DESC,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PublishedStrategy, error) {
		var p PublishedStrategy
		err := r.Scan(&p.ID, &p.Author, &p.RulesKey, &p.Document, &p.PublishedAt)
		return p, err
	})
}
