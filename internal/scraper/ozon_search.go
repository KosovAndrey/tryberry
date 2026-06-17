package scraper

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// OzonSearchScraper — скрейпер поисковой выдачи Ozon (ozon.ru/search/?text=...).
// Реализует SearchScraper.
//
// СТАТУС: ScrapeSearch ПОКА НЕ РАБОТАЕТ. Две причины, обе вне парсинга:
//  1. Прямой composer-API Ozon закрыт антиботом FAB (см. docs/OZON-STATUS.md) —
//     карточный скрейпинг Ozon в проде идёт через браузер-пул сайдкара ozon-miner.
//  2. Сайдкар ozon-miner умеет ТОЛЬКО /scrape?id=<товар> (одна карточка) — у него
//     нет маршрута под поисковый URL. Поэтому даже с рабочим FAB выдачу не достать
//     без доработки сайдкара (search-endpoint → composer searchResultsV2).
//
// MatchesSearch/NormalizeSearchURL реализованы корректно (форматы URL известны),
// чтобы бот узнавал ссылку и давал понятный ответ «скоро», а не «не распознал».
type OzonSearchScraper struct {
	*OzonScraper
}

var _ SearchScraper = (*OzonSearchScraper)(nil)

func NewOzonSearchScraper(base *OzonScraper) *OzonSearchScraper {
	return &OzonSearchScraper{OzonScraper: base}
}

// MatchesSearch — ссылка на выдачу Ozon: ozon.ru с /search в пути или text=.
// Карточка (ozon.ru/product/...) сюда не попадает — её разбирает Scrape.
func (s *OzonSearchScraper) MatchesSearch(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if !strings.Contains(strings.ToLower(u.Host), "ozon.ru") {
		return false
	}
	if strings.Contains(u.Path, "/product") {
		return false
	}
	return strings.Contains(u.Path, "/search") || strings.TrimSpace(u.Query().Get("text")) != ""
}

// NormalizeSearchURL — канонический ключ дедупликации: text (+ опц. category_was_predicted/
// sorting). Без text → ErrInvalidURL.
func (s *OzonSearchScraper) NormalizeSearchURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	text := strings.TrimSpace(u.Query().Get("text"))
	if text == "" {
		return "", fmt.Errorf("%w: no text query in ozon search URL", ErrInvalidURL)
	}
	text = strings.Join(strings.Fields(strings.ToLower(text)), " ")
	canon := url.Values{}
	canon.Set("text", text)
	if sort := strings.TrimSpace(u.Query().Get("sorting")); sort != "" {
		canon.Set("sorting", sort)
	}
	return "https://www.ozon.ru/search/?" + canon.Encode(), nil
}

// ScrapeSearch — пока недоступно: FAB + сайдкар без search-маршрута (см. док-строку
// типа). Возвращаем ErrMarketplaceBlocked, чтобы бот показал «Ozon скоро будет».
func (s *OzonSearchScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	return nil, fmt.Errorf("%w: ozon search не реализован (FAB + сайдкар ozon-miner без search-endpoint)", ErrMarketplaceBlocked)
}
