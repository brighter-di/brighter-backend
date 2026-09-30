// Package tokens is the approved universe: the EVM chains Brighter trades on
// and the Ondo token that stands for each model ticker on each chain.
//
// PROVENANCE. Every address below comes from Ondo's published token registry
// BNB addresses and metadata are embedded from ondo_bnb_registry.json, the
// shared source for Studio's generated asset list. Existing Ethereum mappings
// and editorial descriptions are preserved in curatedAssets.
// `brighterd verify-tokens` reads symbol() and decimals() for each address
// from the chain itself; run it after any edit here.
//
// Never derive an address. An asset with no entry here is unexecutable and
// the allocation that names it is rejected, not trimmed.
package tokens

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CashAsset is the stablecoin a chain's portfolios are funded with and
// liquidate into. It is Ondo's own cash leg on that chain — the only asset
// Ondo redemption settles in (USDC on Ethereum, USDT on BNB Chain) — so a
// deposit never needs an extra swap before minting.
type CashAsset struct {
	Symbol   string `json:"symbol"`
	Address  string `json:"address"`
	Decimals int    `json:"decimals"`
}

type Chain struct {
	ID       int64     `json:"chainId"`
	Key      string    `json:"key"`
	Name     string    `json:"name"`
	Short    string    `json:"short"`
	Cash     CashAsset `json:"cash"`
	Explorer string    `json:"explorer"`

	// Public RPC used only by verify-tokens. Never used for anything a user
	// signs, and never a paid endpoint.
	rpc string

	// Where transaction receipts are read, when they are not read from the
	// default list in deposits.go. Set by tests.
	receipts []string
}

// Chains the portfolio engine enrolls on AND Ondo has deployed the full template universe
// to. HyperEVM is excluded: it has no BILon (every template's fallback) and
// The portfolio engine's enroll endpoint rejects chain 999.
var Chains = []Chain{
	{
		ID: 1, Key: "ethereum", Name: "Ethereum", Short: "ETH",
		Cash:     CashAsset{Symbol: "USDC", Address: "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", Decimals: 6},
		Explorer: "https://etherscan.io",
		rpc:      "https://ethereum-rpc.publicnode.com",
	},
	{
		ID: 56, Key: "bsc", Name: "BNB Chain", Short: "BNB",
		Cash:     CashAsset{Symbol: "USDT", Address: "0x55d398326f99059ff775485246999027b3197955", Decimals: 18},
		Explorer: "https://bscscan.com",
		rpc:      "https://bsc-rpc.publicnode.com",
	},
}

func ChainByID(id int64) (Chain, bool) {
	for _, c := range Chains {
		if c.ID == id {
			return c, true
		}
	}
	return Chain{}, false
}

// AssetID is the CAIP-19 id of an ERC-20 on this chain. The portfolio engine rejects
// checksummed addresses, so it is always lowercased.
func (c Chain) AssetID(address string) string {
	return fmt.Sprintf("eip155:%d/erc20:%s", c.ID, strings.ToLower(address))
}

// CashAssetID is the CAIP-19 id of the chain's cash asset.
func (c Chain) CashAssetID() string { return c.AssetID(c.Cash.Address) }

// AccountID is the chain-bound CAIP-10 id of an address on this chain.
func (c Chain) AccountID(address string) string {
	return fmt.Sprintf("eip155:%d:%s", c.ID, strings.ToLower(address))
}

type Asset struct {
	Ticker     string `json:"ticker"` // the symbol the research engine prices
	Name       string `json:"name"`
	OndoSymbol string `json:"ondoSymbol"`
	Kind       string `json:"kind"`
	ISIN       string `json:"isin"`
	About      string `json:"about"` // one plain sentence for the asset page

	addresses map[int64]string
}

// Address of the asset's Ondo token on a chain.
func (a Asset) Address(chainID int64) (string, bool) {
	addr, ok := a.addresses[chainID]
	return addr, ok
}

// Addresses by chain id (as a string, for JSON).
func (a Asset) Addresses() map[string]string {
	out := make(map[string]string, len(a.addresses))
	for id, addr := range a.addresses {
		out[strconv.FormatInt(id, 10)] = addr
	}
	return out
}

