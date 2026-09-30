package tokens

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Check is one on-chain verification of a registry entry.
type Check struct {
	Chain    string
	Ticker   string
	Address  string
	Want     string
	Got      string
	Decimals int
	WantDec  int // -1 when any value is acceptable
	Err      error
}

func (c Check) OK() bool {
	return c.Err == nil && c.Got == c.Want && (c.WantDec < 0 || c.Decimals == c.WantDec)
}

// Verify reads symbol() and decimals() for every mapped token and every cash
// asset from each chain's public RPC. A registry entry whose on-chain symbol
// disagrees is a mapping error — the address points at the wrong contract.
func Verify(ctx context.Context, hc *http.Client) []Check {
	type job struct {
		chain                 Chain
		ticker, address, want string
		decimals              int
	}
	var jobs []job
	for _, chain := range Chains {
		jobs = append(jobs, job{chain, chain.Cash.Symbol, chain.Cash.Address, chain.Cash.Symbol, chain.Cash.Decimals})
		for _, a := range All() {
			// Registries are chain-specific; a BNB-only asset has no Ethereum
			// mapping to verify and must not produce a false failure.
			if addr, ok := a.Address(chain.ID); ok {
				jobs = append(jobs, job{chain, a.Ticker, addr, a.OndoSymbol, -1})
			}
		}
	}
	out := make([]Check, len(jobs))
	queue := make(chan int)
	var workers sync.WaitGroup
	for range min(6, len(jobs)) {
		workers.Go(func() {
			for i := range queue {
				j := jobs[i]
				out[i] = check(ctx, hc, j.chain, j.ticker, j.address, j.want, j.decimals)
				if out[i].Err != nil && ctx.Err() == nil {
					out[i] = check(ctx, hc, j.chain, j.ticker, j.address, j.want, j.decimals)
				}
			}
		})
	}
	for i := range jobs {
		queue <- i
	}
	close(queue)
	workers.Wait()
	return out
}

func check(ctx context.Context, hc *http.Client, chain Chain, ticker, addr, want string, wantDec int) Check {
	c := Check{Chain: chain.Name, Ticker: ticker, Address: addr, Want: want, WantDec: wantDec}
	sym, err := ethCall(ctx, hc, chain.rpc, addr, "0x95d89b41") // symbol()
	if err != nil {
		c.Err = err
		return c
	}
	c.Got = decodeString(sym)
	dec, err := ethCall(ctx, hc, chain.rpc, addr, "0x313ce567") // decimals()
	if err != nil {
		c.Err = err
		return c
	}
	if len(dec) > 0 {
		c.Decimals = int(new(big.Int).SetBytes(dec).Int64())
	}
	return c
}

func ethCall(ctx context.Context, hc *http.Client, rpc, to, data string) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []any{map[string]string{"to": to, "data": data}, "latest"},
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
		Result string `json:"result"`
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
	return hex.DecodeString(strings.TrimPrefix(out.Result, "0x"))
}

// decodeString decodes an ABI string return, falling back to bytes32 for the
// few tokens that return a fixed-width symbol.
func decodeString(b []byte) string {
	if len(b) >= 64 {
		offset := new(big.Int).SetBytes(b[:32]).Int64()
		if offset+32 <= int64(len(b)) {
			n := new(big.Int).SetBytes(b[offset : offset+32]).Int64()
			if start := offset + 32; n >= 0 && start+n <= int64(len(b)) {
				return string(b[start : start+n])
			}
		}
	}
	return strings.TrimRight(string(b), "\x00")
}
