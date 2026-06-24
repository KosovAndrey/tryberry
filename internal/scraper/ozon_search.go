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
	"strconv"
	"strings"
)

// OzonSearchScraper — скрейпер поисковой выдачи Ozon (ozon.ru/search/?text=...).
// Реализует SearchScraper.
//
// ТРАНСПОРТ: только browser-режим через сайдкар ozon-miner. Прямой composer-API
// закрыт антиботом FAB (см. docs/OZON-STATUS.md); живая выдача достаётся in-page
// fetch'ем из прогретой дорожки сайдкара (GET /search?text=...), который ходит к
// тому же универсальному entrypoint-api, что и карточка, и отдаёт сырой
// widgetStates. Позиции выдачи лежат в виджете tileGridDesktop. Если базовый
// OzonScraper НЕ в browser-режиме (нет OZON_BROWSER_URL) — ScrapeSearch вернёт
// ErrMarketplaceBlocked.
//
// Структура тайла подтверждена прод-дампом (см. parseSearch/buildOzonSearchItem).
// Диаг-лог `ozon search: no items parsed` (widgets+sample) остаётся на случай
// дрейфа разметки. Цены — в КОПЕЙКАХ (контракт SearchItem).
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

// ScrapeSearch — забрать выдачу через сайдкар ozon-miner (browser-пул), с
// пагинацией по nextPage.
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
	// Inner-path выдачи; text не кодируем (encodeURIComponent в сайдкаре).
	return s.scrapePaginated(ctx, "/search/?text="+text, "search", text)
}

// maxOzonPages — потолок страниц пагинации (по ~8–36 тайлов), чтобы не уходить в
// бесконечную прокрутку. Дороже всех (одна браузер-дорожка), поэтому держим низко;
// item-кап SEARCH_MAX_ITEMS_OZON упирается обычно раньше.
const maxOzonPages = 9

// scrapePaginated — общий цикл для выдачи и витрины: идём по nextPage из
// widgetStates, складываем тайлы (дедуп по ArticleID) до maxItems/конца/потолка
// страниц. Партиальный результат при сбое на поздних страницах — ок.
func (s *OzonSearchScraper) scrapePaginated(ctx context.Context, initialPath, kind, what string) (*SearchResultSet, error) {
	out := &SearchResultSet{}
	seen := make(map[string]bool)
	var lastBody []byte
	path := initialPath
	for page := 0; page < maxOzonPages && path != ""; page++ {
		if err := s.limiter.Wait(ctx); err != nil {
			if len(out.Items) > 0 {
				break
			}
			return nil, err
		}
		status, body, err := s.fetchPageViaBrowser(ctx, path)
		if err != nil {
			if len(out.Items) > 0 {
				break // частичный результат сохраняем
			}
			return nil, err
		}
		lastBody = body
		if status == 403 || bytesHasFAB(body) {
			if len(out.Items) > 0 {
				break
			}
			s.log.Warn("ozon "+kind+": FAB block", "status", status, "what", what, "body", snippet(body, 300))
			return nil, ErrMarketplaceBlocked
		}
		if status != 200 {
			if len(out.Items) > 0 {
				break
			}
			return nil, fmt.Errorf("ozon %s status %d", kind, status)
		}
		res := s.parseSearch(body)
		for _, it := range res.Items {
			if it.ArticleID == "" || seen[it.ArticleID] {
				continue
			}
			seen[it.ArticleID] = true
			it.Position = len(out.Items) + 1
			out.Items = append(out.Items, it)
			if len(out.Items) >= s.maxItems {
				out.PagesRead = page + 1
				out.TotalFound = len(out.Items)
				s.log.Info("ozon "+kind+" scraped", "what", what, "items", len(out.Items), "pages", out.PagesRead)
				return out, nil
			}
		}
		out.PagesRead = page + 1
		path = ozonNextPage(body)
	}
	out.TotalFound = len(out.Items)
	if len(out.Items) == 0 {
		s.log.Warn("ozon "+kind+": no items parsed",
			"what", what, "len", len(lastBody),
			"widgets", ozonWidgetNames(lastBody), "sample", ozonSearchSample(lastBody))
		return out, ErrParseFailed
	}
	s.log.Info("ozon "+kind+" scraped", "what", what, "items", len(out.Items), "pages", out.PagesRead)
	return out, nil
}

// ozonNextPage — inner-path следующей страницы из ответа (пусто на последней).
// У Ozon курсор бесконечного скролла лежит НЕ в корне envelope, а внутри стейта
// виджета infiniteVirtualPaginator (stringified-JSON в widgetStates) — поле
// nextPage, напр. "/seller/<seg>/?layout_page_index=2&page=2&paginator_token=…".
// Корневой env.NextPage оставляем как первичный источник (на случай иных раскладок).
func ozonNextPage(body []byte) string {
	var env ozonEnvelope
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	if np := strings.TrimSpace(env.NextPage); np != "" {
		return np
	}
	for k, v := range env.WidgetStates {
		if !strings.HasPrefix(k, "infiniteVirtualPaginator") {
			continue
		}
		var pag struct {
			NextPage string `json:"nextPage"`
		}
		if json.Unmarshal([]byte(v), &pag) == nil {
			if np := strings.TrimSpace(pag.NextPage); np != "" {
				return np
			}
		}
	}
	return ""
}

