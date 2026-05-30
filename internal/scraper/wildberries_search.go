package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// v18 — актуальная версия search API (подтверждено живым ответом, май 2026).
	wbSearchAPIBase = "https://search.wb.ru/exactmatch/ru/common/v18/search"

	wbSearchPageSize   = 100     // товаров на страницу выдачи
	maxSearchBodyBytes = 8 << 20 // защита от неожиданно гигантского ответа
	maxBackoffDelay    = 8 * time.Second
)

// WildberriesSearchScraper — скрейпер поисковой выдачи WB.
//
// Встраивает товарный *WildberriesScraper, поэтому удовлетворяет и
// MarketplaceScraper (карточка товара), и SearchScraper (выдача). Достаточно
// зарегистрировать ОДИН этот скрейпер — он обслуживает оба типа WB-ссылок.
type WildberriesSearchScraper struct {
	*WildberriesScraper // товарный скрейпер (Marketplace/Matches/Scrape)

	pool      *ProxyPool
	maxPages  int
	pageDelay time.Duration
}

// NewWildberriesSearchScraper.
//
//	base      — товарный скрейпер (если nil — создаётся дефолтный);
//	pool      — пул прокси (если nil — прямой клиент без прокси);
//	maxPages  — сколько страниц максимум читать (<=0 → 5; каждая ≈100 товаров);
//	pageDelay — пауза между страницами (вежливость + снижение риска 429).
func NewWildberriesSearchScraper(base *WildberriesScraper, pool *ProxyPool, maxPages int, pageDelay time.Duration) *WildberriesSearchScraper {
	if base == nil {
		base = NewWildberriesScraper(5)
	}
	if pool == nil {
		pool, _ = NewProxyPool(nil, 12*time.Second)
	}
	if maxPages <= 0 {
		maxPages = 5
	}
	if pageDelay < 0 {
		pageDelay = 0
	}
	return &WildberriesSearchScraper{
		WildberriesScraper: base,
		pool:               pool,
		maxPages:           maxPages,
		pageDelay:          pageDelay,
	}
}

// Гарантия на этапе компиляции: тип реализует SearchScraper.
var _ SearchScraper = (*WildberriesSearchScraper)(nil)

// MatchesSearch — WB-ссылка с текстовым поиском (?search=...).
//
// MVP: только текстовый поиск. Категорийные/брендовые/продавцовые ссылки без
// параметра search пока не поддерживаются (фильтры — отдельный шаг).
func (s *WildberriesSearchScraper) MatchesSearch(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if !strings.Contains(strings.ToLower(u.Host), "wildberries.ru") {
		return false
	}
	return strings.TrimSpace(u.Query().Get("search")) != ""
}

// NormalizeSearchURL — канонический ключ дедупликации.
//
// Сохраняем только семантику поиска (текст запроса + сортировка), отбрасывая
// page/dest/spp/appType и прочий сессионный мусор. Текст приводим к нижнему
// регистру и схлопываем пробелы — WB ищет регистронезависимо, так разные
// пользователи с одинаковым по смыслу запросом дают один normalized_url.
func (s *WildberriesSearchScraper) NormalizeSearchURL(rawURL string) (string, error) {
	query, sortMode, err := s.parseSearchParams(rawURL)
	if err != nil {
		return "", err
	}
	canon := url.Values{}
	canon.Set("query", strings.ToLower(query))
	canon.Set("sort", sortMode)
	// canon.Encode() сортирует ключи → стабильный порядок (query, sort).
	return "https://www.wildberries.ru/catalog/0/search.aspx?" + canon.Encode(), nil
}

// ScrapeSearch — постранично собрать выдачу.
func (s *WildberriesSearchScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	query, sortMode, err := s.parseSearchParams(rawURL)
	if err != nil {
		return nil, err
	}

	out := &SearchResultSet{}
	position := 0

	for page := 1; page <= s.maxPages; page++ {
		if page > 1 {
			s.sleep(ctx, s.pageDelay)
		}

		body, err := s.fetchPage(ctx, buildSearchAPIURL(query, sortMode, page))
		if err != nil {
			if out.PagesRead > 0 {
				// Уже что-то набрали — отдаём частичный результат, не роняя всё.
				break
			}
			return nil, err
		}

		var parsed wbSearchResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			if out.PagesRead > 0 {
				break
			}
			return nil, fmt.Errorf("decode search page %d: %w", page, err)
		}

		out.PagesRead = page
		if parsed.Total > 0 {
			out.TotalFound = parsed.Total
		}

		if len(parsed.Products) == 0 {
			break // выдача закончилась
		}

		for _, p := range parsed.Products {
			position++
			item := wbProductToItem(p, position)
			if item.PriceKopecks == 0 {
				// нет цены / нет в наличии — в выдачу не кладём
				continue
			}
			out.Items = append(out.Items, item)
		}

		if len(parsed.Products) < wbSearchPageSize {
			break // последняя страница (неполная)
		}
	}

	return out, nil
}

// ── HTTP с ротацией прокси и backoff ─────────────────────────────────────────

