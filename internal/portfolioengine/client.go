// Package portfolioengine is the portfolio engine client. The API key is tenant-wide — it can
// create strategies and withdraw from any portfolio in the tenant — so it
// lives in this process only and no response ever carries it.
//
// Docs: https://portfolio-engine.example.com
package portfolioengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrNotConfigured = errors.New("portfolioEngine is not configured (PORTFOLIO_ENGINE_API_KEY unset)")

// Error is a failed portfolio engine call with the upstream code and message verbatim.
type Error struct {
	Status     int
	Code       string
	Message    string
	Details    []string
	Method     string
	Path       string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("portfolioEngine %s %s: %s", e.Method, e.Path, e.Message)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	if len(e.Details) > 0 {
		msg += ": " + strings.Join(e.Details, "; ")
	}
	return msg
}

// NotFound reports whether the portfolio engine says the portfolio no longer exists.
func (e *Error) NotFound() bool {
	m := strings.ToLower(e.Message)
	return e.Status == http.StatusNotFound ||
		strings.Contains(m, "unknown portfolio") ||
		strings.Contains(m, "portfolio not found") ||
		strings.Contains(m, "portfolio_not_found")
}

type Options struct {
	BaseURL    string
	APIKey     string
	Logger     *slog.Logger
	HTTPClient *http.Client
	// RetryDelay is the wait before a read's first retry; it triples after
	// each. Default 300ms.
	RetryDelay time.Duration
}

type Client struct {
	baseURL    string
	apiKey     string
	http       *http.Client
	log        *slog.Logger
	retryDelay time.Duration
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
	delay := o.RetryDelay
	if delay <= 0 {
		delay = 300 * time.Millisecond
	}
	return &Client{baseURL: o.BaseURL, apiKey: o.APIKey, http: hc, log: log.With("component", "portfolioEngine"), retryDelay: delay}
}

func (c *Client) Configured() bool { return c.apiKey != "" }

/* ── Types ──────────────────────────────────────────────────────────── */

type AllocationAsset struct {
	AssetID string `json:"assetId"`
	Weight  string `json:"weight"` // percent, up to 2 decimals; all weights sum to 100
}

type Allocation struct {
	Assets []AllocationAsset `json:"assets"`
}

type Schedule struct {
	Type      string `json:"type"` // "interval" | "cron"
	Frequency string `json:"frequency,omitempty"`
	Cron      string `json:"cron,omitempty"`
	Timezone  string `json:"timezone,omitempty"`
}

type SwapPreferences struct {
	SlippageBps    int    `json:"slippageBps,omitempty"`
	PriceImpactBps int    `json:"priceImpactBps,omitempty"`
	ThresholdUsd   string `json:"thresholdUsd,omitempty"`
}

type Preferences struct {
	Swap SwapPreferences `json:"swap"`
}

type CreateStrategyRequest struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	IsPublic    bool         `json:"isPublic"`
	Allocation  Allocation   `json:"allocation"`
	Schedule    Schedule     `json:"schedule"`
	Preferences *Preferences `json:"preferences,omitempty"`
}

type CreateStrategyResponse struct {
	StrategyID string `json:"strategyId"`
	Version    int    `json:"version"`
}

type PublishVersionResponse struct {
	StrategyID string `json:"strategyId"`
	Version    int    `json:"version"`
}

type EnrollSignatureRequest struct {
	OwnerAccountID string  `json:"ownerAccountId"`
	StrategyID     string  `json:"strategyId"`
	ChainIDs       []int64 `json:"chainIds"`
	AccountType    string  `json:"accountType,omitempty"`
}

// EnrollMessage is what the user's wallet signs. ECDSA enrollment returns
// {kind:"ecdsa", raw:<32-byte hex>} signed via EIP-191 personal_sign.
type EnrollMessage struct {
	Kind      string          `json:"kind"`
	Raw       string          `json:"raw,omitempty"`
	TypedData json.RawMessage `json:"typedData,omitempty"`
}

type EnrollSignatureResponse struct {
	FlowID         string        `json:"flowId"`
	AccountIndex   string        `json:"accountIndex"`
	AgentAccountID string        `json:"agentAccountId"`
	AccountType    string        `json:"accountType,omitempty"`
	Message        EnrollMessage `json:"message"`
}

