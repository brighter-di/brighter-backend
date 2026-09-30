package tokens

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Reading a deposit back off the chain.
//
// Brighter never sees a deposit as it happens: the wallet transfers straight
// to the portfolio's smart account. A browser telling us one landed is
// therefore a claim, not a fact, and an endpoint that believed it would let
// anyone put whatever they liked in front of an operator. So the claim is a
// transaction hash and nothing else, and everything said about it afterwards
// comes from the receipt.

// Transfer(address,address,uint256)
const transferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

var txHashPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)

// Receipts need an endpoint willing to serve them. The registry's RPC answers
// eth_call for verify-tokens but refuses eth_getTransactionReceipt as an
// "archive request", even for a transaction minutes old — so a deposit read
// through it would never verify. These are the endpoints the browser already
// uses to confirm a deposit, tried in turn.
var receiptRPCs = map[int64][]string{
	56: {"https://bsc-dataseed.bnbchain.org", "https://bsc-dataseed.defibit.io", "https://bsc-dataseed.ninicoin.io"},
	1:  {"https://ethereum-rpc.publicnode.com", "https://eth.llamarpc.com"},
}

// receiptEndpoints are where a chain's receipts can be read, the registry's own
// RPC last so a chain with no list still has somewhere to ask.
func receiptEndpoints(chain Chain) []string {
	if len(chain.receipts) > 0 {
		return chain.receipts
	}
	if urls, ok := receiptRPCs[chain.ID]; ok {
		return urls
	}
	return []string{chain.rpc}
}

// Deposit is a cash transfer into a deposit address, as the chain records it.
type Deposit struct {
	From   string
	To     string
	Token  string
	Amount *big.Int
}

// ErrNoDeposit means the receipt is real but says nothing we were told it
// said: wrong recipient, wrong token, or a transaction that reverted.
var ErrNoDeposit = fmt.Errorf("no matching cash transfer in that transaction")

// FindDeposit reads a transaction's receipt and returns the transfer of the
// chain's cash asset into `to`, if there is one. Every field it returns is
// from the receipt; nothing is taken from the caller but the hash.
func FindDeposit(ctx context.Context, hc *http.Client, chain Chain, txHash, to string) (*Deposit, error) {
	if !txHashPattern.MatchString(txHash) {
		return nil, fmt.Errorf("not a transaction hash")
	}
	var found *receipt
	var err error
	for _, rpc := range receiptEndpoints(chain) {
		// A public endpoint may be down, rate-limited, or refuse receipts
		// outright; the next one usually will not.
		if found, err = transactionReceipt(ctx, hc, rpc, txHash); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("that transaction is not on %s yet", chain.Name)
	}
	// A reverted transaction moved nothing, whatever its logs say.
	if strings.ToLower(found.Status) != "0x1" {
		return nil, ErrNoDeposit
	}
	cash := strings.ToLower(chain.Cash.Address)
	want := strings.ToLower(to)
	for _, log := range found.Logs {
		if len(log.Topics) != 3 || !strings.EqualFold(log.Topics[0], transferTopic) {
			continue
		}
		if !strings.EqualFold(log.Address, cash) || !strings.EqualFold(topicAddress(log.Topics[2]), want) {
			continue
		}
		amount, ok := new(big.Int).SetString(strings.TrimPrefix(log.Data, "0x"), 16)
		if !ok {
			continue
		}
		return &Deposit{
			From:   topicAddress(log.Topics[1]),
			To:     want,
			Token:  chain.Cash.Symbol,
			Amount: amount,
		}, nil
	}
	return nil, ErrNoDeposit
}

// topicAddress reads the 20-byte address out of a 32-byte indexed topic.
func topicAddress(topic string) string {
	raw := strings.TrimPrefix(topic, "0x")
	if len(raw) < 40 {
		return ""
	}
	return "0x" + strings.ToLower(raw[len(raw)-40:])
}

type receiptLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

type receipt struct {
	Status string       `json:"status"`
	Logs   []receiptLog `json:"logs"`
}

func transactionReceipt(ctx context.Context, hc *http.Client, rpc, txHash string) (*receipt, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_getTransactionReceipt",
		"params": []any{txHash},
	})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpc, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out struct {
		Result *receipt `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("rpc %s: %w", rpc, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("rpc %s: %s", rpc, out.Error.Message)
	}
	return out.Result, nil
}

// Decimals renders an amount in an asset's own units, for a message a person
// reads rather than a number a machine does.
func (c CashAsset) Format(amount *big.Int) string {
	if amount == nil {
		return "0"
	}
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(c.Decimals)), nil)
	whole, rest := new(big.Int).QuoRem(amount, unit, new(big.Int))
	if rest.Sign() == 0 {
		return whole.String()
	}
	frac := strings.TrimRight(fmt.Sprintf("%0*s", c.Decimals, rest.String()), "0")
	if len(frac) > 2 {
		frac = frac[:2]
	}
	if frac == "" {
		return whole.String()
	}
	return whole.String() + "." + frac
}
