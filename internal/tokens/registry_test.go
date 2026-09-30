package tokens

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBNBRegistryResolvesEveryDeployedToken(t *testing.T) {
	var rows []bnbAsset
	if err := json.Unmarshal(bnbRegistryJSON, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 452 || len(All()) != 452 {
		t.Fatal("incomplete BNB universe", len(rows), len(All()))
	}
	addresses := map[string]bool{}
	securities, ethereum := 0, 0
	for _, row := range rows {
		for _, spelling := range []string{row.Ticker, row.Underlying, strings.ToLower(row.Ticker), strings.ToLower(row.Underlying)} {
			a, ok := Lookup(spelling)
			if !ok || a.OndoSymbol != row.Ticker {
				t.Fatal("unresolved or wrong token", spelling, a)
			}
			address, ok := a.Address(56)
			if !ok || address != strings.ToLower(row.Address) {
				t.Fatal("wrong BNB mapping", spelling, address)
			}
			id, ok := AssetID(spelling, 56)
			if !ok || id != "eip155:56/erc20:"+address {
				t.Fatal("wrong executable asset id", spelling, id)
			}
		}
		a, _ := Lookup(row.Ticker)
		address, _ := a.Address(56)
		if addresses[address] {
			t.Fatal("duplicate BNB asset contract", address)
		}
		addresses[address] = true
		if a.Kind != "Currency" {
			securities++
		}
		if _, ok := a.Address(1); ok {
			ethereum++
		}
	}
	if securities != 450 || ethereum != len(curatedAssets) {
		t.Fatal("wrong securities or Ethereum universe", securities, ethereum)
	}
}
func TestPreviouslyMissingBNBAssetsAreMappedWithoutInventedEthereumAddresses(t *testing.T) {
	for ticker, address := range map[string]string{"AMD": "0x9f16e46c73b43bdb70861247d537bee4ea18f639", "PLTR": "0x9351abd19f42101dd36025e495b98e910b255d78", "SMCI": "0xc142ba8ccd36d80c3a001342fb83e4c3d218a873"} {
		a, ok := Lookup(ticker)
		if !ok {
			t.Fatal("missing BNB asset", ticker)
		}
		got, ok := a.Address(56)
		if !ok || got != address {
			t.Fatal("wrong BNB address", ticker, got)
		}
		if _, ok := a.Address(1); ok {
			t.Fatal("inferred Ethereum address", ticker)
		}
		if a.ISIN == "" || a.Name == "" || a.Kind == "" {
			t.Fatal("missing security metadata", ticker)
		}
	}
	for _, ticker := range []string{"FAKE", "FAKEon", ""} {
		if _, ok := Lookup(ticker); ok {
			t.Fatal("unknown ticker accepted", ticker)
		}
	}
}
func TestRegistryPreservesExistingContractsAndDescriptions(t *testing.T) {
	for ticker, original := range curatedAssets {
		a, ok := Lookup(ticker)
		if !ok || a.Name != original.Name || a.About != original.About || a.ISIN != original.ISIN {
			t.Fatal("curated metadata changed", ticker)
		}
		for chain, address := range original.addresses {
			if got, ok := a.Address(chain); !ok || got != strings.ToLower(address) {
				t.Fatal("existing contract changed", ticker, chain, got)
			}
		}
	}
}

// The leveraged and inverse funds are known only because curatedAssets says
// so: Ondo's registry types all five as "Equity ETF" (their typeDetail), so a
// leveraged fund added upstream and not curated here would read as an ordinary
// ETF. This pins the set the catalogue's "hide leveraged" filter can see.
func TestLeveragedFundsAreTheCuratedOnes(t *testing.T) {
	want := map[string]bool{"TQQQ": true, "SOXL": true, "SQQQ": true, "PSQ": true, "SOXS": true}
	for _, a := range All() {
		if a.Leveraged() != want[a.Ticker] {
			t.Fatalf("%s: leveraged=%v, kind %q", a.Ticker, a.Leveraged(), a.Kind)
		}
	}
	for ticker := range want {
		if a, ok := Lookup(ticker); !ok || !a.Leveraged() {
			t.Fatal("curated leveraged fund lost its kind", ticker)
		}
	}
	for _, ticker := range []string{"SPY", "QQQ", "AAPL", "BIL", "GLD", "TLT"} {
		if a, _ := Lookup(ticker); a.Leveraged() {
			t.Fatal("ordinary asset read as leveraged", ticker, a.Kind)
		}
	}
}
