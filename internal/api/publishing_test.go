package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/auth"
	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/strategy"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

const loginAuthor = "0x19e7e376e7c213b7e7e7e46cc70a5dd086daff2a"
const loginOrigin = "https://studio.brighter.fi"

type memoryPublishing struct {
	mu           sync.Mutex
	challenges   map[string]store.AuthChallenge
	sessions     map[string]store.AuthSession
	publications map[string]store.PublishedStrategy
}

func newMemoryPublishing() *memoryPublishing {
	return &memoryPublishing{challenges: map[string]store.AuthChallenge{}, sessions: map[string]store.AuthSession{}, publications: map[string]store.PublishedStrategy{}}
}
func (m *memoryPublishing) CreateAuthChallenge(_ context.Context, c store.AuthChallenge) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.challenges[c.ID] = c
	return nil
}
func (m *memoryPublishing) AuthChallenge(_ context.Context, id, origin string) (*store.AuthChallenge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.challenges[id]
	if !ok || c.Origin != origin || !c.ExpiresAt.After(time.Now()) {
		return nil, store.ErrNotFound
	}
	return &c, nil
}
func (m *memoryPublishing) CompleteAuthChallenge(_ context.Context, id string, s store.AuthSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.challenges[id]
	if !ok || c.Author != s.Author || c.Origin != s.Origin || !c.ExpiresAt.After(time.Now()) {
		return store.ErrNotFound
	}
	delete(m.challenges, id)
	m.sessions[s.TokenHash] = s
	return nil
}
func (m *memoryPublishing) AuthSession(_ context.Context, hash, origin string) (*store.AuthSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[hash]
	if !ok || s.Origin != origin || !s.ExpiresAt.After(time.Now()) {
		return nil, store.ErrNotFound
	}
	return &s, nil
}
func (m *memoryPublishing) RevokeAuthSession(_ context.Context, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, hash)
	return nil
}
func (m *memoryPublishing) PublicationCount(_ context.Context, author string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, p := range m.publications {
		if p.Author == author {
			count++
		}
	}
	return count, nil
}
func (m *memoryPublishing) PublishStrategy(_ context.Context, p *store.PublishedStrategy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, existing := range m.publications {
		if existing.RulesKey == p.RulesKey {
			return store.ErrDuplicateStrategy
		}
		if existing.Author == p.Author {
			count++
		}
	}
	if count >= 10 {
		return store.ErrPublicationLimit
	}
	p.PublishedAt = time.Now().UTC()
	m.publications[p.ID] = *p
	return nil
}
func publishingServer(t *testing.T) (*Server, *memoryPublishing) {
	s, _ := newTestServer(t)
	s.cfg.CORSOrigins = append(s.cfg.CORSOrigins, loginOrigin)
	s.authLimit = newIPLimiter(1000, 1000)
	m := newMemoryPublishing()
	s.publishing = m
	return s, m
}
func publishingRequest(s *Server, method, path, origin, token string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Origin", origin)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}
func loginSignature(message string) string {
	key := secp256k1.PrivKeyFromBytes(bytes.Repeat([]byte{0x11}, 32))
	compact := ecdsa.SignCompact(key, auth.MessageHash(message), false)
	signature := append(compact[1:], compact[0])
	return "0x" + hex.EncodeToString(signature)
}
func seedLogin(m *memoryPublishing, author, token string) {
	m.sessions[sessionHash(token)] = store.AuthSession{TokenHash: sessionHash(token), Author: author, Origin: loginOrigin, ExpiresAt: time.Now().Add(time.Hour)}
}
func TestSignedLoginAndReplayProtection(t *testing.T) {
	s, m := publishingServer(t)
	w := publishingRequest(s, "POST", "/v1/auth/challenge", loginOrigin, "", map[string]any{"address": loginAuthor, "chainId": 56})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var c struct{ ChallengeID, Message string }
	json.Unmarshal(w.Body.Bytes(), &c)
	signed := map[string]string{"challengeId": c.ChallengeID, "signature": loginSignature(c.Message)}
	w = publishingRequest(s, "POST", "/v1/auth/verify", loginOrigin, "", signed)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var session struct{ Token, Author string }
	json.Unmarshal(w.Body.Bytes(), &session)
	if session.Author != loginAuthor || len(session.Token) != 64 || len(m.sessions) != 1 {
		t.Fatal("missing authenticated author")
	}
	if _, ok := m.sessions[session.Token]; ok {
		t.Fatal("plaintext token stored")
	}
	if w = publishingRequest(s, "POST", "/v1/auth/verify", loginOrigin, "", signed); w.Code != 401 {
		t.Fatal("accepted replay", w.Code)
	}
	if w = publishingRequest(s, "GET", "/v1/auth/me", loginOrigin, session.Token, nil); w.Code != 200 {
		t.Fatal("session failed", w.Code)
	}
	if w = publishingRequest(s, "GET", "/v1/auth/me", "https://brighter.fi", session.Token, nil); w.Code != 401 {
		t.Fatal("cross-origin session", w.Code)
	}
	if w = publishingRequest(s, "POST", "/v1/auth/logout", loginOrigin, session.Token, map[string]any{}); w.Code != 204 {
		t.Fatal("logout failed")
	}
	if w = publishingRequest(s, "GET", "/v1/auth/me", loginOrigin, session.Token, nil); w.Code != 401 {
		t.Fatal("revoked session accepted")
	}
}
func TestForgedExpiredAndForeignLogins(t *testing.T) {
	s, m := publishingServer(t)
	if w := publishingRequest(s, "POST", "/v1/auth/challenge", "https://evil.example", "", map[string]any{"address": loginAuthor, "chainId": 56}); w.Code != 403 {
		t.Fatal("foreign login")
	}
	c := store.AuthChallenge{ID: "challenge", Author: loginAuthor, Origin: loginOrigin, Message: "real server challenge", ExpiresAt: time.Now().Add(time.Minute)}
	m.challenges[c.ID] = c
	for _, signature := range []string{"0x00", loginSignature("forged message")} {
		if w := publishingRequest(s, "POST", "/v1/auth/verify", loginOrigin, "", map[string]string{"challengeId": c.ID, "signature": signature}); w.Code != 401 {
			t.Fatal("accepted forgery")
		}
	}
	c.ExpiresAt = time.Now().Add(-time.Minute)
	m.challenges[c.ID] = c
	if w := publishingRequest(s, "POST", "/v1/auth/verify", loginOrigin, "", map[string]string{"challengeId": c.ID, "signature": loginSignature(c.Message)}); w.Code != 401 {
		t.Fatal("accepted expired proof")
	}
}
func TestPublishRequiresLoginAndDerivesAuthor(t *testing.T) {
	s, m := publishingServer(t)
	body := map[string]any{"author": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "strategy": strategy.CustomStrategy{Name: "Mine", Rebalance: "monthly", Benchmark: "SPY", Children: []strategy.CustomNode{{Kind: "asset", Ticker: "QCOM"}}}}
	for _, token := range []string{"", strings.Repeat("f", 64)} {
		w := publishingRequest(s, "POST", "/v1/strategies", loginOrigin, token, body)
		if w.Code != 401 {
			t.Fatal("published without login", w.Code)
		}
	}
	token := strings.Repeat("a", 64)
	seedLogin(m, loginAuthor, token)
	w := publishingRequest(s, "POST", "/v1/strategies", loginOrigin, token, body)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var published struct{ ID, Author, URL string }
	json.Unmarshal(w.Body.Bytes(), &published)
	if published.Author != loginAuthor || !strings.HasPrefix(published.ID, "studio-") || published.URL != "https://brighter.fi/strategies/"+published.ID {
		t.Fatal("incorrect author or public url")
	}
	if len(m.publications) != 1 {
		t.Fatal("not persisted")
	}
	body["strategy"] = strategy.CustomStrategy{Name: "Renamed", Rebalance: "monthly", Benchmark: "SPY", Children: []strategy.CustomNode{{Kind: "group", Name: "Different label", Children: []strategy.CustomNode{{Kind: "asset", Ticker: "QCOMon"}}}}}
	other := strings.Repeat("b", 64)
	seedLogin(m, "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", other)
	w = publishingRequest(s, "POST", "/v1/strategies", loginOrigin, other, body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "STRATEGY_EXISTS") {
		t.Fatal("duplicate published", w.Code, w.Body.String())
	}
}
func TestPublishLimitAndPresetDuplicates(t *testing.T) {
	s, m := publishingServer(t)
	token := strings.Repeat("a", 64)
	seedLogin(m, loginAuthor, token)
	for i := 0; i < 11; i++ {
		c := strategy.CustomStrategy{Name: "Unique rules", Rebalance: "monthly", Benchmark: "SPY", Children: []strategy.CustomNode{{Kind: "condition", Metric: "rsi", Ticker: "SPY", Lookback: 14, Comparator: "gt", CompareMode: "value", Value: float64(i), ThenChildren: []strategy.CustomNode{{Kind: "asset", Ticker: "QCOM"}}, ElseChildren: []strategy.CustomNode{{Kind: "asset", Ticker: "BIL"}}}}}
		w := publishingRequest(s, "POST", "/v1/strategies", loginOrigin, token, strategy.Publication{Strategy: c})
		if i < 10 && w.Code != 201 {
			t.Fatal(i, w.Code, w.Body.String())
		}
		if i == 10 && (w.Code != 409 || !strings.Contains(w.Body.String(), "STRATEGY_LIMIT")) {
			t.Fatal("quota ignored", w.Code, w.Body.String())
		}
	}
	count, _ := m.PublicationCount(context.Background(), loginAuthor)
	if count != 10 {
		t.Fatal(count)
	}
	c := strategy.CustomStrategy{Name: "Renamed built-in", Rebalance: "daily", Benchmark: "SPY", Children: []strategy.CustomNode{{Kind: "condition", Metric: "rsi", Ticker: "SPY", Lookback: 14, Comparator: "gt", CompareMode: "value", Value: 70, ThenChildren: []strategy.CustomNode{{Kind: "asset", Ticker: "BIL"}}, ElseChildren: []strategy.CustomNode{{Kind: "weight", Method: "equal"}}}}}
	for _, ticker := range strategy.Mag7 {
		c.Children[0].ElseChildren[0].Children = append(c.Children[0].ElseChildren[0].Children, strategy.CustomNode{Kind: "asset", Ticker: ticker})
	}
	w := publishingRequest(s, "POST", "/v1/strategies", loginOrigin, token, strategy.Publication{Strategy: c})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "STRATEGY_EXISTS") {
		t.Fatal("preset duplicate", w.Code, w.Body.String())
	}
}
