package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// OzonSearchScraper — скрейпер поисковой выдачи Ozon (ozon.ru/search/?text=...).
// Реализует SearchScraper.
//
// ТРАНСПОРТ: только browser-режим через сайдкар ozon-miner. Прямой composer-API
// закрыт антиботом FAB (см. docs/OZON-STATUS.md); живая выдача достаётся in-page
// fetch'ем из прогретой дорожки сайдкара (GET /search?text=...), который ходит к
// тому же универсальному entrypoint-api, что и карточка, и отдаёт сырой
// widgetStates с виджетом searchResultsV2. Если базовый OzonScraper НЕ в
// browser-режиме (нет OZON_BROWSER_URL) — ScrapeSearch вернёт ErrMarketplaceBlocked.
//
// ПАРСЕР best-effort: точная JSON-структура searchResultsV2 на живой странице не
// зафиксирована (локально FAB + прогрев недоступны). Поэтому извлечение полей
// item'а — эвристическое + насыщенный диаг-лог `ozon search: no items parsed`;
// доводим по прод-логам / прямому дампу `curl ozon-miner:8080/search?text=...`
// (как доводили Я.Маркет). Цены — в КОПЕЙКАХ (контракт SearchItem).
type OzonSearchScraper struct {
	*OzonScraper
	maxItems int
}

var _ SearchScraper = (*OzonSearchScraper)(nil)

