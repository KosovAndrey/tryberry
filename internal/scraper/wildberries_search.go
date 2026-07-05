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

	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

const (
	// Эндпоинт, который реально отдаёт 200: same-origin реверс-прокси WB
	// (www.wildberries.ru/__internal/u-search/...). Голый search.wb.ru режется
	// wbaas-челленджем; этот же путь принимает cookie x_wbaas_token из браузера.
	wbSearchAPIBase = "https://www.wildberries.ru/__internal/u-search/exactmatch/ru/common/v18/search"

	wbSearchPageSize   = 100
	maxSearchBodyBytes = 8 << 20
	maxBackoffDelay    = 8 * time.Second

	// Дефолтный UA, если токен записан без UA. Должен совпадать с тем, под
	// которым выписан токен (UA вшит в сам токен), иначе wbaas может отклонить.
	defaultSearchUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 YaBrowser/26.3.0.0 Safari/537.36"
)

// WildberriesSearchScraper — скрейпер поисковой выдачи WB.
//
// Встраивает товарный *WildberriesScraper, поэтому удовлетворяет и
// MarketplaceScraper, и SearchScraper. Аутентифицируется cookie-токеном wbaas,
// который добывается в браузере и кладётся в Redis (см. scripts/wb-token-update.sh);
// TokenProvider читает его перед запросами.
type WildberriesSearchScraper struct {
	*WildberriesScraper

	pool      *ProxyPool
	tokens    TokenProvider
	maxPages  int
	pageDelay time.Duration

	// browserURL — база сайдкара wb-search-miner (браузер-как-транспорт). Голый
	// direct с датацентр-IP wbaas режет 403 на ПОПУЛЯРНЫХ запросах (iphone 17 →
	// 403, капибара → ок), хотя токен валиден: различие в ТРАНСПОРТЕ (браузер
	// проходит челлендж/JA3, http.Client — нет), не в IP. На 403 уводим запрос в
	// прогретый браузер сайдкара (in-page fetch к тому же u-search), как сделано у
	// Ozon (FAB). Пусто → фолбэка нет, 403 уходит наверх как ErrMarketplaceBlocked.
	browserURL    string
	browserClient *http.Client
}

// NewWildberriesSearchScraper.
//
//	base      — товарный скрейпер (nil → дефолтный);
//	pool      — пул прокси (nil → прямой клиент; для WB-поиска прокси не нужен);
//	tokens    — провайдер cookie-токена wbaas (nil → пустой статический);
//	maxPages  — макс. страниц (<=0 → 5);
//	pageDelay — пауза между страницами.
func NewWildberriesSearchScraper(base *WildberriesScraper, pool *ProxyPool, tokens TokenProvider, maxPages int, pageDelay time.Duration) *WildberriesSearchScraper {
	if base == nil {
		base = NewWildberriesScraper(5)
	}
	if pool == nil {
		pool, _ = NewProxyPool(nil, 12*time.Second)
	}
	if tokens == nil {
		tokens = StaticTokenProvider{}
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
		tokens:             tokens,
		maxPages:           maxPages,
		pageDelay:          pageDelay,
	}
}

var _ SearchScraper = (*WildberriesSearchScraper)(nil)

// SetBrowserSidecar подключает сайдкар wb-search-miner как 403-фолбэк: на 403
// direct запрос уходит в прогретый браузер (GET /search?query=&sort=&page=).
// Пустой URL — фолбэка нет (403 остаётся ошибкой). См. поле browserURL.
func (s *WildberriesSearchScraper) SetBrowserSidecar(baseURL string) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return
	}
	s.browserURL = baseURL
	// Сайдкар делает in-page fetch в браузере (навигация + челлендж) — щедрый
	// таймаут, тяжёлая дорожка отвечает не мгновенно.
	s.browserClient = &http.Client{Timeout: 45 * time.Second}
}

// MatchesSearch — WB-ссылка с текстовым поиском (?search=...).
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

