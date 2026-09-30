package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brighter-di/backend/internal/auth"
	"github.com/brighter-di/backend/internal/portfolioengine"
	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/strategy"
	"github.com/brighter-di/backend/internal/tokens"
)

type PortfolioView struct {
	PortfolioID    string          `json:"portfolioId"`
	Owner          string          `json:"owner"`
	ChainID        int64           `json:"chainId"`
	DepositAddress string          `json:"depositAddress"`
	CreatedAt      time.Time       `json:"createdAt"`
	TemplateID     string          `json:"templateId"`
	TemplateName   string          `json:"templateName"`
	Params         strategy.Params `json:"params"`
	ParamsLabel    string          `json:"paramsLabel"`
	StrategyID     string          `json:"strategyId"`
	// The allocation currently published to the portfolio's portfolio engine strategy
	// and the data cutoff it was computed from.
	Allocation []TargetLine `json:"allocation"`
	Cutoff     string       `json:"cutoff"`
	Version    int          `json:"version"`
	// Set only on an archived portfolio: when its owner retired it.
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
}

func (s *Service) portfolioView(ctx context.Context, p *store.Portfolio) *PortfolioView {
	v := &PortfolioView{
		PortfolioID: p.PortfolioID, Owner: p.Owner, ChainID: p.ChainID, DepositAddress: p.DepositAddress,
		CreatedAt: p.CreatedAt, TemplateID: p.Strategy.TemplateID, TemplateName: p.Strategy.TemplateID,
		Params: p.Strategy.Params, ParamsLabel: p.Strategy.ParamsKey, StrategyID: p.Strategy.PortfolioEngineStrategyID,
		Allocation: targetLines(p.Strategy.Allocation, nil),
		Cutoff:     p.Strategy.Cutoff.Format(dateFmt), Version: p.Strategy.Version,
	}
	if tpl, err := s.Template(ctx, p.Strategy.TemplateID); err == nil {
		v.TemplateName = tpl.Name
		v.ParamsLabel = tpl.Label(p.Strategy.Params)
	}
	return v
}

// ListPortfolios returns an owner's live portfolios, or the ones they have
// archived. An archived portfolio holds nothing and runs nothing — archiving
// requires it to be empty — so it is kept for its history, not for its balance.
func (s *Service) ListPortfolios(ctx context.Context, owner string, archived bool) ([]*PortfolioView, error) {
	owner, err := normaliseOwner(owner)
	if err != nil {
		return nil, err
	}
	// The app lists a wallet's portfolios as soon as it connects, so this is
	// where a new wallet first shows up.
	if first, err := s.store.RecordWallet(ctx, owner); err != nil {
		s.log.Warn("wallet not recorded", "err", err)
	} else if first {
		s.announce("New wallet connected: "+shortAddr(owner), "A new wallet connected.")
	}
	rows, err := s.store.ListPortfolios(ctx, owner, archived)
	if err != nil {
		return nil, err
	}
	out := make([]*PortfolioView, len(rows))
	for i, p := range rows {
		out[i] = s.portfolioView(ctx, p)
		out[i].ArchivedAt = p.DroppedAt
	}
	return out, nil
}

// ScheduleView is a portfolio's automation: whether rebalancing runs, how
// often, and when it last and next ran.
type ScheduleView struct {
	Status          string  `json:"status"` // active | paused
	Frequency       *string `json:"frequency"`
	NextDueAt       *string `json:"nextDueAt"`
	LastRebalanceAt *string `json:"lastRebalanceAt"`
}

type PortfolioDetail struct {
	*PortfolioView
	// Nil when trading is not configured or the record has no schedule.
	Schedule *ScheduleView `json:"schedule"`
}

