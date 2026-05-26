package scraper

import (
	"context"
	"strings"
)

// OzonScraper — заглушка.

type OzonScraper struct{}

func NewOzonScraper() *OzonScraper {
	return &OzonScraper{}
}

func (s *OzonScraper) Marketplace() Marketplace {
	return MarketplaceOzon
}

func (s *OzonScraper) Matches(url string) bool {
	return strings.Contains(url, "ozon.ru/product/")
}

func (s *OzonScraper) Scrape(_ context.Context, _ string) (*Result, error) {
	return nil, ErrNotImplemented
}