// NormalizeSearchURL — канонический ключ дедупликации (query + sort).
func (s *WildberriesSearchScraper) NormalizeSearchURL(rawURL string) (string, error) {
	query, sortMode, err := s.parseSearchParams(rawURL)
	if err != nil {
		return "", err
	}
	canon := url.Values{}
	canon.Set("search", strings.ToLower(query))
	canon.Set("sort", sortMode)
	return "https://www.wildberries.ru/catalog/0/search.aspx?" + canon.Encode(), nil
}

// ScrapeSearch — постранично собрать выдачу.
func (s *WildberriesSearchScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	query, sortMode, err := s.parseSearchParams(rawURL)
	if err != nil {
		return nil, err
	}
	referer := "https://www.wildberries.ru/catalog/0/search.aspx?search=" + url.QueryEscape(query)

	out := &SearchResultSet{}
	position := 0

	// preferBrowser залипает на весь запрос: как только страница упёрлась в 403
	// direct и её спас браузер-сайдкар, остальные страницы идут сразу в сайдкар —
	// не тратим по заведомому 403 на страницу.
	var preferBrowser bool

	for page := 1; page <= s.maxPages; page++ {
		if page > 1 {
			s.sleep(ctx, s.pageDelay)
		}

		var body []byte
		if preferBrowser {
			body, err = s.fetchViaBrowser(ctx, query, sortMode, page)
		} else {
			body, err = s.fetchPage(ctx, buildSearchAPIURL(query, sortMode, page), referer)
			// direct заблокирован (обычно 403 на горячем) → уводим в браузер и
			// залипаем на нём до конца запроса.
			if err != nil && s.browserURL != "" {
				if bbody, berr := s.fetchViaBrowser(ctx, query, sortMode, page); berr == nil {
					body, err = bbody, nil
					preferBrowser = true
				}
			}
		}
		if err != nil {
			if out.PagesRead > 0 {
				break // частичный результат лучше, чем ошибка на всё
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
			break
		}

		for _, p := range parsed.Products {
			position++
			item := wbProductToItem(p, position)
			if item.PriceKopecks == 0 {
				continue
			}
			out.Items = append(out.Items, item)
		}

		if len(parsed.Products) < wbSearchPageSize {
			break
		}
	}

	return out, nil
}

// ── HTTP с токеном, ротацией прокси и backoff ────────────────────────────────

func (s *WildberriesSearchScraper) fetchPage(ctx context.Context, apiURL, referer string) ([]byte, error) {
	maxAttempts := s.pool.Size() + 2
	if tp, ok := s.tokens.(interface{ PoolSize() int }); ok {
		if n := tp.PoolSize() + 2; n > maxAttempts {
			maxAttempts = n
		}
	}
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

		tok, err := s.tokens.Token(ctx)
		if err != nil {
			lastErr = fmt.Errorf("%w: token provider: %v", ErrMarketplaceBlocked, err)
			s.sleep(ctx, delay)
			delay = bumpDelay(delay)
			continue
		}
		if !tok.Valid() {
			lastErr = fmt.Errorf("%w: пустой wbaas-токен (майнер не наполнил пул)", ErrMarketplaceBlocked)
			s.sleep(ctx, delay)
			delay = bumpDelay(delay)
			continue
		}
		ua := tok.UserAgent
		if ua == "" {
			ua = defaultSearchUA
		}

		pc := s.pool.next()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return nil, err
		}
		s.setHeaders(req, referer, tok.Cookie, ua)

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
			s.tokens.MarkGood(ctx, tok.Slot)
			metrics.WBSearchFetch.WithLabelValues("direct", "ok").Inc()
			return body, nil
		case resp.StatusCode == http.StatusForbidden:
			// wbaas режет горячий запрос: различие в транспорте (браузер vs
			// http.Client), не в токене — тот же токен проходит на редких запросах.
			// Ретраить direct бесполезно; выходим быстро, ScrapeSearch уводит в
			// браузер-сайдкар (см. docs/WB-SEARCH-STATUS.md).
			metrics.WBSearchFetch.WithLabelValues("direct", "forbidden").Inc()
			return nil, fmt.Errorf("%w: 403 via %s", ErrMarketplaceBlocked, pc.label)
		case resp.StatusCode == http.StatusTooManyRequests:
			// 429 + server: wbaas — токен протух/невалиден. После 2 подряд 429 на
			// слоте провайдер выводит его из ротации; следующая попытка (round-robin)
			// берёт другой токен из пула.
			s.tokens.MarkBad(ctx, tok.Slot)
			metrics.WBSearchFetch.WithLabelValues("direct", "429").Inc()
			lastErr = fmt.Errorf("%w: 429 via %s (slot %d, возможно протух токен)", ErrMarketplaceBlocked, pc.label, tok.Slot)
			s.sleep(ctx, delay)
			delay = bumpDelay(delay)
		default:
			metrics.WBSearchFetch.WithLabelValues("direct", "other").Inc()
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

// fetchViaBrowser — 403-фолбэк: одна страница выдачи из прогретого браузера
// сайдкара wb-search-miner (GET /search?query=&sort=&page=). Сайдкар делает
// in-page fetch к тому же u-search из доверенного контекста и отдаёт СЫРОЙ JSON
// той же формы (wbSearchResponse), зеркаля upstream-статус (403 при стойком
// челлендже). Токен тут не нужен — cookie живёт в самом браузере.
func (s *WildberriesSearchScraper) fetchViaBrowser(ctx context.Context, query, sortMode string, page int) ([]byte, error) {
	if s.browserURL == "" || s.browserClient == nil {
		return nil, fmt.Errorf("%w: browser sidecar not configured", ErrMarketplaceBlocked)
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("sort", sortMode)
	q.Set("page", strconv.Itoa(page))
	api := s.browserURL + "/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.browserClient.Do(req)
	if err != nil {
		metrics.WBSearchFetch.WithLabelValues("browser", "error").Inc()
		return nil, fmt.Errorf("%w: wb search sidecar: %v", ErrMarketplaceBlocked, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxSearchBodyBytes))

	switch resp.StatusCode {
	case http.StatusOK:
		metrics.WBSearchFetch.WithLabelValues("browser", "ok").Inc()
		return body, nil
	case http.StatusForbidden:
		metrics.WBSearchFetch.WithLabelValues("browser", "forbidden").Inc()
		return nil, fmt.Errorf("%w: 403 via sidecar (челлендж не пройден и в браузере)", ErrMarketplaceBlocked)
	default:
		// 502/503 — дорожка не прогрета/сайдкар недоступен.
		metrics.WBSearchFetch.WithLabelValues("browser", "error").Inc()
		return nil, fmt.Errorf("%w: sidecar status %d", ErrMarketplaceBlocked, resp.StatusCode)
	}
}

func (s *WildberriesSearchScraper) setHeaders(req *http.Request, referer, cookie, ua string) {
	// Близко к реальному запросу браузера к __internal/u-search.
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "ru,en;q=0.9")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Referer", referer)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
}

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
	query = strings.Join(strings.Fields(query), " ")
	sortMode = strings.TrimSpace(strings.ToLower(q.Get("sort")))
	if sortMode == "" {
		sortMode = "popular"
	}
	return query, sortMode, nil
}

// buildSearchAPIURL — URL запроса к u-search v18. Набор параметров —
// семантически нейтральный минимум, проверенный на живом 200-ответе.
func buildSearchAPIURL(query, sortMode string, page int) string {
	q := url.Values{}
	q.Set("appType", "1")
	q.Set("curr", "rub")
	q.Set("dest", "-1257786")
	q.Set("inheritFilters", "false")
	q.Set("lang", "ru")
	q.Set("locale", "ru")
	q.Set("page", strconv.Itoa(page))
	q.Set("query", query)
	q.Set("resultset", "catalog")
	q.Set("sort", sortMode)
	q.Set("spp", "30")
	q.Set("suppressSpellcheck", "false")
	return wbSearchAPIBase + "?" + q.Encode()
}

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

func wbImageURL(id int64) string {
	vol := id / 100000
	part := id / 1000
	basket := wbBasketNumber(id)
	return fmt.Sprintf("https://basket-%02d.wbbasket.ru/vol%d/part%d/%d/images/big/1.webp",
		basket, vol, part, id)
}

// ── Структуры ответа u-search v18 (products в КОРНЕ) ─────────────────────────

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
