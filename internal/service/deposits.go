package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/brighter-di/backend/internal/tokens"
)

// Deposits are the one thing Brighter is told about rather than doing: funds
// go from a wallet straight to the portfolio's smart account, so the browser
// is the only party that knows a deposit happened. That makes a report a
// claim, and an endpoint that simply relayed it would put whatever anyone
// posted in front of an operator.
//
// So a successful deposit is reported as a transaction hash and nothing else,
// and every word of the message comes from the receipt. A failure cannot be
// checked against anything, so it is marked unverified and kept to operators.

// depositRPC reads receipts from the same public endpoints verify-tokens uses.
var depositRPC = &http.Client{Timeout: 20 * time.Second}

// ReportedDeposit is what the chain said about a reported deposit.
type ReportedDeposit struct {
	Amount string `json:"amount"`
	Token  string `json:"token"`
	TxHash string `json:"txHash"`
}

// ReportDeposit announces a deposit once the chain agrees it happened. A hash
// that names no transfer of this chain's cash asset into this portfolio's
// deposit address announces nothing at all.
func (s *Service) ReportDeposit(ctx context.Context, id, txHash string) (*ReportedDeposit, error) {
	p, err := s.knownPortfolio(ctx, id)
	if err != nil {
		return nil, err
	}
	chain, ok := tokens.ChainByID(p.ChainID)
	if !ok {
		return nil, errf(422, "DEPOSIT_UNVERIFIABLE", "Deposits on chain %d cannot be checked.", p.ChainID)
	}
	found, err := tokens.FindDeposit(ctx, depositRPC, chain, txHash, p.DepositAddress)
	if errors.Is(err, tokens.ErrNoDeposit) {
		return nil, errf(422, "DEPOSIT_NOT_FOUND", "That transaction holds no %s transfer to this portfolio.", chain.Cash.Symbol)
	}
	if err != nil {
		return nil, errf(422, "DEPOSIT_UNVERIFIABLE", "%s", err.Error())
	}
	amount := chain.Cash.Format(found.Amount)
	v := s.portfolioView(ctx, p)
	s.announce(
		fmt.Sprintf("Deposit received: %s %s into %s (%s), portfolio %s of %s, from %s. %s/tx/%s",
			amount, found.Token, v.TemplateName, v.ParamsLabel, id, shortAddr(p.Owner), shortAddr(found.From), chain.Explorer, txHash),
		fmt.Sprintf("Deposit received: %s %s into %s (%s).", amount, found.Token, v.TemplateName, v.ParamsLabel),
	)
	return &ReportedDeposit{Amount: amount, Token: found.Token, TxHash: txHash}, nil
}

// maxDepositProblem keeps a wallet's error text to something a message can
// carry; the browser's copy is a sentence, not a stack trace.
const maxDepositProblem = 300

// ReportDepositProblem passes on a deposit that did not complete. Nothing here
// can be checked against a chain, so it reaches operators only and says so.
func (s *Service) ReportDepositProblem(ctx context.Context, id, reason string) error {
	p, err := s.knownPortfolio(ctx, id)
	if err != nil {
		return err
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) > maxDepositProblem {
		reason = reason[:maxDepositProblem] + "…"
	}
	if reason == "" {
		reason = "no reason given"
	}
	v := s.portfolioView(ctx, p)
	// Operators only: an unverified report must not reach the broadcast.
	s.announce(fmt.Sprintf("Deposit failed (unverified, reported by the page): %s (%s), portfolio %s of %s: %s",
		v.TemplateName, v.ParamsLabel, id, shortAddr(p.Owner), reason), "")
	return nil
}
