package scraper

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// OzonSellerScraper — витрина продавца Ozon (ozon.ru/seller/<slug>-<id>/) как
// поиск-подписка. Транспорт — тот же сайдкар ozon-miner (browser-пул): витрина
// рендерится тем же entrypoint-api, что выдача и карточка, и отдаёт тот же
// widgetStates с tileGrid. Поэтому переиспользуем парсер выдачи (встраиваем
// *OzonSearchScraper ради parseSearch и базового OzonScraper-транспорта).
//
// Без browser-режима (нет OZON_BROWSER_URL) ScrapeSearch вернёт blocked.
type OzonSellerScraper struct {
	*OzonSearchScraper
}

var _ SearchScraper = (*OzonSellerScraper)(nil)

// NewOzonSellerScraper оборачивает карточный OzonScraper (через OzonSearchScraper —
// нужен его parseSearch). maxItems<=0 → 60.
func NewOzonSellerScraper(base *OzonScraper, maxItems int) *OzonSellerScraper {
	return &OzonSellerScraper{OzonSearchScraper: NewOzonSearchScraper(base, maxItems)}
}

// ozonSellerSegRe — сегмент витрины из пути /seller/<slug-id>/ (slug с числовым
// id на хвосте). Это и ключ дедупликации, и path для сайдкара.
var ozonSellerSegRe = regexp.MustCompile(`/seller/([^/?#]+)`)

// MatchesSearch — ссылка на витрину продавца Ozon (/seller/<...>). Карточка
// (/product/...) сюда не попадает.
func (s *OzonSellerScraper) MatchesSearch(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if !strings.Contains(strings.ToLower(u.Host), "ozon.ru") {
		return false
	}
	return ozonSellerSegRe.MatchString(u.Path)
}

// NormalizeSearchURL — канонический ключ по сегменту витрины: slug-id уникален.
func (s *OzonSellerScraper) NormalizeSearchURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	m := ozonSellerSegRe.FindStringSubmatch(u.Path)
	if len(m) != 2 || m[1] == "" {
		return "", fmt.Errorf("%w: not an ozon seller URL", ErrInvalidURL)
	}
	return "https://www.ozon.ru/seller/" + m[1] + "/", nil
}

// SellerName — имя витрины из слага (nike-store-12345 → "Nike Store"): хвостовой
// числовой id отбрасываем, дефисы → пробелы, Title-case. Сети не требует.
// Настоящее имя из widgetStates — возможная доводка позже.
func (s *OzonSellerScraper) SellerName(_ context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	m := ozonSellerSegRe.FindStringSubmatch(u.Path)
	if len(m) != 2 {
		return "", nil
	}
	return prettifyOzonSellerSeg(m[1]), nil
}

// prettifyOzonSellerSeg: "nike-store-12345" → "Nike Store" (отбрасываем -<id>).
func prettifyOzonSellerSeg(seg string) string {
	parts := strings.Split(seg, "-")
	// Отбросить хвостовой числовой id.
	if len(parts) > 1 && ozonDigitsRe.MatchString(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// ScrapeSearch — забрать витрину через сайдкар ozon-miner (GET /seller?path=<seg>).
func (s *OzonSellerScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	if s.OzonScraper == nil || s.mode != ozonModeBrowser || !s.configured {
		return nil, fmt.Errorf("%w: ozon seller требует browser-сайдкар (mode=browser + OZON_BROWSER_URL)", ErrMarketplaceBlocked)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	m := ozonSellerSegRe.FindStringSubmatch(u.Path)
	if len(m) != 2 || m[1] == "" {
		return nil, fmt.Errorf("%w: no seller segment", ErrInvalidURL)
	}
	// Витрина = тот же widgetStates с tileGrid; пагинация по nextPage общим циклом.
	return s.scrapePaginated(ctx, "/seller/"+m[1]+"/", "seller", m[1])
}
