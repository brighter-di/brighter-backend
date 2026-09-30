// Package backtestengine is the backtest engine API client. The key pair grants the same
// access as the user who created it, so it lives in this process only and
// nothing in here is ever serialised to a client.
//
// Docs: https://backtest-engine.example.com
package backtestengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var ErrNotConfigured = errors.New("backtestEngine credentials are not configured (BACKTEST_ENGINE_API_KEY_ID / BACKTEST_ENGINE_API_SECRET)")

// Error is a failed backtest engine call. Detail is the backtest engine's own reason, verbatim:
// a swallowed upstream error is how a wrong allocation reaches a trade.
type Error struct {
	Status int
	Method string
	Path   string
	Detail string
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("backtestEngine %s %s: %s", e.Method, e.Path, e.Detail)
	}
	return fmt.Sprintf("backtestEngine %d on %s %s: %s", e.Status, e.Method, e.Path, e.Detail)
}

type Options struct {
	BaseURL    string
	KeyID      string
	Secret     string
	Logger     *slog.Logger
	HTTPClient *http.Client
}

type Client struct {
	baseURL string
	keyID   string
	secret  string
	http    *http.Client
	log     *slog.Logger

	// Rate limits are per endpoint (~25/min for most). Calls are spaced by a
	// floor gap and 429s are retried with the server's Retry-After.
	mu       sync.Mutex
	lastCall time.Time
	minGap   time.Duration
}

func New(o Options) *Client {
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		baseURL: o.BaseURL,
		keyID:   o.KeyID,
		secret:  o.Secret,
		http:    hc,
		log:     log.With("component", "backtestEngine"),
		minGap:  150 * time.Millisecond,
	}
}

func (c *Client) Configured() bool { return c.keyID != "" && c.secret != "" }

type BacktestRequest struct {
	Tree      map[string]any
	StartDate string // YYYY-MM-DD
	EndDate   string // YYYY-MM-DD
	Benchmark string
}

// Backtest runs an inline strategy tree — nothing is saved on the account.
func (c *Client) Backtest(ctx context.Context, r BacktestRequest) (*Backtest, error) {
	body := map[string]any{
		// `strategyTree` is an envelope and all three keys are required; the backtest engine
		// reads the tree from raw_value and only presence-checks the others.
		// Omitting them is a malli coercion 400 (verified against the live API).
		"strategyTree": map[string]any{
			"raw_value":     r.Tree,
			"encoding_type": "none",
			"encoded_value": "",
		},
		"capital":            100000,
		"apply_reg_fee":      true,
		"apply_taf_fee":      true,
		"apply_cat_fee":      true,
		"apply_subscription": "none",
		"slippage_percent":   0.0005,
		"start_date":         r.StartDate,
		"end_date":           r.EndDate,
	}
	if r.Benchmark != "" {
		body["benchmark_tickers"] = []string{r.Benchmark}
	}
	data, err := c.do(ctx, http.MethodPost, "/v1/backtest", body, 150*time.Second)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Method: http.MethodPost, Path: "/v1/backtest", Detail: "response is not a JSON object"}
	}
	return parse(raw, r.Benchmark)
}

func (c *Client) throttle(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := time.Until(c.lastCall.Add(c.minGap)); wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.lastCall = time.Now()
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, timeout time.Duration) ([]byte, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("backtestEngine: encode %s: %w", path, err)
		}
	}

	const maxRetries = 2
	for attempt := 0; ; attempt++ {
		if err := c.throttle(ctx); err != nil {
			return nil, err
		}
		data, status, retryAfter, err := c.once(ctx, method, path, payload, timeout)
		if err != nil {
			return nil, err
		}
		if status == http.StatusTooManyRequests && attempt < maxRetries {
			if retryAfter <= 0 {
				retryAfter = time.Duration(attempt+1) * 2 * time.Second
			}
			c.log.Warn("rate limited; retrying", "path", path, "wait", retryAfter)
			t := time.NewTimer(retryAfter)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			}
			continue
		}
		if status >= 400 {
			return nil, &Error{Status: status, Method: method, Path: path, Detail: errorDetail(data)}
		}
		return data, nil
	}
}

func (c *Client) once(ctx context.Context, method, path string, payload []byte, timeout time.Duration) ([]byte, int, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("x-api-key-id", c.keyID)
	req.Header.Set("authorization", "Bearer "+c.secret)
	req.Header.Set("accept", "application/json")
	if payload != nil {
		req.Header.Set("content-type", "application/json")
	}

	started := time.Now()
	res, err := c.http.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, 0, 0, &Error{Status: http.StatusGatewayTimeout, Method: method, Path: path, Detail: fmt.Sprintf("timed out after %s", timeout)}
		}
		if ctx.Err() != nil {
			return nil, 0, 0, ctx.Err()
		}
		return nil, 0, 0, &Error{Method: method, Path: path, Detail: "network error: " + err.Error()}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, 0, 0, &Error{Status: http.StatusBadGateway, Method: method, Path: path, Detail: "reading response: " + err.Error()}
	}
	c.log.Debug("backtestEngine request", "method", method, "path", path, "status", res.StatusCode, "elapsed", time.Since(started))

	var retryAfter time.Duration
	if s, err := strconv.Atoi(res.Header.Get("retry-after")); err == nil && s > 0 {
		retryAfter = time.Duration(s) * time.Second
	}
	return data, res.StatusCode, retryAfter, nil
}

// errorDetail pulls the backtest engine's reason out of an error body. Coercion errors
// echo the whole submitted payload back under `value`, which buries the cause
// past any truncation, so the echo is dropped.
func errorDetail(data []byte) string {
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err == nil {
		for _, k := range []string{"humanized", "errors", "message", "error", "detail"} {
			if v, ok := parsed[k]; ok && v != nil {
				if s, ok := v.(string); ok {
					return truncate(s, 900)
				}
				b, _ := json.Marshal(v)
				return truncate(string(b), 900)
			}
		}
		delete(parsed, "value")
		b, _ := json.Marshal(parsed)
		return truncate(string(b), 900)
	}
	return truncate(string(data), 900)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