type EnrollRequest struct {
	OwnerAccountID string  `json:"ownerAccountId"`
	StrategyID     string  `json:"strategyId"`
	ChainIDs       []int64 `json:"chainIds"`
	AccountType    string  `json:"accountType,omitempty"`
	AccountIndex   string  `json:"accountIndex"`
	AgentAccountID string  `json:"agentAccountId"`
	FlowID         string  `json:"flowId"`
	Signature      string  `json:"signature"`
	PortfolioName  string  `json:"portfolioName,omitempty"`
}

type SmartAccount struct {
	AccountID        string `json:"accountId"`
	DepositAccountID string `json:"depositAccountId,omitempty"`
}

type EnrollResponse struct {
	PortfolioID   string         `json:"portfolioId"`
	StrategyID    string         `json:"strategyId"`
	SmartAccounts []SmartAccount `json:"smartAccounts"`
}

type OperationRef struct {
	OperationID string `json:"operationId"`
	SubmittedAt string `json:"submittedAt"`
}

type WithdrawAsset struct {
	AssetID   string `json:"assetId"`
	AmountRaw string `json:"amountRaw"`
}

type WithdrawSignatureRequest struct {
	RecipientAccountID string          `json:"recipientAccountId"`
	Assets             []WithdrawAsset `json:"assets"`
	Liquidate          bool            `json:"liquidate"`
	SettlementAssetID  string          `json:"settlementAssetId,omitempty"`
}

type LiquidateAllSignatureRequest struct {
	RecipientAccountID string `json:"recipientAccountId"`
	SettlementAssetID  string `json:"settlementAssetId,omitempty"`
}

type SignedAuthorization struct {
	Message   json.RawMessage `json:"message"`
	Signature string          `json:"signature"`
}

/* ── Endpoints ──────────────────────────────────────────────────────── */

func (c *Client) CreateStrategy(ctx context.Context, r CreateStrategyRequest) (*CreateStrategyResponse, error) {
	var out CreateStrategyResponse
	return &out, c.do(ctx, http.MethodPost, "/strategies", r, &out)
}

// PublishVersion replaces a strategy's allocation. Enrolled portfolios move
// to it on their next scheduled rebalance.
func (c *Client) PublishVersion(ctx context.Context, strategyID string, a Allocation, changeLog string) (*PublishVersionResponse, error) {
	body := map[string]any{"allocation": a}
	if changeLog != "" {
		body["changeLog"] = truncate(changeLog, 500)
	}
	var out PublishVersionResponse
	return &out, c.do(ctx, http.MethodPost, "/strategies/"+url.PathEscape(strategyID)+"/versions", body, &out)
}

func (c *Client) EnrollSignature(ctx context.Context, r EnrollSignatureRequest) (*EnrollSignatureResponse, error) {
	var out EnrollSignatureResponse
	return &out, c.do(ctx, http.MethodPost, "/enroll/signature", r, &out)
}

func (c *Client) Enroll(ctx context.Context, r EnrollRequest) (*EnrollResponse, error) {
	var out EnrollResponse
	return &out, c.do(ctx, http.MethodPost, "/enroll", r, &out)
}

func (c *Client) Portfolio(ctx context.Context, id string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, portfolioPath(id, ""), nil, &out)
}

func (c *Client) Positions(ctx context.Context, id string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, portfolioPath(id, "/positions"), nil, &out)
}

// Start resumes (or begins) a portfolio's rebalance schedule. Idempotent.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, portfolioPath(id, "/start"), nil, nil)
}

// Stop pauses a portfolio's rebalance schedule, keeping its history and
// on-chain state. Idempotent; the portfolio engine answers 400 when there is no schedule.
func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, portfolioPath(id, "/stop"), nil, nil)
}

// Rebalance dispatches an out-of-schedule run. The portfolio engine enforces a per-portfolio
// cooldown and answers 429 with Retry-After inside it.
func (c *Client) Rebalance(ctx context.Context, id string) (*OperationRef, error) {
	var out OperationRef
	return &out, c.do(ctx, http.MethodPost, portfolioPath(id, "/rebalance"), nil, &out)
}

func (c *Client) Operation(ctx context.Context, id, opID string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodGet, portfolioPath(id, "/operations/"+url.PathEscape(opID)), nil, &out)
}

// PerformancePoint is one UTC day of a portfolio's curve. Amounts are decimal
// strings in USD; CashFlowUsd is that day's net external flow (positive for
// deposits, negative for withdrawals).
type PerformancePoint struct {
	Date          string  `json:"date"`
	PercentChange *string `json:"percentChange"`
	ValueUsd      string  `json:"valueUsd"`
	CashFlowUsd   string  `json:"cashFlowUsd"`
	IsLive        bool    `json:"isLive"`
}

