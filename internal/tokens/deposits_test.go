package tokens

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	depositAddress = "0x1111111111111111111111111111111111111111"
	payer          = "0x2222222222222222222222222222222222222222"
	otherToken     = "0x3333333333333333333333333333333333333333"
)

func topic(address string) string {
	return "0x000000000000000000000000" + strings.TrimPrefix(strings.ToLower(address), "0x")
}

// rpc serves one canned receipt, so a test can say exactly what the chain says.
func rpc(t *testing.T, result any) (*httptest.Server, *http.Client) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(server.Close)
	return server, server.Client()
}

func bnbWith(url string) Chain {
	for _, c := range Chains {
		if c.ID == 56 {
			c.rpc = url
			c.receipts = []string{url}
			return c
		}
	}
	panic("BNB Chain missing from the registry")
}

func logFor(token, to, amount string) map[string]any {
	return map[string]any{
		"address": token,
		"topics":  []string{transferTopic, topic(payer), topic(to)},
		"data":    amount,
	}
}

func TestADepositIsReadOffTheChainNotTakenOnTrust(t *testing.T) {
	cash := bnbWith("").Cash.Address
	// 250 USDT, 18 decimals on BNB Chain.
	amount := "0x" + new(big.Int).Mul(big.NewInt(250), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)).Text(16)
	hash := "0x" + strings.Repeat("ab", 32)

	t.Run("a real transfer to the deposit address", func(t *testing.T) {
		server, client := rpc(t, map[string]any{"status": "0x1", "logs": []any{logFor(cash, depositAddress, amount)}})
		got, err := FindDeposit(context.Background(), client, bnbWith(server.URL), hash, depositAddress)
		if err != nil {
			t.Fatal(err)
		}
		if got.Amount.Cmp(new(big.Int).SetBytes(mustHex(t, amount))) != 0 {
			t.Fatalf("amount %s", got.Amount)
		}
		if !strings.EqualFold(got.From, payer) || !strings.EqualFold(got.To, depositAddress) || got.Token != "USDT" {
			t.Fatalf("%+v", got)
		}
	})

	// Each of these is a receipt that exists but does not say what the browser
	// claimed, and none of them may reach an operator as a deposit.
	for _, c := range []struct {
		name   string
		result any
	}{
		{"paid to somebody else", map[string]any{"status": "0x1", "logs": []any{logFor(cash, payer, amount)}}},
		{"a different token", map[string]any{"status": "0x1", "logs": []any{logFor(otherToken, depositAddress, amount)}}},
		{"the transaction reverted", map[string]any{"status": "0x0", "logs": []any{logFor(cash, depositAddress, amount)}}},
		{"nothing was transferred", map[string]any{"status": "0x1", "logs": []any{}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			server, client := rpc(t, c.result)
			if _, err := FindDeposit(context.Background(), client, bnbWith(server.URL), hash, depositAddress); err != ErrNoDeposit {
				t.Fatalf("err = %v, want ErrNoDeposit", err)
			}
		})
	}

	t.Run("a hash that is not on the chain", func(t *testing.T) {
		server, client := rpc(t, nil)
		if _, err := FindDeposit(context.Background(), client, bnbWith(server.URL), hash, depositAddress); err == nil {
			t.Fatal("an unmined transaction must not read as a deposit")
		}
	})

	t.Run("something that is not a hash never reaches the chain", func(t *testing.T) {
		for _, bad := range []string{"", "0x", "not-a-hash", strings.Repeat("a", 66), "0x" + strings.Repeat("zz", 32)} {
			if _, err := FindDeposit(context.Background(), http.DefaultClient, bnbWith("http://127.0.0.1:0"), bad, depositAddress); err == nil {
				t.Fatalf("%q was accepted", bad)
			}
		}
	})
}

func TestCashAmountsReadAsMoney(t *testing.T) {
	usdt := CashAsset{Symbol: "USDT", Decimals: 18}
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	for _, c := range []struct {
		amount *big.Int
		want   string
	}{
		{new(big.Int).Mul(big.NewInt(250), unit), "250"},
		{new(big.Int).Add(new(big.Int).Mul(big.NewInt(250), unit), new(big.Int).Div(unit, big.NewInt(2))), "250.5"},
		{big.NewInt(0), "0"},
		{nil, "0"},
	} {
		if got := usdt.Format(c.amount); got != c.want {
			t.Fatalf("%v formatted as %q, want %q", c.amount, got, c.want)
		}
	}
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	n, ok := new(big.Int).SetString(strings.TrimPrefix(value, "0x"), 16)
	if !ok {
		t.Fatal("bad hex", value)
	}
	return n.Bytes()
}