// NewOzonSearchScraper оборачивает карточный OzonScraper (переиспользуем его
// browser-транспорт/limiter). maxItems<=0 → 60.
func NewOzonSearchScraper(base *OzonScraper, maxItems int) *OzonSearchScraper {
	if maxItems <= 0 {
		maxItems = 60
	}
	return &OzonSearchScraper{OzonScraper: base, maxItems: maxItems}
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

// NormalizeSearchURL — канонический ключ дедупликации: text (+ опц. sorting).
// Без text → ErrInvalidURL.
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

// ScrapeSearch — забрать выдачу через сайдкар ozon-miner (browser-пул).
func (s *OzonSearchScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	if s.OzonScraper == nil || s.mode != ozonModeBrowser || !s.configured {
		return nil, fmt.Errorf("%w: ozon search требует browser-сайдкар (mode=browser + OZON_BROWSER_URL)", ErrMarketplaceBlocked)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	text := strings.TrimSpace(u.Query().Get("text"))
	if text == "" {
		return nil, fmt.Errorf("%w: no text in ozon search URL", ErrInvalidURL)
	}
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	status, body, err := s.fetchSearchViaBrowser(ctx, text)
	if err != nil {
		return nil, err
	}
	if status == 403 || bytesHasFAB(body) {
		s.log.Warn("ozon search: FAB block", "status", status, "text", text, "body", snippet(body, 300))
		return nil, ErrMarketplaceBlocked
	}
	if status != 200 {
		return nil, fmt.Errorf("ozon search status %d", status)
	}

	out := s.parseSearch(body)
	if len(out.Items) == 0 {
		// Диагностика для доводки парсера по прод-логам (как у Я.Маркета).
		s.log.Warn("ozon search: no items parsed",
			"text", text, "len", len(body),
			"widgets", ozonWidgetNames(body),
			"sample", ozonSearchSample(body))
		return out, ErrParseFailed
	}
	s.log.Info("ozon search scraped", "text", text, "items", len(out.Items))
	return out, nil
}

// fetchSearchViaBrowser — просим сайдкар сделать поисковый in-page fetch из живой
// дорожки. Зеркало fetchViaBrowser (карточка), но выдача тяжелее → лимит 8 МБ.
func (s *OzonSearchScraper) fetchSearchViaBrowser(ctx context.Context, text string) (int, []byte, error) {
	api := s.browserURL + "/search?text=" + url.QueryEscape(text)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := s.browserClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("ozon search browser sidecar: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout {
		return 0, nil, fmt.Errorf("ozon search sidecar unavailable: status %d: %s",
			resp.StatusCode, snippet(body, 200))
	}
	return resp.StatusCode, body, nil
}

// ── Парсинг searchResultsV2 ────────────────────────────────────────────────────

// ozonSearchLinkRe — ссылка на карточку из тайла выдачи: /product/<slug>-<id>/ .
// Числовой хвост — SKU (стабильный ключ дедупликации, products апсертится по URL).
var ozonSearchLinkRe = regexp.MustCompile(`/product/(?:[^"/?#]*-)?(\d+)/?`)

// parseSearch — позиции выдачи из widgetStates (виджеты searchResultsV2 / tileGrid).
//
// Каждый тайл — объект с product-ссылкой (action/link → /product/...-<id>/), ценой
// (атомы price[] со знаком ₽, как в карточке) и заголовком. Структура атомов между
// версиями дрейфует, поэтому поля достаём эвристически (reused findPriceTexts/
// findOzonImageURL/findFirstString из ozon.go). Берём только тайлы с id и ценой.
func (s *OzonSearchScraper) parseSearch(body []byte) *SearchResultSet {
	out := &SearchResultSet{}
	var env ozonEnvelope
	if json.Unmarshal(body, &env) != nil || len(env.WidgetStates) == 0 {
		return out
	}

	seen := make(map[string]bool)
	// Детерминированный обход: сперва явные searchResults-виджеты, потом прочие
	// (имена дрейфуют — fallback на любой виджет с тайлами).
	keys := make([]string, 0, len(env.WidgetStates))
	for k := range env.WidgetStates {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return ozonSearchWidgetRank(keys[i]) < ozonSearchWidgetRank(keys[j]) ||
			(ozonSearchWidgetRank(keys[i]) == ozonSearchWidgetRank(keys[j]) && keys[i] < keys[j])
	})

	for _, k := range keys {
		if len(out.Items) >= s.maxItems {
			break
		}
		if ozonSearchWidgetRank(k) == 2 {
			continue // не похоже на контейнер выдачи — пропускаем
		}
		var data any
		if json.Unmarshal([]byte(env.WidgetStates[k]), &data) != nil {
			continue
		}
		collectOzonSearchItems(data, seen, &out.Items, s.maxItems)
	}
	out.TotalFound = len(out.Items)
	return out
}

// ozonSearchWidgetRank: 0 — явный searchResultsV2, 1 — tile/grid, 2 — прочее.
func ozonSearchWidgetRank(name string) int {
	ln := strings.ToLower(name)
	switch {
	case strings.Contains(ln, "searchresult"):
		return 0
	case strings.Contains(ln, "tilegrid") || strings.Contains(ln, "tile") || strings.Contains(ln, "grid"):
		return 1
	default:
		return 2
	}
}

// collectOzonSearchItems рекурсивно ищет массивы "items" и собирает из их
// элементов покупаемые тайлы (есть product-ссылка + цена).
func collectOzonSearchItems(v any, seen map[string]bool, out *[]SearchItem, max int) {
	switch t := v.(type) {
	case map[string]any:
		if items, ok := t["items"].([]any); ok {
			for _, it := range items {
				if len(*out) >= max {
					return
				}
				m, ok := it.(map[string]any)
				if !ok {
					continue
				}
				if si, ok := buildOzonSearchItem(m); ok && !seen[si.ArticleID] {
					seen[si.ArticleID] = true
					si.Position = len(*out) + 1
					*out = append(*out, si)
				}
			}
		}
		for _, val := range t {
			collectOzonSearchItems(val, seen, out, max)
		}
	case []any:
		for _, e := range t {
			collectOzonSearchItems(e, seen, out, max)
		}
	}
}

// buildOzonSearchItem собирает SearchItem из объекта-тайла. false — если нет
// product-ссылки или цены (тайл-баннер/реклама/«нет в наличии»).
func buildOzonSearchItem(m map[string]any) (SearchItem, bool) {
	link := findOzonProductLink(m)
	if link == "" {
		return SearchItem{}, false
	}
	id := ozonSearchLinkRe.FindStringSubmatch(link)
	if len(id) < 2 {
		return SearchItem{}, false
	}

	byStyle := map[string]string{}
	findPriceTexts(m, byStyle)
	var price float64
	for _, style := range []string{"PRICE", "CARD_PRICE", "ORIGINAL_PRICE"} {
		if v := parseRubles(byStyle[style]); v > 0 {
			price = v
			break
		}
	}
	if price <= 0 {
		return SearchItem{}, false
	}

	item := SearchItem{
		ArticleID:    id[1],
		Name:         strings.TrimSpace(findOzonTileTitle(m)),
		URL:          ozonAbsoluteURL(link),
		PriceKopecks: int64(price * 100),
		ImageURL:     findOzonImageURL(m),
	}
	if old := parseRubles(byStyle["ORIGINAL_PRICE"]); old > price {
		item.OldPriceKopecks = int64(old * 100)
	}
	return item, true
}

// findOzonProductLink рекурсивно возвращает первую строку-ссылку на карточку.
func findOzonProductLink(v any) string {
	switch t := v.(type) {
	case string:
		if ozonSearchLinkRe.MatchString(t) {
			return t
		}
	case map[string]any:
		// явные ключи действий первыми (детерминированно)
		for _, k := range []string{"link", "deepLink", "deeplink", "url", "path"} {
			if s, ok := t[k].(string); ok && ozonSearchLinkRe.MatchString(s) {
				return s
			}
		}
		for _, val := range t {
			if u := findOzonProductLink(val); u != "" {
				return u
			}
		}
	case []any:
		for _, e := range t {
			if u := findOzonProductLink(e); u != "" {
				return u
			}
		}
	}
	return ""
}

// findOzonTileTitle — заголовок тайла. Пробуем явные ключи, затем — атом текста с
// буквами и без ₽ (чтобы не подхватить цену). Best-effort.
func findOzonTileTitle(m map[string]any) string {
	for _, key := range []string{"title", "name"} {
		if s := findFirstString(m, key); s != "" && !strings.Contains(s, "₽") {
			return s
		}
	}
	return ""
}

// ozonAbsoluteURL — абсолютная ссылка на карточку без query (стабильный ключ).
func ozonAbsoluteURL(link string) string {
	if i := strings.IndexByte(link, '?'); i >= 0 {
		link = link[:i]
	}
	if strings.HasPrefix(link, "http") {
		return link
	}
	return "https://www.ozon.ru" + link
}

// ── Диагностика ────────────────────────────────────────────────────────────────

func ozonWidgetNames(body []byte) []string {
	var env ozonEnvelope
	if json.Unmarshal(body, &env) != nil {
		return nil
	}
	names := make([]string, 0, len(env.WidgetStates))
	for k := range env.WidgetStates {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ozonSearchSample — сырой кусок первого вероятного виджета выдачи (для доводки
// парсера по логам, когда тайлы не распознались).
func ozonSearchSample(body []byte) string {
	var env ozonEnvelope
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	best, bestRank := "", 3
	for k, v := range env.WidgetStates {
		if r := ozonSearchWidgetRank(k); r < bestRank {
			best, bestRank = v, r
		}
	}
	return snippet([]byte(best), 800)
}
