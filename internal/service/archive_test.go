package service

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"github.com/brighter-di/backend/internal/auth"
)

// archiveOwner is the address of the private key 0x11…11.
const archiveOwner = "0x19e7e376e7c213b7e7e7e46cc70a5dd086daff2a"

func signArchive(message string) string {
	key := secp256k1.PrivKeyFromBytes(bytes.Repeat([]byte{0x11}, 32))
	compact := ecdsa.SignCompact(key, auth.MessageHash(message), false)
	return "0x" + hex.EncodeToString(append(compact[1:], compact[0]))
}

// The frontend's archiveMessage builds this exact text; a change here must
// be made there too.
func TestArchiveMessageIsStable(t *testing.T) {
	got := ArchiveMessage("pf-1", time.Date(2026, 9, 15, 8, 30, 0, 0, time.UTC))
	want := "Archive Brighter portfolio pf-1\n\nThis stops its automation and hides it from your portfolios. It moves no funds.\n\nIssued At: 2026-09-15T08:30:00Z"
	if got != want {
		t.Fatalf("message changed:\n%q\nwant\n%q", got, want)
	}
}

func TestArchiveNeedsTheOwnerAndAnEmptyPortfolio(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	req := mag7()
	req.Owner = archiveOwner
	pf := h.invest(t, req)

	issued := testNow.UTC().Format(time.RFC3339)
	signed := func(at string) ArchiveRequest {
		parsed, _ := time.Parse(time.RFC3339, at)
		return ArchiveRequest{IssuedAt: at, Signature: signArchive(ArchiveMessage(pf.PortfolioID, parsed))}
	}

	other := signed(issued)
	other.Signature = sig
	wantCode(t, h.svc.Archive(ctx, pf.PortfolioID, other), "BAD_SIGNATURE")
	wantCode(t, h.svc.Archive(ctx, pf.PortfolioID, signed(testNow.Add(-time.Hour).UTC().Format(time.RFC3339))), "ARCHIVE_EXPIRED")

	h.portfolioEngine.Handle("GET /portfolios/{id}/positions", func(r *http.Request, _ map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"portfolioId": r.PathValue("id"), "totalValueUsd": "0", "assets": []any{}, "warnings": []any{}, "inTransit": map[string]any{"totalUsd": "25.00"}}
	})
	wantCode(t, h.svc.Archive(ctx, pf.PortfolioID, signed(issued)), "PORTFOLIO_NOT_EMPTY")
	if n := len(h.portfolioEngine.Requests("POST /portfolios/{id}/stop")); n != 0 {
		t.Fatalf("stopped a portfolio with money in transit (%d calls)", n)
	}

	// While it is live it is on the live list and carries no archived date.
	if live, err := h.svc.ListPortfolios(ctx, archiveOwner, false); err != nil || len(live) != 1 || live[0].ArchivedAt != nil {
		t.Fatalf("before archiving: %+v %v", live, err)
	}
	if empty, err := h.svc.ListPortfolios(ctx, archiveOwner, true); err != nil || len(empty) != 0 {
		t.Fatalf("archived list is not empty before archiving: %+v %v", empty, err)
	}

	h.portfolioEngine.Handle("GET /portfolios/{id}/positions", func(r *http.Request, _ map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"portfolioId": r.PathValue("id"), "totalValueUsd": "0.004", "assets": []any{}, "warnings": []any{}}
	})
	if err := h.svc.Archive(ctx, pf.PortfolioID, signed(issued)); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if n := len(h.portfolioEngine.Requests("POST /portfolios/{id}/stop")); n != 1 {
		t.Fatalf("%d stop calls, want 1", n)
	}
	_, err := h.svc.Portfolio(ctx, pf.PortfolioID)
	wantCode(t, err, "PORTFOLIO_NOT_FOUND")
	if list, err := h.svc.ListPortfolios(ctx, archiveOwner, false); err != nil || len(list) != 0 {
		t.Fatalf("archived portfolio still listed: %v %v", list, err)
	}

	// It is retired, not gone: the archived list is where it lives now, and it
	// says when it was retired.
	archived, err := h.svc.ListPortfolios(ctx, archiveOwner, true)
	if err != nil || len(archived) != 1 {
		t.Fatalf("archived list: %v %v", archived, err)
	}
	if archived[0].PortfolioID != pf.PortfolioID {
		t.Fatalf("archived %s, want %s", archived[0].PortfolioID, pf.PortfolioID)
	}
	if archived[0].ArchivedAt == nil || archived[0].ArchivedAt.IsZero() {
		t.Fatalf("no archivedAt: %+v", archived[0])
	}
	if archived[0].TemplateName == "" || len(archived[0].Allocation) == 0 {
		t.Fatalf("an archived portfolio must still describe itself: %+v", archived[0])
	}
}