func (s *Service) Portfolio(ctx context.Context, id string) (*PortfolioDetail, error) {
	p, err := s.knownPortfolio(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &PortfolioDetail{PortfolioView: s.portfolioView(ctx, p)}
	if s.portfolioEngine.Configured() {
		raw, err := s.guard(ctx, id, func() (json.RawMessage, error) { return s.portfolioEngine.Portfolio(ctx, id) })
		if err != nil {
			return nil, err
		}
		// Only the schedule leaves this process; the rest of the upstream
		// record (names, owner ids, descriptions) is not the browser's.
		var rec struct {
			Schedule *ScheduleView `json:"schedule"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			s.log.Warn("portfolio record has an unexpected shape", "portfolio", id, "err", err)
		}
		d.Schedule = rec.Schedule
	}
	return d, nil
}

func (s *Service) Positions(ctx context.Context, id string) (json.RawMessage, error) {
	if _, err := s.knownPortfolio(ctx, id); err != nil {
		return nil, err
	}
	return s.guard(ctx, id, func() (json.RawMessage, error) { return s.portfolioEngine.Positions(ctx, id) })
}

// Deploy starts automation and asks for an immediate rebalance, so a fresh
// deposit is invested now instead of at the next scheduled run.
func (s *Service) Deploy(ctx context.Context, id string) (*portfolioengine.OperationRef, error) {
	p, err := s.knownPortfolio(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.portfolioEngine.Start(ctx, id); err != nil {
		var ge *portfolioengine.Error
		if errors.As(err, &ge) && ge.NotFound() {
			_ = s.store.MarkPortfolioDropped(ctx, id)
			return nil, err
		}
		s.log.Warn("start automation failed before deploy", "portfolio", id, "err", err)
	}
	var op *portfolioengine.OperationRef
	_, err = s.guard(ctx, id, func() (json.RawMessage, error) {
		var err error
		op, err = s.portfolioEngine.Rebalance(ctx, id)
		return nil, err
	})
	if err == nil {
		v := s.portfolioView(ctx, p)
		s.announce(fmt.Sprintf("Rebalance requested: %s (%s), portfolio %s of %s.", v.TemplateName, v.ParamsLabel, id, shortAddr(p.Owner)),
			fmt.Sprintf("Rebalance requested: %s (%s).", v.TemplateName, v.ParamsLabel))
	}
	return op, err
}

func (s *Service) Operation(ctx context.Context, id, opID string) (json.RawMessage, error) {
	if _, err := s.knownPortfolio(ctx, id); err != nil {
		return nil, err
	}
	return s.guard(ctx, id, func() (json.RawMessage, error) { return s.portfolioEngine.Operation(ctx, id, opID) })
}

/* ── Performance ────────────────────────────────────────────────────── */

// PerformancePoint is one day of a portfolio: its value, the money put in so
// far net of withdrawals, and the difference.
type PerformancePoint struct {
	Date      string  `json:"date"`
	Value     float64 `json:"value"`
	Deposited float64 `json:"deposited"`
	Profit    float64 `json:"profit"`
	// Money-weighted return to this day as a fraction; nil when unavailable.
	Return *float64 `json:"return"`
	Live   bool     `json:"live"`
}

type PerformanceView struct {
	Method string             `json:"method"`
	AsOf   string             `json:"asOf"`
	Points []PerformancePoint `json:"points"`
}

// The curve's live point moves with prices; the page needs it about as fresh
// as the positions it sits beside.
const performanceTTL = time.Minute

// Performance is a portfolio's daily value curve with its profit. The source
// reports each day's value and net cash flow; deposits accumulate from those.
func (s *Service) Performance(ctx context.Context, id string) (*PerformanceView, error) {
	if _, err := s.knownPortfolio(ctx, id); err != nil {
		return nil, err
	}
	v, err := s.cached(ctx, "performance|"+id, cachePolicy{ttl: performanceTTL}, func(ctx context.Context) (any, error) {
		var perf *portfolioengine.Performance
		if _, err := s.guard(ctx, id, func() (json.RawMessage, error) {
			var err error
			perf, err = s.portfolioEngine.Performance(ctx, id)
			return nil, err
		}); err != nil {
			return nil, err
		}
		view := &PerformanceView{Method: perf.Meta.Method, AsOf: perf.Meta.AsOf, Points: make([]PerformancePoint, 0, len(perf.Points))}
		deposited := 0.0
		for _, p := range perf.Points {
			value := parseAmount(p.ValueUsd)
			deposited += parseAmount(p.CashFlowUsd)
			pt := PerformancePoint{Date: p.Date, Value: value, Deposited: deposited, Profit: value - deposited, Live: p.IsLive}
			if p.PercentChange != nil {
				if f, err := strconv.ParseFloat(*p.PercentChange, 64); err == nil {
					r := f / 100
					pt.Return = &r
				}
			}
			view.Points = append(view.Points, pt)
		}
		return view, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*PerformanceView), nil
}

// Since keeps the points dated on or after date (YYYY-MM-DD). Deposits and
// returns are still accumulated over the whole history, so the points are
// the same ones a full response carries.
func (p PerformanceView) Since(date string) PerformanceView {
	i := slices.IndexFunc(p.Points, func(pt PerformancePoint) bool { return pt.Date >= date })
	if i < 0 {
		i = len(p.Points)
	}
	p.Points = p.Points[i:]
	return p
}

// parseAmount reads the source's decimal strings; anything unparseable is 0.
func parseAmount(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

var digits = regexp.MustCompile(`^[0-9]+$`)

type WithdrawRequest struct {
	// "liquidate": swap every holding to the chain's cash asset and send it to
	// the owner. "withdraw": send the listed assets to the owner as they are.
	Mode   string                          `json:"mode"`
	Assets []portfolioengine.WithdrawAsset `json:"assets"`
}

// WithdrawSignature prepares the authorization the owner signs. Proceeds only
// ever go to the portfolio's owner, on the portfolio's chain.
func (s *Service) WithdrawSignature(ctx context.Context, id string, req WithdrawRequest) (json.RawMessage, error) {
	p, err := s.knownPortfolio(ctx, id)
	if err != nil {
		return nil, err
	}
	chain, ok := tokens.ChainByID(p.ChainID)
	if !ok {
		return nil, errf(500, "UNSUPPORTED_CHAIN", "portfolio %s is on unsupported chain %d", id, p.ChainID)
	}
	recipient := chain.AccountID(p.Owner)
	switch req.Mode {
	case "liquidate":
		return s.guard(ctx, id, func() (json.RawMessage, error) {
			return s.portfolioEngine.LiquidateAllSignature(ctx, id, portfolioengine.LiquidateAllSignatureRequest{
				RecipientAccountID: recipient,
				// Ondo redemption settles only in the chain's Ondo cash asset.
				SettlementAssetID: chain.CashAssetID(),
			})
		})
	case "withdraw":
		if len(req.Assets) == 0 {
			return nil, errf(400, "BAD_WITHDRAWAL", "assets are required for an in-kind withdrawal")
		}
		prefix := chain.AssetID("")
		for _, a := range req.Assets {
			if !strings.HasPrefix(strings.ToLower(a.AssetID), prefix) || !digits.MatchString(a.AmountRaw) || strings.TrimLeft(a.AmountRaw, "0") == "" {
				return nil, errf(400, "BAD_WITHDRAWAL", "each asset needs a %s asset id and a positive integer amountRaw", chain.Name)
			}
		}
		return s.guard(ctx, id, func() (json.RawMessage, error) {
			return s.portfolioEngine.WithdrawSignature(ctx, id, portfolioengine.WithdrawSignatureRequest{
				RecipientAccountID: recipient, Assets: req.Assets, Liquidate: false,
			})
		})
	}
	return nil, errf(400, "BAD_WITHDRAWAL", `mode must be "liquidate" or "withdraw"`)
}

type WithdrawSubmit struct {
	Mode      string          `json:"mode"`
	Message   json.RawMessage `json:"message"`
	Signature string          `json:"signature"`
}

func (s *Service) Withdraw(ctx context.Context, id string, req WithdrawSubmit) (*portfolioengine.OperationRef, error) {
	p, err := s.knownPortfolio(ctx, id)
	if err != nil {
		return nil, err
	}
	if !hexSig.MatchString(req.Signature) || len(req.Message) == 0 {
		return nil, errf(400, "BAD_WITHDRAWAL", "message and a 0x-prefixed signature are required")
	}
	defer func() {
		if err != nil {
			return
		}
		what := "withdraw tokens"
		if req.Mode == "liquidate" {
			what = "sell everything"
			if chain, ok := tokens.ChainByID(p.ChainID); ok {
				what += " to " + chain.Cash.Symbol
			}
		}
		v := s.portfolioView(ctx, p)
		s.announce(fmt.Sprintf("Withdrawal submitted: %s from %s (%s), portfolio %s of %s.", what, v.TemplateName, v.ParamsLabel, id, shortAddr(p.Owner)),
			fmt.Sprintf("Withdrawal submitted: %s from %s (%s).", what, v.TemplateName, v.ParamsLabel))
	}()
	body := portfolioengine.SignedAuthorization{Message: req.Message, Signature: req.Signature}
	var op *portfolioengine.OperationRef
	_, err = s.guard(ctx, id, func() (json.RawMessage, error) {
		var err error
		switch req.Mode {
		case "liquidate":
			op, err = s.portfolioEngine.LiquidateAll(ctx, id, body)
		case "withdraw":
			op, err = s.portfolioEngine.Withdraw(ctx, id, body)
		default:
			err = errf(400, "BAD_WITHDRAWAL", `mode must be "liquidate" or "withdraw"`)
		}
		return nil, err
	})
	return op, err
}

/* ── Archive ────────────────────────────────────────────────────────── */

// ArchiveMessage is what the owner's wallet signs (EIP-191) to archive a
// portfolio. The frontend builds the same text; keep the two in step.
func ArchiveMessage(id string, issuedAt time.Time) string {
	return fmt.Sprintf("Archive Brighter portfolio %s\n\nThis stops its automation and hides it from your portfolios. It moves no funds.\n\nIssued At: %s",
		id, issuedAt.UTC().Format(time.RFC3339))
}

type ArchiveRequest struct {
	IssuedAt  string `json:"issuedAt"`
	Signature string `json:"signature"`
}

// archiveWindow bounds how old a signed archive request may be.
const archiveWindow = 10 * time.Minute

// Archive retires an empty portfolio at its owner's request. The portfolio engine has no
// archive: its automation is stopped there (history and smart account are
// kept, and /start would resume it) and the portfolio leaves Brighter's lists.
func (s *Service) Archive(ctx context.Context, id string, req ArchiveRequest) error {
	p, err := s.knownPortfolio(ctx, id)
	if err != nil {
		return err
	}
	issued, err := time.Parse(time.RFC3339, req.IssuedAt)
	if err != nil || s.now().Sub(issued).Abs() > archiveWindow {
		return errf(400, "ARCHIVE_EXPIRED", "This archive request has expired. Try again.")
	}
	if err := auth.Verify(ArchiveMessage(id, issued), req.Signature, p.Owner); err != nil {
		return errf(401, "BAD_SIGNATURE", "%s", err.Error())
	}

	// Money must never be hidden with a portfolio: only an empty one goes.
	raw, err := s.guard(ctx, id, func() (json.RawMessage, error) { return s.portfolioEngine.Positions(ctx, id) })
	if err != nil {
		return err
	}
	var pos struct {
		TotalValueUsd string `json:"totalValueUsd"`
		InTransit     *struct {
			TotalUsd string `json:"totalUsd"`
		} `json:"inTransit"`
	}
	if err := json.Unmarshal(raw, &pos); err != nil {
		return fmt.Errorf("positions for %s: %w", id, err)
	}
	inTransit := 0.0
	if pos.InTransit != nil {
		inTransit = parseAmount(pos.InTransit.TotalUsd)
	}
	if parseAmount(pos.TotalValueUsd) >= 0.01 || inTransit >= 0.01 {
		return errf(409, "PORTFOLIO_NOT_EMPTY", "Withdraw everything before archiving this portfolio.")
	}

	_, err = s.guard(ctx, id, func() (json.RawMessage, error) { return nil, s.portfolioEngine.Stop(ctx, id) })
	var ge *portfolioengine.Error
	// 400 is "no rebalance schedule": nothing to stop.
	if err != nil && !(errors.As(err, &ge) && ge.Status == 400) {
		return err
	}
	if err := s.store.MarkPortfolioDropped(ctx, id); err != nil {
		return err
	}
	v := s.portfolioView(ctx, p)
	s.announce(fmt.Sprintf("Portfolio archived: %s (%s), portfolio %s of %s.", v.TemplateName, v.ParamsLabel, id, shortAddr(p.Owner)),
		fmt.Sprintf("Portfolio archived: %s (%s).", v.TemplateName, v.ParamsLabel))
	return nil
}

func (s *Service) knownPortfolio(ctx context.Context, id string) (*store.Portfolio, error) {
	p, err := s.store.GetPortfolio(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errf(404, "PORTFOLIO_NOT_FOUND", "Unknown portfolio %s.", id)
	}
	return p, err
}

// guard runs a portfolio engine call and retires our record of a portfolio the portfolio engine says
// no longer exists, so users do not keep hitting a dead row.
func (s *Service) guard(ctx context.Context, id string, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	if !s.portfolioEngine.Configured() {
		return nil, portfolioengine.ErrNotConfigured
	}
	raw, err := fn()
	var ge *portfolioengine.Error
	if errors.As(err, &ge) && ge.NotFound() && strings.Contains(ge.Path, id) {
		if derr := s.store.MarkPortfolioDropped(ctx, id); derr == nil {
			s.log.Info("portfolio marked dropped", "portfolio", id, "cause", ge.Message)
		}
	}
	return raw, err
}
