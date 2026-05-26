package scraper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

// Registry — реестр всех зарегистрированных скрейперов.
// Автоматически выбирает нужный по URL товара.
type Registry struct {
	scrapers []MarketplaceScraper
}

func NewRegistry(scrapers ...MarketplaceScraper) *Registry {
	return &Registry{scrapers: scrapers}
}

// FindByURL возвращает скрейпер для данного URL или ошибку если ни один не подошёл
func (r *Registry) FindByURL(url string) (MarketplaceScraper, error) {
	for _, s := range r.scrapers {
		if s.Matches(url) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: no scraper matches URL %s", ErrInvalidURL, url)
}

// Scrape — удобный хелпер: найти скрейпер и сразу выполнить скрейпинг
func (r *Registry) Scrape(ctx context.Context, url string) (*Result, Marketplace, error) {
	s, err := r.FindByURL(url)
	if err != nil {
		return nil, "", err
	}

	mp := string(s.Marketplace())
	start := time.Now()

	result, err := s.Scrape(ctx, url)

	metrics.ScrapeDuration.WithLabelValues(mp).Observe(time.Since(start).Seconds())

	status := "success"
	switch {
	case err == nil:
	case errors.Is(err, ErrProductNotFound):
		status = "not_found"
	case errors.Is(err, ErrMarketplaceBlocked):
		status = "blocked"
	default:
		status = "error"
	}
	metrics.ScrapeRequests.WithLabelValues(mp, status).Inc()

	if err != nil {
		return nil, s.Marketplace(), err
	}
	return result, s.Marketplace(), nil
}

// SupportedMarketplaces — список всех зарегистрированных маркетплейсов
func (r *Registry) SupportedMarketplaces() []Marketplace {
	out := make([]Marketplace, 0, len(r.scrapers))
	for _, s := range r.scrapers {
		out = append(out, s.Marketplace())
	}
	return out
}