func (s *WildberriesSearchScraper) fetchPage(ctx context.Context, apiURL string) ([]byte, error) {
	// Достаточно попыток, чтобы перебрать все прокси плюс пара повторов.
	maxAttempts := s.pool.Size() + 2
	if maxAttempts < 3 {
		maxAttempts = 3
	}

	delay := 500 * time.Millisecond
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		pc := s.pool.next() // round-robin: следующая попытка — другой прокси
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return nil, err
		}
		s.setHeaders(req)

		resp, err := pc.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request via %s: %w", pc.label, err)
			s.sleep(ctx, delay)
			delay = bumpDelay(delay)
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSearchBodyBytes))
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK && readErr == nil:
			return body, nil

		case resp.StatusCode == http.StatusTooManyRequests:
			// 429 — IP/частотный лимит. Следующая попытка уйдёт с другого
			// прокси (pool.next), плюс выжидаем нарастающую паузу.
			lastErr = fmt.Errorf("%w: 429 via %s", ErrMarketplaceBlocked, pc.label)
			s.sleep(ctx, delay)
			delay = bumpDelay(delay)

		default:
			if readErr != nil {
				lastErr = fmt.Errorf("read body (status %d) via %s: %w", resp.StatusCode, pc.label, readErr)
			} else {
				lastErr = fmt.Errorf("status %d via %s", resp.StatusCode, pc.label)
			}
			s.sleep(ctx, delay)
			delay = bumpDelay(delay)
		}
	}

	if lastErr == nil {
		lastErr = ErrMarketplaceBlocked
	}
	return nil, lastErr
}

func (s *WildberriesSearchScraper) setHeaders(req *http.Request) {
	// Заголовки, при которых запрос с сервера реально проходил (см. разведку):
	// именно Accept-Language + браузерный UA + Referer/Origin отличали 200 от
	// мгновенного 429. Accept-Encoding не ставим вручную — стандартный
	// транспорт Go сам добавит gzip и прозрачно распакует ответ.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
	req.Header.Set("Origin", "https://www.wildberries.ru")
	req.Header.Set("Referer", "https://www.wildberries.ru/")
}

// sleep — пауза, прерываемая отменой контекста.
func (s *WildberriesSearchScraper) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func bumpDelay(d time.Duration) time.Duration {
	d *= 2
	if d > maxBackoffDelay {
		return maxBackoffDelay
	}
	return d
}

// ── Парсинг ──────────────────────────────────────────────────────────────────

// parseSearchParams — вытащить из браузерного URL текст запроса и сортировку.
func (s *WildberriesSearchScraper) parseSearchParams(rawURL string) (query, sortMode string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	q := u.Query()
	query = strings.TrimSpace(q.Get("search"))
	if query == "" {
		return "", "", fmt.Errorf("%w: no search query in URL", ErrInvalidURL)
	}
	query = strings.Join(strings.Fields(query), " ") // схлопнуть кратные пробелы
	sortMode = strings.TrimSpace(strings.ToLower(q.Get("sort")))
	if sortMode == "" {
		sortMode = "popular"
	}
	return query, sortMode, nil
}

// buildSearchAPIURL — собрать URL запроса к search.wb.ru v18.
func buildSearchAPIURL(query, sortMode string, page int) string {
	q := url.Values{}
	q.Set("appType", "1")
	q.Set("curr", "rub")
	q.Set("dest", "-1257786")
	q.Set("lang", "ru")
	q.Set("page", strconv.Itoa(page))
	q.Set("query", query)
	q.Set("resultset", "catalog")
	q.Set("sort", sortMode)
	q.Set("spp", "30")
	return wbSearchAPIBase + "?" + q.Encode()
}

// wbProductToItem — товар WB → SearchItem. Цена: product (финальная), basic
// (старая). Подстраховка total на случай дрейфа API в будущем.
func wbProductToItem(p wbSearchProduct, position int) SearchItem {
	var price, oldPrice int64
	if len(p.Sizes) > 0 {
		pr := p.Sizes[0].Price
		price = pr.Product
		if price == 0 {
			price = pr.Total
		}
		oldPrice = pr.Basic
	}

	art := strconv.FormatInt(p.ID, 10)
	return SearchItem{
		ArticleID:         art,
		Name:              p.Name,
		Brand:             p.Brand,
		URL:               "https://www.wildberries.ru/catalog/" + art + "/detail.aspx",
		ImageURL:          wbImageURL(p.ID),
		Position:          position,
		PriceKopecks:      price,
		OldPriceKopecks:   oldPrice,
		FeedbackPointsRaw: p.FeedbackPoints,
	}
}

// wbImageURL — URL картинки товара через basket-CDN (host-bucket по vol).
// Использует общий с товарным скрейпером wbBasketNumber (wildberries.go).
func wbImageURL(id int64) string {
	vol := id / 100000
	part := id / 1000
	basket := wbBasketNumber(id)
	return fmt.Sprintf("https://basket-%02d.wbbasket.ru/vol%d/part%d/%d/images/big/1.webp",
		basket, vol, part, id)
}

// ── Структуры ответа search.wb.ru v18 ────────────────────────────────────────
// В v18 products лежат в КОРНЕ ответа (подтверждено живым ответом), в отличие
// от card.wb.ru, где они под data.products.

type wbSearchResponse struct {
	Total    int               `json:"total"`
	Products []wbSearchProduct `json:"products"`
}

type wbSearchProduct struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Brand          string `json:"brand"`
	FeedbackPoints int64  `json:"feedbackPoints"`
	Sizes          []struct {
		Price struct {
			Basic   int64 `json:"basic"`
			Product int64 `json:"product"`
			Total   int64 `json:"total"`
		} `json:"price"`
	} `json:"sizes"`
}