// LeveragedKind reports whether a Kind names a leveraged or inverse fund.
// These compound daily, so a strategy that can hold one is kept out of the
// list unless it is asked for, and out of the landing page's chart entirely.
// Kind is the only marker: the curated entries below carry "Leveraged ETF"
// and "Inverse ETF", and nothing derives leverage from a ticker.
func LeveragedKind(kind string) bool {
	kind = strings.ToLower(kind)
	return strings.Contains(kind, "leveraged") || strings.Contains(kind, "inverse")
}

// Leveraged reports whether this asset is a leveraged or inverse fund.
func (a Asset) Leveraged() bool { return LeveragedKind(a.Kind) }

// Icon is Ondo's CDN logo for the token.
func (a Asset) Icon() string {
	return "https://cdn.ondo.finance/tokens/logos/" + strings.ToLower(a.OndoSymbol) + "_40x40.svg"
}

var curatedAssets = map[string]Asset{
	"AAPL": {Ticker: "AAPL", Name: "Apple Inc.", OndoSymbol: "AAPLon", Kind: "Common Stock", ISIN: "US0378331005",
		About:     "Designs the iPhone, Mac and iPad and runs the App Store and Apple's subscription services.",
		addresses: map[int64]string{1: "0x14c3abf95cb9c93a8b82c1cdcb76d72cb87b2d4c"}},
	"MSFT": {Ticker: "MSFT", Name: "Microsoft Corporation", OndoSymbol: "MSFTon", Kind: "Common Stock", ISIN: "US5949181045",
		About:     "Makes Windows and Office and runs Azure, one of the largest cloud platforms.",
		addresses: map[int64]string{1: "0xb812837b81a3a6b81d7cd74cfb19a7f2784555e5"}},
	"AMZN": {Ticker: "AMZN", Name: "Amazon.com, Inc.", OndoSymbol: "AMZNon", Kind: "Common Stock", ISIN: "US0231351067",
		About:     "Runs the largest US online store and Amazon Web Services, the leading cloud provider.",
		addresses: map[int64]string{1: "0xbb8774fb97436d23d74c1b882e8e9a69322cfd31"}},
	"GOOGL": {Ticker: "GOOGL", Name: "Alphabet Inc. Class A", OndoSymbol: "GOOGLon", Kind: "Common Stock", ISIN: "US02079K3059",
		About:     "Parent of Google Search, YouTube, Android and Google Cloud. Class A shares carry voting rights.",
		addresses: map[int64]string{1: "0xba47214edd2bb43099611b208f75e4b42fdcfedc"}},
	"META": {Ticker: "META", Name: "Meta Platforms, Inc.", OndoSymbol: "METAon", Kind: "Common Stock", ISIN: "US30303M1027",
		About:     "Owns Facebook, Instagram, WhatsApp and Messenger, and builds AI models and VR headsets.",
		addresses: map[int64]string{1: "0x59644165402b611b350645555b50afb581c71eb2"}},
	"NVDA": {Ticker: "NVDA", Name: "NVIDIA Corporation", OndoSymbol: "NVDAon", Kind: "Common Stock", ISIN: "US67066G1040",
		About:     "Designs the GPUs and networking behind most AI training and data-center computing.",
		addresses: map[int64]string{1: "0x2d1f7226bd1f780af6b9a49dcc0ae00e8df4bdee"}},
	"TSLA": {Ticker: "TSLA", Name: "Tesla, Inc.", OndoSymbol: "TSLAon", Kind: "Common Stock", ISIN: "US88160R1014",
		About:     "Builds electric vehicles, batteries and home energy storage, and develops self-driving software.",
		addresses: map[int64]string{1: "0xf6b1117ec07684d3958cad8beb1b302bfd21103f"}},
	"SPY": {Ticker: "SPY", Name: "SPDR S&P 500 ETF Trust", OndoSymbol: "SPYon", Kind: "Equity ETF", ISIN: "US78462F1030",
		About:     "Tracks the S&P 500 — the 500 largest US companies — and is the most traded ETF in the world.",
		addresses: map[int64]string{1: "0xfedc5f4a6c38211c1338aa411018dfaf26612c08"}},
	"QQQ": {Ticker: "QQQ", Name: "Invesco QQQ Trust", OndoSymbol: "QQQon", Kind: "Equity ETF", ISIN: "US46090E1038",
		About:     "Tracks the Nasdaq-100: the 100 largest non-financial companies listed on Nasdaq.",
		addresses: map[int64]string{1: "0x0e397938c1aa0680954093495b70a9f5e2249aba"}},
	"BIL": {Ticker: "BIL", Name: "SPDR Bloomberg 1-3 Month T-Bill ETF", OndoSymbol: "BILon", Kind: "Bond ETF", ISIN: "US78468R6633",
		About:     "Holds US Treasury bills maturing in one to three months — close to cash, earning short-term interest.",
		addresses: map[int64]string{1: "0x2710e30d84b376c34a3b3036aa196b26a83a321b"}},

	// Asset classes for Global Rotation and All-Weather.
	"IWM": {Ticker: "IWM", Name: "iShares Russell 2000 ETF", OndoSymbol: "IWMon", Kind: "Equity ETF", ISIN: "US4642876555",
		About:     "Tracks the Russell 2000 index of about 2,000 smaller US companies.",
		addresses: map[int64]string{1: "0x070d79021dd7e841123cb0cf554993bf683c511d"}},
	"EFA": {Ticker: "EFA", Name: "iShares MSCI EAFE ETF", OndoSymbol: "EFAon", Kind: "Equity ETF", ISIN: "US4642874659",
		About:     "Holds large and mid-size companies in developed markets outside North America, mainly Europe, Japan and Australia.",
		addresses: map[int64]string{1: "0x4111b60bc87f2bd1e81e783e271d7f0ec6ee088b"}},
	"EEM": {Ticker: "EEM", Name: "iShares MSCI Emerging Markets ETF", OndoSymbol: "EEMon", Kind: "Equity ETF", ISIN: "US4642872349",
		About:     "Holds large and mid-size companies in emerging markets such as China, India, Taiwan and Brazil.",
		addresses: map[int64]string{1: "0x77a1a02e4a888ada8620b93c30de8a41e621126c"}},
	"TLT": {Ticker: "TLT", Name: "iShares 20+ Year Treasury Bond ETF", OndoSymbol: "TLTon", Kind: "Bond ETF", ISIN: "US87422Q1094",
		About:     "Holds US Treasury bonds maturing in more than 20 years, so it moves strongly with interest rates.",
		addresses: map[int64]string{1: "0x992651bfeb9a0dcc4457610e284ba66d86489d4d"}},
	"IEF": {Ticker: "IEF", Name: "iShares 7-10 Year Treasury Bond ETF", OndoSymbol: "IEFon", Kind: "Bond ETF", ISIN: "US4642874402",
		About:     "Holds US Treasury bonds maturing in seven to ten years.",
		addresses: map[int64]string{1: "0xa2ec76139028f279a1c790d323c57cc4158098d6"}},
	"GLD": {Ticker: "GLD", Name: "SPDR Gold Shares", OndoSymbol: "GLDon", Kind: "Commodity ETF", ISIN: "US78463V1070",
		About:     "Holds physical gold bullion in vaults, so its price follows gold.",
		addresses: map[int64]string{1: "0x423d42e505e64f99b6e277eb7ed324cc5606f139"}},
	"DBC": {Ticker: "DBC", Name: "Invesco DB Commodity Index Tracking Fund", OndoSymbol: "DBCon", Kind: "Commodity ETF", ISIN: "US46138B1035",
		About:     "Tracks futures on 14 heavily traded commodities, including oil, gold, wheat and copper.",
		addresses: map[int64]string{1: "0x20224080ad516769723c9a4a18325fc4e8c9ab5d"}},

	// Blue chips beyond Mag7, for Low Volatility Leaders.
	"AVGO": {Ticker: "AVGO", Name: "Broadcom Inc.", OndoSymbol: "AVGOon", Kind: "Common Stock", ISIN: "US11135F1012",
		About:     "Designs chips and networking hardware for data centers and phones, and sells infrastructure software.",
		addresses: map[int64]string{1: "0x0d54d4279b9e8c54cd8547c2c75a8ee81a0bcae8"}},
	"JPM": {Ticker: "JPM", Name: "JPMorgan Chase & Co.", OndoSymbol: "JPMon", Kind: "Common Stock", ISIN: "US46625H1005",
		About:     "The largest US bank by assets, spanning consumer banking, investment banking and asset management.",
		addresses: map[int64]string{1: "0x03c1ec4ca9dbb168e6db0def827c085999cbffaf"}},
	"LLY": {Ticker: "LLY", Name: "Eli Lilly and Company", OndoSymbol: "LLYon", Kind: "Common Stock", ISIN: "US5324571083",
		About:     "Develops medicines, including leading diabetes and obesity drugs.",
		addresses: map[int64]string{1: "0xf192957ae52db3eb088654403cc2eded014ae556"}},
	"COST": {Ticker: "COST", Name: "Costco Wholesale Corporation", OndoSymbol: "COSTon", Kind: "Common Stock", ISIN: "US22160K1051",
		About:     "Runs Costco membership warehouse stores across the US and abroad.",
		addresses: map[int64]string{1: "0x0c8276e4fec072cf7854be69c70f7773d1610857"}},
	"V": {Ticker: "V", Name: "Visa Inc.", OndoSymbol: "Von", Kind: "Common Stock", ISIN: "US92826C8394",
		About:     "Runs the Visa card network that carries payments between banks, merchants and cardholders.",
		addresses: map[int64]string{1: "0xac37c20c1d0e5285035e056101a64e263ff94a41"}},
	"WMT": {Ticker: "WMT", Name: "Walmart Inc.", OndoSymbol: "WMTon", Kind: "Common Stock", ISIN: "US9311421039",
		About:     "Runs Walmart supercenters and Sam's Club; the largest US retailer by revenue.",
		addresses: map[int64]string{1: "0x82106347ddbb23ce44cf4ce4053ef1adf8b9323b"}},

	// Leveraged and inverse funds, for the switcher templates.
	"TQQQ": {Ticker: "TQQQ", Name: "ProShares UltraPro QQQ", OndoSymbol: "TQQQon", Kind: "Leveraged ETF", ISIN: "US74347X8314",
		About:     "Aims for three times the Nasdaq-100's daily return. The multiple resets every day, so over longer stretches it drifts from 3× the index.",
		addresses: map[int64]string{1: "0xa45cd7ac9865b9539166ebaf2abc362df4736580"}},
	"SQQQ": {Ticker: "SQQQ", Name: "ProShares UltraPro Short QQQ", OndoSymbol: "SQQQon", Kind: "Inverse ETF", ISIN: "US74350P6759",
		About:     "Aims for three times the inverse of the Nasdaq-100's daily return: it rises when the index falls. The multiple resets every day.",
		addresses: map[int64]string{1: "0x0a00c19246fc41b2524d56c87ec44ce8b30ba0f8"}},
	"PSQ": {Ticker: "PSQ", Name: "ProShares Short QQQ", OndoSymbol: "PSQon", Kind: "Inverse ETF", ISIN: "US74349Y8378",
		About:     "Aims for the inverse of the Nasdaq-100's daily return, one for one: it rises when the index falls.",
		addresses: map[int64]string{1: "0x9ebd34d99cc3a45b39cafc14ad7994263fa2be56"}},
	"SOXL": {Ticker: "SOXL", Name: "Direxion Daily Semiconductor Bull 3X Shares", OndoSymbol: "SOXLon", Kind: "Leveraged ETF", ISIN: "US25459W4583",
		About:     "Aims for three times the daily return of an index of US-listed chipmakers. The multiple resets every day.",
		addresses: map[int64]string{1: "0xd318bbbe6b6e83f74e7acaeb5e94c08ddbdb44c0"}},
	"SOXS": {Ticker: "SOXS", Name: "Direxion Daily Semiconductor Bear 3X Shares", OndoSymbol: "SOXSon", Kind: "Inverse ETF", ISIN: "US25461H5726",
		About:     "Aims for three times the inverse of the daily return of an index of US-listed chipmakers: it rises when they fall.",
		addresses: map[int64]string{1: "0x01eb2ee6ec05c304666210133b762088ff9965ac"}},
}

func Lookup(ticker string) (Asset, bool) {
	canonical, ok := assetAliases[strings.ToUpper(ticker)]
	a, found := assets[canonical]
	return a, ok && found
}

// DisplayName is the security name for a ticker, or the ticker itself.
func DisplayName(ticker string) string {
	if a, ok := Lookup(ticker); ok {
		return a.Name
	}
	return ticker
}

// AssetID is the CAIP-19 id of a ticker's Ondo token on a chain.
func AssetID(ticker string, chainID int64) (string, bool) {
	a, ok := Lookup(ticker)
	if !ok {
		return "", false
	}
	chain, ok := ChainByID(chainID)
	if !ok {
		return "", false
	}
	addr, ok := a.addresses[chainID]
	if !ok {
		return "", false
	}
	return chain.AssetID(addr), true
}

// All returns every asset, sorted by ticker.
func All() []Asset {
	out := make([]Asset, 0, len(assets))
	for _, a := range assets {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ticker < out[j].Ticker })
	return out
}
