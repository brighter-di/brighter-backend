package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/brighter-di/backend/internal/mcp"
)

// The Model Context Protocol endpoint, served for mcp.brighter.fi.
//
// It offers the public catalogue and the Studio authoring loop — browse,
// write rules, check them, backtest them — and nothing else. No portfolio, no
// wallet, no write. That is not an oversight: a portfolio is identified by an
// unguessable id rather than by proving ownership, so exposing one here would
// turn any id repeated into a chat transcript into a live capability.

// mcpHandler builds the endpoint for this server, with its own admission
// budget, so a model's backtest loop is charged to the MCP's limits and never
// to the website's. Built per server rather than once per process: a package
// level Once would hand every later server the first one's service.
func (s *Server) mcpHandler() http.Handler {
	if s.mcp == nil {
		s.mcp = mcp.New(s.svc, s.admitMCP, s.log)
	}
	return s.mcp
}

// admitMCP charges every call against the per-client budget, and only a call
// that can reach the backtest engine against the shared one. Reading the language or
// checking a rule is local work and should never be rationed like a backtest.
func (s *Server) admitMCP(r *http.Request, upstream bool) error {
	if ok, wait := s.mcpIP.allow(s.clientIP(r)); !ok {
		return retryIn(wait)
	}
	if upstream {
		if ok, wait := s.mcpGlobal.allow(); !ok {
			return retryIn(wait)
		}
	}
	return nil
}

// retryIn tells a model when to come back, in words it can act on rather than a
// header it will not see.
func retryIn(wait time.Duration) error {
	return fmt.Errorf("Try again in %d seconds.", max(int(wait.Round(time.Second)/time.Second), 1))
}
