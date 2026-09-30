package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/brighter-di/backend/internal/auth"
	"github.com/brighter-di/backend/internal/service"
	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/strategy"
	"github.com/brighter-di/backend/internal/tokens"
)

type publishingRepository interface {
	CreateAuthChallenge(context.Context, store.AuthChallenge) error
	AuthChallenge(context.Context, string, string) (*store.AuthChallenge, error)
	CompleteAuthChallenge(context.Context, string, store.AuthSession) error
	AuthSession(context.Context, string, string) (*store.AuthSession, error)
	RevokeAuthSession(context.Context, string) error
	PublicationCount(context.Context, string) (int, error)
	PublishStrategy(context.Context, *store.PublishedStrategy) error
}

func randomToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
func sessionHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
func (s *Server) authOrigin(r *http.Request) (string, error) {
	origin := strings.TrimRight(r.Header.Get("Origin"), "/")
	if origin == "" || !slices.Contains(s.cfg.CORSOrigins, origin) {
		return "", &service.Error{Status: 403, Code: "LOGIN_ORIGIN", Message: "Sign in from Brighter Studio."}
	}
	return origin, nil
}
func authError() error {
	return &service.Error{Status: 401, Code: "LOGIN_REQUIRED", Message: "Sign in with your wallet to publish a strategy."}
}
func (s *Server) authChallenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("cache-control", "no-store")
	origin, err := s.authOrigin(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		Address string `json:"address"`
		ChainID int64  `json:"chainId"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	author, err := auth.NormalizeAddress(body.Address)
	if err != nil {
		s.fail(w, r, &service.Error{Status: 400, Code: "BAD_ADDRESS", Message: err.Error()})
		return
	}
	if _, ok := tokens.ChainByID(body.ChainID); !ok {
		s.fail(w, r, &service.Error{Status: 400, Code: "UNSUPPORTED_CHAIN", Message: "Sign in on Ethereum or BNB Chain."})
		return
	}
	nonce, err := randomToken()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	message, err := auth.Message(origin, author, nonce, body.ChainID, now, now.Add(24*time.Hour))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	challenge := store.AuthChallenge{ID: nonce, Author: author, Origin: origin, Message: message, ExpiresAt: now.Add(5 * time.Minute)}
	if err = s.publishing.CreateAuthChallenge(r.Context(), challenge); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"challengeId": nonce, "message": message, "expiresAt": challenge.ExpiresAt})
}
func (s *Server) authVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("cache-control", "no-store")
	origin, err := s.authOrigin(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		ChallengeID string `json:"challengeId"`
		Signature   string `json:"signature"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	c, err := s.publishing.AuthChallenge(r.Context(), body.ChallengeID, origin)
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, authError())
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err = auth.Verify(c.Message, body.Signature, c.Author); err != nil {
		s.fail(w, r, &service.Error{Status: 401, Code: "BAD_SIGNATURE", Message: err.Error()})
		return
	}
	token, err := randomToken()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	session := store.AuthSession{TokenHash: sessionHash(token), Author: c.Author, Origin: origin, ExpiresAt: c.ExpiresAt.Add(24*time.Hour - 5*time.Minute)}
	if err = s.publishing.CompleteAuthChallenge(r.Context(), c.ID, session); errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, authError())
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "author": session.Author, "expiresAt": session.ExpiresAt})
}
func (s *Server) authenticated(r *http.Request) (*store.AuthSession, error) {
	origin, err := s.authOrigin(r)
	if err != nil {
		return nil, err
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) != 71 {
		return nil, authError()
	}
	session, err := s.publishing.AuthSession(r.Context(), sessionHash(header[7:]), origin)
	if errors.Is(err, store.ErrNotFound) {
		return nil, authError()
	}
	return session, err
}
func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("cache-control", "no-store")
	session, err := s.authenticated(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	count, err := s.publishing.PublicationCount(r.Context(), session.Author)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"author": session.Author, "expiresAt": session.ExpiresAt, "publishedCount": count, "limit": 10})
}
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("cache-control", "no-store")
	session, err := s.authenticated(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err = s.publishing.RevokeAuthSession(r.Context(), session.TokenHash); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) publishStrategy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("cache-control", "no-store")
	session, err := s.authenticated(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body strategy.Publication
	if !s.decode(w, r, &body) {
		return
	}
	if err = body.Validate(); err != nil {
		s.fail(w, r, &service.Error{Status: 422, Code: "INVALID_STRATEGY", Message: err.Error()})
		return
	}
	compiled, err := strategy.CompileCustom(body.Strategy)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Presets and community strategies both reserve their default rules.
	for _, preset := range strategy.Templates {
		tree := preset.Tree(preset.Defaults())
		if strategy.SameTreeRules(tree, compiled.Tree) {
			s.fail(w, r, &service.Error{Status: 409, Code: "STRATEGY_EXISTS", Message: "This strategy is already published. Use the existing strategy."})
			return
		}
	}
	id, err := randomToken()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	raw, err := json.Marshal(body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	publication := &store.PublishedStrategy{ID: "studio-" + id[:24], Author: session.Author, RulesKey: strategy.RulesFingerprint(compiled.Tree), Document: raw}
	err = s.publishing.PublishStrategy(r.Context(), publication)
	if errors.Is(err, store.ErrPublicationLimit) {
		err = &service.Error{Status: 409, Code: "STRATEGY_LIMIT", Message: "You can publish up to 10 strategies."}
	}
	if errors.Is(err, store.ErrDuplicateStrategy) {
		err = &service.Error{Status: 409, Code: "STRATEGY_EXISTS", Message: "This strategy is already published. Use the existing strategy."}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": publication.ID, "author": publication.Author, "publishedAt": publication.PublishedAt, "url": "https://brighter.fi/strategies/" + publication.ID})
}
