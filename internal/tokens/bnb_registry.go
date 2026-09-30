package tokens

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strings"
)

// ondo_bnb_registry.json is the authoritative BNB registry. Studio's asset
// list is generated from this file; Ethereum addresses are never inferred.
//
//go:embed ondo_bnb_registry.json
var bnbRegistryJSON []byte

type bnbAsset struct {
	Ticker     string `json:"ticker"`
	Name       string `json:"name"`
	Underlying string `json:"underlying"`
	Address    string `json:"address"`
	Kind       string `json:"kind"`
	ISIN       string `json:"isin"`
	TypeDetail string `json:"typeDetail"`
}

var assets = loadBNBRegistry(curatedAssets, bnbRegistryJSON)
var assetAliases = func() map[string]string {
	out := make(map[string]string, len(assets)*2)
	for ticker, a := range assets {
		out[strings.ToUpper(ticker)] = ticker
		out[strings.ToUpper(a.OndoSymbol)] = ticker
	}
	return out
}()

func loadBNBRegistry(curated map[string]Asset, raw []byte) map[string]Asset {
	var rows []bnbAsset
	if err := json.Unmarshal(raw, &rows); err != nil {
		panic(fmt.Sprintf("invalid BNB token registry: %v", err))
	}
	out := make(map[string]Asset, len(rows))
	addressPattern := regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	seenSymbols, seenAddresses := map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		ticker := row.Underlying
		// Match Studio's display symbol for USDon while retaining the real
		// contract symbol for verification and the correct contract address.
		if ticker == row.Ticker && strings.HasSuffix(ticker, "on") {
			ticker = strings.TrimSuffix(ticker, "on")
		}
		address := strings.ToLower(row.Address)
		symbol := strings.ToUpper(row.Ticker)
		if ticker == "" || strings.TrimSpace(row.Name) == "" || row.Ticker == "" || !addressPattern.MatchString(address) || address == "0x0000000000000000000000000000000000000000" || seenSymbols[symbol] || seenAddresses[address] {
			panic(fmt.Sprintf("invalid or duplicate BNB token registry entry %q", row.Ticker))
		}
		if _, exists := out[ticker]; exists {
			panic(fmt.Sprintf("duplicate BNB underlying ticker %q", ticker))
		}
		if row.Kind != "Stock" && row.Kind != "ETF" && row.Kind != "Currency" {
			panic(fmt.Sprintf("invalid BNB token kind %q", row.Kind))
		}
		seenSymbols[symbol], seenAddresses[address] = true, true
		a, exists := curated[ticker]
		if exists {
			if a.OndoSymbol != row.Ticker {
				panic(fmt.Sprintf("BNB symbol disagrees with curated token %q", ticker))
			}
			a.addresses = maps.Clone(a.addresses)
		} else {
			name := strings.TrimSpace(strings.TrimSuffix(row.Name, " (Ondo)"))
			kind := row.TypeDetail
			if row.Kind == "Currency" {
				kind = "Currency"
			}
			if kind == "" {
				kind = row.Kind
			}
			a = Asset{Ticker: ticker, Name: name, OndoSymbol: row.Ticker, Kind: kind, ISIN: row.ISIN, About: "Tokenized exposure to " + name + ".", addresses: map[int64]string{}}
		}
		a.addresses[56] = address
		out[ticker] = a
	}
	// A missing source row must not silently remove an existing contract.
	for ticker := range curated {
		if _, exists := out[ticker]; !exists {
			panic(fmt.Sprintf("curated token %q is absent from the BNB registry", ticker))
		}
	}
	return out
}
