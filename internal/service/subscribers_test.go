package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Anyone can press Start on the bot, so what subscribers get must never name
// a portfolio or a wallet: a portfolio id opens its holdings in the app.
func TestSubscribersGetEventsWithoutIDs(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	if _, err := h.store.AddSubscriber(ctx, 42); err != nil {
		t.Fatal(err)
	}
	pf := h.invest(t, mag7())
	if _, err := h.svc.ListPortfolios(ctx, owner, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Deploy(ctx, pf.PortfolioID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Withdraw(ctx, pf.PortfolioID, WithdrawSubmit{Mode: "liquidate", Message: json.RawMessage(`{}`), Signature: sig}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"New strategy: Mag7 Momentum Rank (126d · 3) on BNB Chain, starting at",
		"New portfolio: Mag7 Momentum Rank (126d · 3) on BNB Chain.",
		"A new wallet connected.",
		"Rebalance requested: Mag7 Momentum Rank (126d · 3).",
		"Withdrawal submitted: sell everything to USDT from Mag7 Momentum Rank (126d · 3).",
	} {
		h.telegram.WaitTo(t, "42", want)
	}
	for _, m := range h.telegram.To("42") {
		if strings.Contains(m, pf.PortfolioID) || strings.Contains(m, "0x") {
			t.Errorf("a public message names a portfolio or wallet: %q", m)
		}
	}
	// The operator chat keeps the full text.
	h.telegram.WaitTo(t, "1", "portfolio "+pf.PortfolioID+" of "+shortAddr(ownerLower))
}
