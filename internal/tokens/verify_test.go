package tokens

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifyChecksOnlyActualChainMappings(t *testing.T) {
	symbols, decimals := map[string]string{}, map[string]int{}
	wanted := 0
	for _, chain := range Chains {
		symbols[strings.ToLower(chain.Cash.Address)] = chain.Cash.Symbol
		decimals[strings.ToLower(chain.Cash.Address)] = chain.Cash.Decimals
		wanted++
		for _, a := range All() {
			if address, ok := a.Address(chain.ID); ok {
				symbols[address] = a.OndoSymbol
				decimals[address] = 18
				wanted++
			}
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid RPC", 400)
			return
		}
		var call struct{ To, Data string }
		json.Unmarshal(body.Params[0], &call)
		symbol, ok := symbols[strings.ToLower(call.To)]
		if !ok {
			t.Error("verification called unmapped contract", call.To)
			http.Error(w, "unmapped", 400)
			return
		}
		result := fmt.Sprintf("0x%064x", decimals[strings.ToLower(call.To)])
		if call.Data == "0x95d89b41" {
			encoded := hex.EncodeToString([]byte(symbol))
			padding := (64 - len(encoded)%64) % 64
			result = fmt.Sprintf("0x%064x%064x%s%s", 32, len(symbol), encoded, strings.Repeat("0", padding))
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer server.Close()
	original := append([]Chain(nil), Chains...)
	defer func() { Chains = original }()
	for i := range Chains {
		Chains[i].rpc = server.URL
	}
	checks := Verify(context.Background(), server.Client())
	if len(checks) != wanted || wanted != 482 {
		t.Fatal("wrong number of mapped contracts checked", len(checks), wanted)
	}
	for _, check := range checks {
		if !check.OK() {
			t.Fatalf("false verification failure: %+v", check)
		}
	}
}
