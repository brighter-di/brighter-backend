package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/strategy"
)

type publicationCatalog interface {
	PublishedStrategy(context.Context, string) (*store.PublishedStrategy, error)
	PublishedStrategies(context.Context) ([]store.PublishedStrategy, error)
}

func publicationTemplate(p *store.PublishedStrategy) (*strategy.Template, error) {
	var document strategy.Publication
	if err := json.Unmarshal(p.Document, &document); err != nil {
		return nil, err
	}
	return document.Template(p.ID, p.Author, p.PublishedAt.UTC().Format(time.RFC3339)), nil
}
func (s *Service) Template(ctx context.Context, id string) (*strategy.Template, error) {
	if t, ok := strategy.ByID(id); ok {
		return t, nil
	}
	if !strings.HasPrefix(id, "studio-") || s.catalog == nil {
		return nil, errf(404, "UNKNOWN_TEMPLATE", "Unknown strategy %q.", id)
	}
	p, err := s.catalog.PublishedStrategy(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errf(404, "UNKNOWN_TEMPLATE", "Unknown strategy %q.", id)
	}
	if err != nil {
		return nil, err
	}
	return publicationTemplate(p)
}
func (s *Service) PublicTemplates(ctx context.Context) ([]TemplateView, error) {
	out := s.Templates()
	if s.catalog == nil {
		return out, nil
	}
	rows, err := s.catalog.PublishedStrategies(ctx)
	if err != nil {
		return nil, err
	}
	// The public list is oldest first; the app reverses it for Newest.
	for i := len(rows) - 1; i >= 0; i-- {
		t, err := publicationTemplate(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, s.templateView(t))
	}
	return out, nil
}

func customStrategyError(err error) error {
	var invalid *strategy.CustomError
	if errors.As(err, &invalid) {
		return errf(422, "INVALID_STRATEGY", "%s", invalid.Reason)
	}
	return err
}