// fetchPageViaBrowser — in-page fetch произвольного inner-path через сайдкар
// (GET /page?path=…). Выдача/витрина тяжёлые → лимит 8 МБ.
func (s *OzonSearchScraper) fetchPageViaBrowser(ctx context.Context, path string) (int, []byte, error) {
	api := s.browserURL + "/page?path=" + url.QueryEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := s.browserClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("ozon page browser sidecar: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout {
		return 0, nil, fmt.Errorf("ozon page sidecar unavailable: status %d: %s",
			resp.StatusCode, snippet(body, 200))
	}
	return resp.StatusCode, body, nil
}

// ── Парсинг выдачи (tileGridDesktop) ───────────────────────────────────────────

// ozonSearchLinkRe — ссылка на карточку из тайла выдачи: /product/<slug>-<id>/ .
// Числовой хвост — SKU (стабильный ключ дедупликации, products апсертится по URL).
var ozonSearchLinkRe = regexp.MustCompile(`/product/(?:[^"/?#]*-)?(\d+)/?`)

// ozonDigitsRe — строка целиком из цифр (валидация строкового SKU).
var ozonDigitsRe = regexp.MustCompile(`^\d+$`)

// parseSearch — позиции выдачи из widgetStates. Тайлы лежат в tileGridDesktop;
// обход ранжируем (searchResults/tileGrid сперва) на случай дрейфа имён виджетов.
//
// Каждый тайл — объект с id/sku (SKU), action.link (карточка), mainState (атомы
// priceV2 и textDS-название) и tileImage. Цену/картинку достаём общими хелперами
// карточки (findPriceTexts/findOzonImageURL). Берём только тайлы с SKU и ценой.
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

// buildOzonSearchItem собирает SearchItem из объекта-тайла tileGridDesktop. false —
// если нет SKU или цены (тайл-баннер/реклама/«нет в наличии»).
//
// Структура тайла (снято с прода, text=iphone): id/sku — SKU; action.link —
// ссылка на карточку; mainState[] — атомы: priceV2.price[] (text+textStyle
// PRICE/ORIGINAL_PRICE) и textDS с id=="name" (название); tileImage — галерея.
func buildOzonSearchItem(m map[string]any) (SearchItem, bool) {
	sku := ozonTileSKU(m)
	link := findOzonProductLink(m)
	if sku == "" && link != "" {
		if mm := ozonSearchLinkRe.FindStringSubmatch(link); len(mm) >= 2 {
			sku = mm[1]
		}
	}
	if sku == "" {
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

	itemURL := ozonAbsoluteURL(link)
	if itemURL == "" || itemURL == "https://www.ozon.ru" {
		itemURL = "https://www.ozon.ru/product/" + sku + "/"
	}
	item := SearchItem{
		ArticleID:    sku,
		Name:         strings.TrimSpace(findOzonTileName(m)),
		URL:          itemURL,
		PriceKopecks: int64(price * 100),
		ImageURL:     findOzonImageURL(m),
	}
	if old := parseRubles(byStyle["ORIGINAL_PRICE"]); old > price {
		item.OldPriceKopecks = int64(old * 100)
	}
	return item, true
}

// ozonTileSKU — SKU тайла: поле id (строка) или sku (число). "" — если нет.
func ozonTileSKU(m map[string]any) string {
	for _, k := range []string{"id", "sku"} {
		switch v := m[k].(type) {
		case string:
			if ozonDigitsRe.MatchString(v) {
				return v
			}
		case json.Number:
			return v.String()
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

// findOzonTileName — название тайла из mainState: атом помечен id=="name" с текстом
// в textDS.text. Fallback — первый textDS-атом mainState (имя — единственный textDS,
// цены лежат в priceV2). Best-effort на случай дрейфа разметки.
func findOzonTileName(m map[string]any) string {
	ms, ok := m["mainState"].([]any)
	if !ok {
		return findFirstString(m, "title")
	}
	var fallback string
	for _, a := range ms {
		am, ok := a.(map[string]any)
		if !ok {
			continue
		}
		text := ozonTextDSText(am)
		if text == "" {
			continue
		}
		if am["id"] == "name" {
			return text
		}
		if fallback == "" {
			fallback = text
		}
	}
	return fallback
}

// ozonTextDSText — текст из атома textDS ({"textDS":{"text":"..."}}). "" если нет.
func ozonTextDSText(atom map[string]any) string {
	if td, ok := atom["textDS"].(map[string]any); ok {
		if s, ok := td["text"].(string); ok {
			return s
		}
	}
	return ""
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
