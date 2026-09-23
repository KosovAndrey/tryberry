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
	// apiBase — база u-search. Пусто → wbSearchAPIBase (same-origin проксик за
	// wbaas, требует cookie-токен). Замер 2026-08-24: ПУБЛИЧНЫЙ search.wb.ru
	// отдаёт ту же выдачу БЕЗ токена и без браузера (200, фильтр xsubject
	// работает) — при живом egress это снимает с критического пути и токен-пул,
	// и браузерный сайдкар со стеной wbaas. Переключается WB_SEARCH_API_BASE.
	apiBase string

	browserURL string
	// searchDirectOff — не ходить в публичную ручку вовсе. См. SetSearchDirect.
	searchDirectOff bool
	browserClient   *http.Client
	// browserMaxPages — сколько страниц тянуть через сайдкар (навигация-перехват
	// нативного ответа фронта). Дорого (навигация на страницу), поэтому по
	// умолчанию 1 (топ-100 — для горячих запросов достаточно). <=0 → 1.
	browserMaxPages int
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

// SetAPIBase переключает базу u-search (пусто → дефолтный same-origin проксик).
// Публичный хост (search.wb.ru) токена не требует — см. поле apiBase.
func (s *WildberriesSearchScraper) SetAPIBase(base string) {
	s.apiBase = strings.TrimRight(strings.TrimSpace(base), "?&")
}

// searchAPIBase — действующая база запроса.
func (s *WildberriesSearchScraper) searchAPIBase() string {
	if s.apiBase == "" {
		return wbSearchAPIBase
	}
	return s.apiBase
}

// tokenRequired — нужен ли cookie-токен wbaas. Он нужен ТОЛЬКО same-origin
// проксику на www.wildberries.ru: публичный search.wb.ru пускает без него, и
// требовать токен там означало бы падать из-за пустого пула на ровном месте.
func (s *WildberriesSearchScraper) tokenRequired() bool {
	return strings.Contains(s.searchAPIBase(), "wildberries.ru")
}

// SetBrowserSidecar подключает сайдкар wb-search-miner как 403-фолбэк: на 403
// direct запрос уходит в прогретый браузер (GET /search?query=&sort=&page=).
// maxPages — сколько страниц тянуть через сайдкар (<=0 → 1). Пустой URL —
// фолбэка нет (403 остаётся ошибкой). См. поля browserURL/browserMaxPages.
//
// Потолок страниц раньше упирался в механику: сайдкар навигировал браузер на
// страницу выдачи, а `&page=N` в URL каталога WB игнорировал и всегда отдавал
// первую. С 23-09-2026 сайдкар ходит in-page fetch'ем прямо в __internal, где
// пагинация работает, — ограничение осталось только вопросом цены запроса.
// SetSearchDirect выключает публичную ручку поиска: false — сразу в сайдкар.
// Рычаг в .env (WB_SEARCH_DIRECT), чтобы вернуть direct без выкатки, когда WB
// снова откроет публичный хост.
func (s *WildberriesSearchScraper) SetSearchDirect(v bool) { s.searchDirectOff = !v }

func (s *WildberriesSearchScraper) SetBrowserSidecar(baseURL string, maxPages int) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if maxPages <= 0 {
		maxPages = 1
	}
	s.browserMaxPages = maxPages
	if baseURL == "" {
		return
	}
	s.browserURL = baseURL
	// Сайдкар делает in-page fetch в браузере (навигация + челлендж) — щедрый
	// таймаут, тяжёлая дорожка отвечает не мгновенно.
	// Переиспользуемый клиент к сайдкару (внутренний хост) с пулом keep-alive.
	s.browserClient = &http.Client{Timeout: 45 * time.Second, Transport: newTunedHTTPTransport()}
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

// NormalizeSearchURL — канонический ключ дедупликации (query + sort + фильтры).
// Он же уходит пользователю кнопкой «Открыть выдачу», поэтому остаётся рабочей
// ссылкой WB: фильтры пишем в родном виде (priceU=1343700;15000000), без
// %3B-экранирования точки с запятой.
func (s *WildberriesSearchScraper) NormalizeSearchURL(rawURL string) (string, error) {
	query, sortMode, filters, err := s.parseSearchParams(rawURL)
	if err != nil {
		return "", err
	}
	return canonicalSearchURL(query, sortMode, filters), nil
}

