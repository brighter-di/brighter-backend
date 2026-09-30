// Package prices fetches daily closing prices for the stocks and ETFs
// Brighter's strategies hold. It uses Nasdaq's public quote
// API: no key, and it answers from a server where Yahoo rate-limits and
// Stooq demands a browser challenge.
package prices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Error is a failed price lookup. The detail is for logs, not users.
type Error struct {
	Status int
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("prices %d: %s", e.Status, e.Detail) }

// transient is a failure worth another try: rate limiting, or the server or
// the network failing rather than the request.
func (e *Error) transient() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

// Class is Nasdaq's asset class for a symbol.
type Class string

const (
	Stock Class = "stocks"
	ETF   Class = "etf"
)

type Bar struct {
	Date  string // YYYY-MM-DD, the trading day
	Close float64
}

type Options struct {
	BaseURL string
	// IndexedURL selects the independent Go indexer's local history API.
	IndexedURL string
	Logger     *slog.Logger
	HTTPClient *http.Client
	// RetryDelay is the wait before the first retry; it triples after each.
	// Default 500ms.
	RetryDelay time.Duration
}

type Client struct {
	base       string
	indexed    bool
	http       *http.Client
	log        *slog.Logger
	retryDelay time.Duration
}

func New(o Options) *Client {
	base := strings.TrimRight(o.BaseURL, "/")
	if o.IndexedURL != "" {
		base = strings.TrimRight(o.IndexedURL, "/")
	}
	if base == "" {
		base = "https://api.nasdaq.com"
	}
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
		delay = 500 * time.Millisecond
	}
	return &Client{base: base, indexed: o.IndexedURL != "", http: hc, log: log.With("component", "prices"), retryDelay: delay}
}

// History returns daily closes between from and to, oldest first.
func (c *Client) History(ctx context.Context, ticker string, class Class, from, to time.Time) ([]Bar, error) {
	if c.indexed {
		return c.indexedHistory(ctx, ticker, from, to)
	}
	q := url.Values{
		"assetclass": {string(class)},
		"fromdate":   {from.Format("2006-01-02")},
		"todate":     {to.Format("2006-01-02")},
		"limit":      {"3000"},
	}
	var body struct {
		Data *struct {
			TradesTable struct {
				Rows []struct {
					Date  string `json:"date"`
					Close string `json:"close"`
				} `json:"rows"`
			} `json:"tradesTable"`
		} `json:"data"`
		Status struct {
			BCodeMessage []struct {
				ErrorMessage string `json:"errorMessage"`
			} `json:"bCodeMessage"`
		} `json:"status"`
	}
	if err := c.get(ctx, "/api/quote/"+url.PathEscape(ticker)+"/historical?"+q.Encode(), &body); err != nil {
		return nil, err
	}
	if body.Data == nil {
		msg := "no history"
		if len(body.Status.BCodeMessage) > 0 {
			msg = body.Status.BCodeMessage[0].ErrorMessage
		}
		return nil, &Error{Status: http.StatusBadGateway, Detail: ticker + ": " + msg}
	}
	bars := make([]Bar, 0, len(body.Data.TradesTable.Rows))
	for _, r := range body.Data.TradesTable.Rows {
		d, err := time.Parse("01/02/2006", r.Date)
		if err != nil {
			continue
		}
		v, ok := number(r.Close)
		if !ok || v <= 0 {
			continue
		}
		bars = append(bars, Bar{Date: d.Format("2006-01-02"), Close: v})
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	return bars, nil
}

// get fetches path in up to three attempts. The source is unofficial and
// blips; a second try usually lands.
func (c *Client) get(ctx context.Context, path string, out any) error {
	const attempts = 3
	delay := c.retryDelay
	for attempt := 1; ; attempt++ {
		err := c.once(ctx, path, out)
		var pe *Error
		if err == nil || attempt == attempts || !errors.As(err, &pe) || !pe.transient() {
			return err
		}
		c.log.Warn("prices request failed; retrying", "path", path, "attempt", attempt, "err", err)
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

func (c *Client) once(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	// The API serves browsers; without these it answers slowly or not at all.
	req.Header.Set("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36")
	req.Header.Set("accept", "application/json")
	req.Header.Set("origin", "https://www.nasdaq.com")
	req.Header.Set("referer", "https://www.nasdaq.com/")

	started := time.Now()
	res, err := c.http.Do(req)
	if err != nil {
		return &Error{Status: http.StatusBadGateway, Detail: "request failed: " + err.Error()}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return &Error{Status: http.StatusBadGateway, Detail: "reading response: " + err.Error()}
	}
	c.log.Debug("prices request", "path", path, "status", res.StatusCode, "elapsed", time.Since(started))
	if res.StatusCode != http.StatusOK {
		snippet := string(data)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return &Error{Status: res.StatusCode, Detail: snippet}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Status: http.StatusBadGateway, Detail: "non-JSON response"}
	}
	return nil
}

// number parses Nasdaq's display strings: "$1,234.56", "+1.23", "-0.45%".
func number(s string) (float64, bool) {
	s = strings.NewReplacer("$", "", ",", "", "%", "", "+", "").Replace(strings.TrimSpace(s))
	if s == "" || s == "N/A" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}
