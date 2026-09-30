package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestOperatorNotifications(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	pf := h.invest(t, mag7())
	h.telegram.Wait(t, "New strategy: Mag7 Momentum Rank")
	h.telegram.Wait(t, "New portfolio: Mag7 Momentum Rank (126d · 3) on BNB Chain by "+shortAddr(ownerLower))

	// The first listing announces the wallet; a return visit does not.
	for range 2 {
		if _, err := h.svc.ListPortfolios(ctx, owner, false); err != nil {
			t.Fatal(err)
		}
	}
	h.telegram.Wait(t, "New wallet connected: "+shortAddr(ownerLower))

	if _, err := h.svc.Deploy(ctx, pf.PortfolioID); err != nil {
		t.Fatal(err)
	}
	h.telegram.Wait(t, "Rebalance requested: Mag7 Momentum Rank")

	if _, err := h.svc.Withdraw(ctx, pf.PortfolioID, WithdrawSubmit{Mode: "liquidate", Message: json.RawMessage(`{}`), Signature: sig}); err != nil {
		t.Fatal(err)
	}
	h.telegram.Wait(t, "Withdrawal submitted: sell everything to USDT")

	combos, err := h.svc.Combos(ctx)
	if err != nil || len(combos) != 1 {
		t.Fatalf("combos %v, %v", combos, err)
	}
	h.backtestEngine.Set(map[string]float64{"AAPL": 0.5, "BIL": 0.5}, testNow)
	h.svc.Reevaluate(ctx, combos[0], "manual", 6)
	if msg := h.telegram.Wait(t, "Allocation updated: Mag7 Momentum Rank"); !strings.Contains(msg, "AAPL 50.0%, BIL 50.0%") {
		t.Fatalf("message %q", msg)
	}

	// Messages go out in order, so by now a second wallet notice would have arrived.
	wallets := 0
	for _, m := range h.telegram.Messages() {
		if strings.HasPrefix(m, "New wallet connected") {
			wallets++
		}
	}
	if wallets != 1 {
		t.Fatalf("%d wallet notices, want 1: %q", wallets, h.telegram.Messages())
	}
}