// canonicalSearchURL — стабильная ссылка на выдачу: search+sort, затем фильтры
// в порядке ключей (и значений внутри ключа), чтобы один и тот же набор давал
// один ключ независимо от порядка в исходной ссылке.
func canonicalSearchURL(query, sortMode string, filters url.Values) string {
	canon := url.Values{}
	canon.Set("search", strings.ToLower(query))
	canon.Set("sort", sortMode)
	out := "https://www.wildberries.ru/catalog/0/search.aspx?" + canon.Encode()
	if f := encodeFilterQuery(filters); f != "" {
		out += "&" + f
	}
	return out
}

// encodeFilterQuery — фильтры как готовый кусок query-строки WB
// («priceU=1343700;15000000&xsubject=3274»), в том же каноническом порядке, что
// и в ссылке. Пусто, если фильтров нет.
func encodeFilterQuery(filters url.Values) string {
	keys := make([]string, 0, len(filters))
	for k := range filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), filters[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			// «;» оставляем литеральной — так фильтр выглядит в ссылке WB.
			parts = append(parts, k+"="+strings.ReplaceAll(url.QueryEscape(v), "%3B", ";"))
		}
	}
	return strings.Join(parts, "&")
}

// ScrapeSearch — постранично собрать выдачу.
func (s *WildberriesSearchScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	query, sortMode, filters, err := s.parseSearchParams(rawURL)
	if err != nil {
		return nil, err
	}
	referer := canonicalSearchURL(query, sortMode, filters)
	priceRange := parseWBPriceRange(filters.Get("priceU"))

	out := &SearchResultSet{}
	position := 0

	// preferBrowser залипает на весь запрос: как только страница упёрлась в 403
	// direct и её спас браузер-сайдкар, остальные страницы идут сразу в сайдкар.
	// browserUsed ограничивает число браузер-страниц (навигация дорогая).
	// direct по умолчанию пробуем первым, но когда публичная ручка закрыта
	// насовсем (с 23-09-2026), каждый такой заход — заведомый 403, который мы
	// же и шлём в адрес WB. WB_SEARCH_DIRECT=false уводит сразу в браузер.
	preferBrowser := s.searchDirectOff && s.browserURL != ""
	browserUsed := 0

	for page := 1; page <= s.maxPages; page++ {
		if page > 1 {
			s.sleep(ctx, s.pageDelay)
		}

		var body []byte
		if preferBrowser {
			if browserUsed >= s.browserMaxPages {
				break // лимит браузер-страниц исчерпан — партиал (топ-N) достаточно
			}
			body, err = s.fetchViaBrowser(ctx, query, sortMode, page, filters)
			browserUsed++
		} else {
			body, err = s.fetchPage(ctx, buildSearchAPIURL(s.searchAPIBase(), query, sortMode, page, filters), referer)
			// direct заблокирован (обычно 403 на горячем) → уводим в браузер и
			// залипаем на нём до конца запроса.
			if err != nil && s.browserURL != "" && browserUsed < s.browserMaxPages {
				if bbody, berr := s.fetchViaBrowser(ctx, query, sortMode, page, filters); berr == nil {
					body, err = bbody, nil
					preferBrowser = true
					browserUsed++
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
			item := wbProductToItem(p, position+1)
			if item.PriceKopecks == 0 {
				continue
			}
			// Ценовой фильтр досеиваем У СЕБЯ: замером 2026-08-24 подтверждено, что
			// u-search параметр priceU ИГНОРИРУЕТ (786 результатов и max 1 295 100 ₽
			// при priceU=1343700;15000000 — ровно как без фильтра), тогда как
			// xsubject честно сужает (786 → 404). Наверх priceU всё равно шлём: если
			// WB его починит, до нас доедет уже отфильтрованная страница.
			if !priceRange.contains(item.PriceKopecks) {
				continue
			}
			position++
			item.Position = position
			out.Items = append(out.Items, item)
		}

		if len(parsed.Products) < wbSearchPageSize {
			break
		}
	}

	return out, nil
}

// wbPriceRange — ценовой диапазон из priceU=<мин>;<макс> (обе границы в
// КОПЕЙКАХ, как и цены в ответе u-search). Нулевая граница — «не задана».
type wbPriceRange struct{ min, max int64 }

// contains — попадает ли цена в диапазон. Пустой диапазон пропускает всё.
func (r wbPriceRange) contains(kopecks int64) bool {
	if r.min > 0 && kopecks < r.min {
		return false
	}
	if r.max > 0 && kopecks > r.max {
		return false
	}
	return true
}

// parseWBPriceRange разбирает «1343700;15000000». Мусор → пустой диапазон
// (фильтруем как раньше, ничего не теряя).
func parseWBPriceRange(v string) wbPriceRange {
	lo, hi, ok := strings.Cut(strings.TrimSpace(v), ";")
	if !ok {
		return wbPriceRange{}
	}
	var r wbPriceRange
	if n, err := strconv.ParseInt(strings.TrimSpace(lo), 10, 64); err == nil && n > 0 {
		r.min = n
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(hi), 10, 64); err == nil && n > 0 {
		r.max = n
	}
	if r.min > 0 && r.max > 0 && r.min > r.max {
		return wbPriceRange{} // границы перепутаны — не режем выдачу в ноль
	}
	return r
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

		needToken := s.tokenRequired()
		// На публичном хосте пул токенов НЕ трогаем вовсе: cookie wbaas туда
		// не нужна (и незачем светить её на чужом домене), а 429 оттуда —
		// это rate-limit, а не протухший токен, и слоты за него жечь нельзя.
		tok := SearchToken{Slot: -1}
		if needToken {
			t, err := s.tokens.Token(ctx)
			if err != nil {
				lastErr = fmt.Errorf("%w: token provider: %v", ErrMarketplaceBlocked, err)
				s.sleep(ctx, delay)
				delay = bumpDelay(delay)
				continue
			}
			if !t.Valid() {
				lastErr = fmt.Errorf("%w: пустой wbaas-токен (майнер не наполнил пул)", ErrMarketplaceBlocked)
				s.sleep(ctx, delay)
				delay = bumpDelay(delay)
				continue
			}
			tok = t
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
			// Считаем ОБЯЗАТЕЛЬНО: сюда попадает мёртвый egress (прокси не
			// пропускает, узел лёг). Без метрики такой отказ выглядит не всплеском
			// ошибок, а ТИШИНОЙ в direct — ровно то, что запутало разбор 27-08,
			// когда весь поиск молча уехал в браузерный сайдкар.
			metrics.WBSearchFetch.WithLabelValues("direct", "error").Inc()
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
			// берёт другой токен из пула. На публичном хосте токена нет — там 429
			// это обычный rate-limit, лечится только паузой (Slot -1 пул игнорит).
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
// сайдкара wb-search-miner (GET /search?query=&sort=&page=). Сайдкар навигирует
// браузер на страницу запроса и ПЕРЕХВАТЫВАЕТ нативный ответ u-search фронта
// (ручной fetch wbaas отвергает 403) — отдаёт СЫРОЙ JSON той же формы
// (wbSearchResponse), зеркаля upstream-статус. Токен тут не нужен — cookie в браузере.
func (s *WildberriesSearchScraper) fetchViaBrowser(ctx context.Context, query, sortMode string, page int, filters url.Values) ([]byte, error) {
	if s.browserURL == "" || s.browserClient == nil {
		return nil, fmt.Errorf("%w: browser sidecar not configured", ErrMarketplaceBlocked)
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("sort", sortMode)
	q.Set("page", strconv.Itoa(page))
	// Фильтры отдаём сайдкару одной строкой (готовый кусок query WB): он дописывает
	// её в URL страницы навигации, чтобы фронт запросил у u-search ту же выдачу.
	if f := encodeFilterQuery(filters); f != "" {
		q.Set("filters", f)
	}
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

// parseSearchParams — текст запроса, сортировка и ФИЛЬТРЫ выдачи из ссылки.
// Фильтры (цена/предмет/бренд) сужают выдачу так же, как у витрины продавца:
// потеряв их, мы следим не за тем, что выбрал пользователь (ссылка «rtx 5080
// дороже 13 437 ₽, предмет "видеокарты"» без них превращается в голое «rtx 5080»
// и приносит наклейки с кулерами). Поэтому парсим их и прокидываем и в API,
// и в ключ дедупа.
func (s *WildberriesSearchScraper) parseSearchParams(rawURL string) (query, sortMode string, filters url.Values, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	// RawQuery разбираем вручную: url.Query() с Go 1.17 отбрасывает пары с «;»,
	// а фильтры WB именно такие (priceU=1343700;15000000, f5023=a;b;c).
	q := parseRawQuery(u.RawQuery)
	query = strings.TrimSpace(q.Get("search"))
	if query == "" {
		return "", "", nil, fmt.Errorf("%w: no search query in URL", ErrInvalidURL)
	}
	query = strings.Join(strings.Fields(query), " ")
	sortMode = strings.TrimSpace(strings.ToLower(q.Get("sort")))
	if sortMode == "" {
		sortMode = "popular"
	}
	filters = url.Values{}
	for k, vs := range q {
		canon, ok := searchFilterKey(k)
		if !ok {
			continue
		}
		for _, v := range vs {
			if v = strings.TrimSpace(v); v != "" {
				filters.Add(canon, sortSemicolonValues(v))
			}
		}
	}
	return query, sortMode, filters, nil
}

// wbSearchFacetRe — числовой фасет WB (f5023=..., f204557=...): предмет/бренд/
// цвет/характеристика. Именованные фильтры — в wbSearchFilterNames.
var wbSearchFacetRe = regexp.MustCompile(`^f\d+$`)

// wbSearchFilterNames — именованные фильтры выдачи WB в КАНОНИЧЕСКОМ написании
// (u-search чувствителен к регистру: priceU, не priceu). Ключ карты — нижний
// регистр, значение — как слать в API. Белый список, а не «всё кроме трекинга»:
// трекинг-параметры у WB бесконечны, фильтры наперечёт.
var wbSearchFilterNames = map[string]string{
	"priceu":    "priceU",
	"dprice":    "dprice",
	"xsubject":  "xsubject",
	"subject":   "subject",
	"fbrand":    "fbrand",
	"fsupplier": "fsupplier",
	"fcolor":    "fcolor",
	"fdlvr":     "fdlvr",
	"fkind":     "fkind",
	"frating":   "frating",
	"foriginal": "foriginal",
}

// searchFilterKey — фильтр ли это выдачи, и как он пишется в API.
func searchFilterKey(k string) (string, bool) {
	if canon, ok := wbSearchFilterNames[strings.ToLower(strings.TrimSpace(k))]; ok {
		return canon, true
	}
	if wbSearchFacetRe.MatchString(k) {
		return k, true
	}
	return "", false
}

// buildSearchAPIURL — URL запроса к u-search v18. Набор параметров —
// семантически нейтральный минимум, проверенный на живом 200-ответе.
func buildSearchAPIURL(base, query, sortMode string, page int, filters url.Values) string {
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
	// Фильтры из ссылки — последними: они и есть выбор пользователя.
	for k, vs := range filters {
		q[k] = vs
	}
	if base == "" {
		base = wbSearchAPIBase
	}
	return base + "?" + q.Encode()
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
	// TotalQuantity/Sizes[].Stocks — НАСТОЯЩЕЕ наличие. Цена в карточке остаётся и
	// после того, как товар кончился, поэтому «цена > 0» наличием не является
	// (замер 01-09-2026: у OOS-товара totalQuantity=0 и stocks пустые, у живых
	// totalQuantity совпадает с суммой qty по складам).
	TotalQuantity int64    `json:"totalQuantity"`
	Sizes         []wbSize `json:"sizes"`
}

// wbSize — размер/оффер товара. Тип именованный, а не анонимный: к нему
// обращаются тесты, и каждое новое поле в анонимной структуре ломало бы их сборку.
type wbSize struct {
	Price struct {
		Basic   int64 `json:"basic"`
		Product int64 `json:"product"`
		Total   int64 `json:"total"`
	} `json:"price"`
	// Stocks — остатки по складам. Пустой список = товара нет, даже если цена есть.
	Stocks []struct {
		Qty int64 `json:"qty"`
	} `json:"stocks"`
}