type Performance struct {
	PortfolioID string `json:"portfolioId"`
	Meta        struct {
		Method string `json:"method"`
		AsOf   string `json:"asOf"`
	} `json:"meta"`
	Points []PerformancePoint `json:"points"`
}

// Performance is the portfolio's daily value curve with its cash flows. The
// money-weighted return (MWR) reflects the user's own deposits and
// withdrawals, which is what "how much have I made" means.
func (c *Client) Performance(ctx context.Context, id string) (*Performance, error) {
	var out Performance
	return &out, c.do(ctx, http.MethodGet, portfolioPath(id, "/performance?returnMethod=MWR"), nil, &out)
}

func (c *Client) WithdrawSignature(ctx context.Context, id string, r WithdrawSignatureRequest) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodPost, portfolioPath(id, "/withdraw/signature"), r, &out)
}

func (c *Client) Withdraw(ctx context.Context, id string, r SignedAuthorization) (*OperationRef, error) {
	var out OperationRef
	return &out, c.do(ctx, http.MethodPost, portfolioPath(id, "/withdraw"), r, &out)
}

func (c *Client) LiquidateAllSignature(ctx context.Context, id string, r LiquidateAllSignatureRequest) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, http.MethodPost, portfolioPath(id, "/liquidate-all/signature"), r, &out)
}

func (c *Client) LiquidateAll(ctx context.Context, id string, r SignedAuthorization) (*OperationRef, error) {
	var out OperationRef
	return &out, c.do(ctx, http.MethodPost, portfolioPath(id, "/liquidate-all"), r, &out)
}

func portfolioPath(id, suffix string) string {
	return "/portfolios/" + url.PathEscape(id) + suffix
}

/* ── Transport ──────────────────────────────────────────────────────── */

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    string   `json:"code"`
		Message string   `json:"message"`
		Details []string `json:"details"`
	} `json:"error"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("portfolioEngine: encode %s: %w", path, err)
		}
	}
	// Reads are retried through brief upstream failures. Writes never are: a
	// write that timed out may still have happened on the portfolio engine's side.
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	delay := c.retryDelay
	for attempt := 1; ; attempt++ {
		err := c.once(ctx, method, path, payload, out)
		var ge *Error
		if err == nil || attempt == attempts || !errors.As(err, &ge) || !ge.transient() {
			return err
		}
		c.log.Warn("portfolioEngine read failed; retrying", "path", path, "attempt", attempt, "err", err)
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return err
		}
		delay *= 3
	}
}

// transient is a failure of the upstream or the network, not of the request.
func (e *Error) transient() bool {
	switch e.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (c *Client) once(ctx context.Context, method, path string, payload []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("accept", "application/json")
	if payload != nil {
		req.Header.Set("content-type", "application/json")
	}

	started := time.Now()
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &Error{Status: http.StatusGatewayTimeout, Method: method, Path: path, Message: "request timed out"}
		}
		return &Error{Status: http.StatusBadGateway, Method: method, Path: path, Message: "network error: " + err.Error()}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return &Error{Status: http.StatusBadGateway, Method: method, Path: path, Message: "reading response: " + err.Error()}
	}
	c.log.Debug("portfolioEngine request", "method", method, "path", path, "status", res.StatusCode, "elapsed", time.Since(started))

	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return &Error{Status: http.StatusBadGateway, Code: "PORTFOLIO_ENGINE_INVALID_RESPONSE", Method: method, Path: path,
			Message: fmt.Sprintf("non-JSON response (HTTP %d)", res.StatusCode)}
	}
	if !env.Success || res.StatusCode >= 400 {
		e := &Error{Status: res.StatusCode, Method: method, Path: path, Message: "request failed"}
		if e.Status < 400 {
			e.Status = http.StatusBadGateway
		}
		if env.Error != nil {
			e.Code, e.Message, e.Details = env.Error.Code, env.Error.Message, env.Error.Details
		}
		if s, err := strconv.Atoi(res.Header.Get("retry-after")); err == nil && s > 0 {
			e.RetryAfter = time.Duration(s) * time.Second
		}
		return e
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], env.Data...)
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return &Error{Status: http.StatusBadGateway, Code: "PORTFOLIO_ENGINE_INVALID_RESPONSE", Method: method, Path: path,
			Message: "unexpected response shape: " + err.Error()}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
