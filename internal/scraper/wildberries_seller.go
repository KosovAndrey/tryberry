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
)

const (
	// Открытый каталог-эндпоинт витрины продавца: отдаёт {products,total} БЕЗ
	// wbaas-токена и без прокси (в отличие от текстового поиска). Форма ответа —
	// как у u-search, поэтому переиспользуем wbSearchResponse/wbProductToItem.
	wbSellerAPIBase  = "https://catalog.wb.ru/sellers/v4/catalog"
	wbSellerPageSize = 100

	// Открытый supplier-by-id: имя/реквизиты продавца. Поле trademark — бренд
	// витрины (то, что видит юзер вверху страницы магазина).
	wbSupplierInfoBase = "https://static-basket-01.wbbasket.ru/vol0/data/supplier-by-id"

	// Открытый конфиг кастомной витрины магазина (constructor): по буквенному
	// слагу /seller/{slug} отдаёт JSON с числовым supplierID. Так фронт WB и
	// резолвит буквенные ссылки — БЕЗ токена и антибота.
	wbShopConfigBase = "https://static-basket-01.wbbasket.ru/vol0/constructor-api/shops/v3"

	// tbTextParam — наш «клиентский» текст-фильтр в ссылке подписки. Применяется
	// на нашей стороне к name/brand карточек (для магазинов, где нет хороших
	// фильтров WB); в WB-API НЕ уходит. Префикс tb_ — чтобы не пересечься с
	// параметрами WB.
	tbTextParam = "tb_q"
)

// wbSellerURLRe — путь /seller/{id}, где id = supplier_id WB.
var wbSellerURLRe = regexp.MustCompile(`/seller/(\d+)`)

// WildberriesSellerScraper — скрейпер витрины продавца WB как поисковой выдачи.
//
// Встраивает товарный *WildberriesScraper (удовлетворяет MarketplaceScraper) и
// реализует SearchScraper: «запрос» здесь — supplier_id + фильтры WB из ссылки,
// плюс опциональный клиентский текст-фильтр (tb_q). Антибот не нужен: каталог
// продавца открыт, ходим простым клиентом без токена и прокси.
type WildberriesSellerScraper struct {
	*WildberriesScraper

	client    *http.Client
	apiBase   string
	maxPages  int
	pageDelay time.Duration

	// fetch — получить тело страницы по URL. Поле (а не прямой вызов метода),
	// чтобы в тестах подменять сетевой слой фейком и проверять логику пагинации/
	// фильтрации герметично, без сети.
	fetch func(ctx context.Context, apiURL string) ([]byte, error)
}

// NewWildberriesSellerScraper.
//
//	base      — товарный скрейпер (nil → дефолтный);
//	maxPages  — макс. страниц по 100 (<=0 → 5); это же и есть CAP на размер выдачи;
//	pageDelay — пауза между страницами.
func NewWildberriesSellerScraper(base *WildberriesScraper, maxPages int, pageDelay time.Duration) *WildberriesSellerScraper {
	if base == nil {
		base = NewWildberriesScraper(5)
	}
	if maxPages <= 0 {
		maxPages = 5
	}
	if pageDelay < 0 {
		pageDelay = 0
	}
	s := &WildberriesSellerScraper{
		WildberriesScraper: base,
		// Переиспользуемый клиент с тюнингованным пулом keep-alive (per-host 32):
		// открытый каталог продавца (catalog.wb.ru) без антибота, ходим постранично.
		client:             &http.Client{Timeout: 12 * time.Second, Transport: newTunedHTTPTransport()},
		apiBase:            wbSellerAPIBase,
		maxPages:           maxPages,
		pageDelay:          pageDelay,
	}
	s.fetch = s.fetchSellerPage
	return s
}

// ResolveVanity резолвит буквенный слаг витрины (/seller/moderndevice) в числовой
// supplierID через открытый конфиг кастомной витрины (constructor-api/shops/v3/
// {slug}.json) — так же, как это делает фронт WB. Токен/антибот не нужны. 404 на
// слаг (нет кастомной витрины) → ошибка, бот покажет хинт.
func (s *WildberriesSellerScraper) ResolveVanity(ctx context.Context, slug string) (string, error) {
	body, err := s.fetch(ctx, fmt.Sprintf("%s/%s.json", wbShopConfigBase, url.PathEscape(slug)))
	if err != nil {
		return "", err
	}
	var cfg struct {
		SupplierID json.Number `json:"supplierID"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return "", fmt.Errorf("%w: decode shop config: %v", ErrParseFailed, err)
	}
	id := cfg.SupplierID.String()
	if id == "" || id == "0" {
		return "", fmt.Errorf("%w: no supplierID for slug %q", ErrProductNotFound, slug)
	}
	return id, nil
}

var _ SearchScraper = (*WildberriesSellerScraper)(nil)

// MaxItems — потолок размера выдачи (CAP): maxPages × 100. Для гейта подключения.
func (s *WildberriesSellerScraper) MaxItems() int { return s.maxPages * wbSellerPageSize }

// MatchesSearch — WB-ссылка на витрину продавца (/seller/{id}).
func (s *WildberriesSellerScraper) MatchesSearch(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if !strings.Contains(strings.ToLower(u.Host), "wildberries.ru") {
		return false
	}
	return wbSellerURLRe.MatchString(u.Path)
}

// NormalizeSearchURL — канонический ключ дедупликации: supplier + значимые
// фильтры WB + клиентский текст. Навигационный шум (page/dest/spp/…) отброшен,
// значения мультизначных фильтров отсортированы → одинаковый смысл = один ключ.
func (s *WildberriesSellerScraper) NormalizeSearchURL(rawURL string) (string, error) {
	supplierID, filters, text, err := s.parseSellerParams(rawURL)
	if err != nil {
		return "", err
	}
	canon := url.Values{}
	canon.Set("supplier", supplierID)
	for k, vs := range filters {
		canon[k] = vs // уже отсортированы в parseSellerParams
	}
	if text != "" {
		canon.Set(tbTextParam, text)
	}
	return "https://www.wildberries.ru/seller/" + supplierID + "?" + canon.Encode(), nil
}

// ScrapeSearch — постранично собрать витрину продавца (до maxPages = CAP),
// применив клиентский текст-фильтр. Частичный результат при сбое на поздних
// страницах допустим.
func (s *WildberriesSellerScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	supplierID, filters, text, err := s.parseSellerParams(rawURL)
	if err != nil {
		return nil, err
	}
	tokens := textTokens(text)

	out := &SearchResultSet{}
	position := 0

	for page := 1; page <= s.maxPages; page++ {
		if page > 1 {
			s.sleep(ctx, s.pageDelay)
		}

		body, err := s.fetch(ctx, s.buildSellerAPIURL(supplierID, filters, page))
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
			return nil, fmt.Errorf("decode seller page %d: %w", page, err)
		}

		out.PagesRead = page
		if parsed.Total > 0 {
			out.TotalFound = parsed.Total
		}
		if len(parsed.Products) == 0 {
			break
		}

		for _, p := range parsed.Products {
			item := wbProductToItem(p, 0)
			if item.PriceKopecks == 0 {
				continue
			}
			if !sellerTextMatches(item, tokens) {
				continue
			}
			position++
			item.Position = position
			out.Items = append(out.Items, item)
		}

		if len(parsed.Products) < wbSellerPageSize {
			break
		}
	}

	return out, nil
}

// SellerTotal — размер выдачи (одна страница, читаем total). Для гейта CAP при
// подключении: дёшево понять, укладывается ли магазин+фильтры в лимит.
func (s *WildberriesSellerScraper) SellerTotal(ctx context.Context, rawURL string) (int, error) {
	supplierID, filters, _, err := s.parseSellerParams(rawURL)
	if err != nil {
		return 0, err
	}
	body, err := s.fetch(ctx, s.buildSellerAPIURL(supplierID, filters, 1))
	if err != nil {
		return 0, err
	}
	var parsed wbSearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("decode seller total: %w", err)
	}
	return parsed.Total, nil
}

// SellerName — отображаемое имя магазина из открытого supplier-by-id: trademark
// (бренд-витрина, напр. «КАПИБАРА»), фолбэк — supplierName (юрлицо). Пустая
// строка, если имя не нашли. Для человекочитаемого ярлыка подписки.
func (s *WildberriesSellerScraper) SellerName(ctx context.Context, rawURL string) (string, error) {
	supplierID, _, _, err := s.parseSellerParams(rawURL)
	if err != nil {
		return "", err
	}
	body, err := s.fetch(ctx, fmt.Sprintf("%s/%s.json", wbSupplierInfoBase, supplierID))
	if err != nil {
		return "", err
	}
	var info struct {
		Trademark    string `json:"trademark"`
		SupplierName string `json:"supplierName"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("decode supplier info: %w", err)
	}
	if name := strings.TrimSpace(info.Trademark); name != "" {
		return name, nil
	}
	return strings.TrimSpace(info.SupplierName), nil
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

// sellerFetchAttempts — попыток на страницу. catalog.wb.ru отдаёт 429 как burst-
// лимит по IP (особенно на reseller-кадансе 1 мин и общем egress с текстовым
// поиском); обычно проходит со 2-й попытки. Прокси у открытого каталога нет —
// лечим бэкоффом.
const sellerFetchAttempts = 3

func (s *WildberriesSellerScraper) fetchSellerPage(ctx context.Context, apiURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < sellerFetchAttempts; attempt++ {
		if attempt > 0 {
			// Экспоненциальный бэкофф 1с→2с (cap 4с) перед повтором.
			s.sleep(ctx, sellerBackoff(attempt))
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", wbUserAgent)
		req.Header.Set("Accept", "*/*")

		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%w: seller catalog request: %v", ErrMarketplaceBlocked, err)
			continue // транзиентная сетевая ошибка — повторяем
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxSearchBodyBytes))
			resp.Body.Close()
			return body, rerr
		case resp.StatusCode == http.StatusTooManyRequests:
			resp.Body.Close()
			lastErr = fmt.Errorf("%w: 429 from seller catalog", ErrMarketplaceBlocked)
			continue // burst-лимит — бэкофф и повтор
		default:
			status := resp.StatusCode
			resp.Body.Close()
			return nil, fmt.Errorf("seller catalog status %d", status)
		}
	}
	return nil, lastErr
}

// sellerBackoff — 1с, 2с, 4с… с потолком maxBackoffDelay.
func sellerBackoff(attempt int) time.Duration {
	d := time.Second << (attempt - 1)
	if d > maxBackoffDelay {
		return maxBackoffDelay
	}
	return d
}

func (s *WildberriesSellerScraper) sleep(ctx context.Context, d time.Duration) {
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

// ── Парсинг URL и сборка запроса ─────────────────────────────────────────────

// parseSellerParams достаёт из ссылки витрины supplier_id, значимые фильтры WB
// (в API уходят, в дедупе участвуют) и клиентский текст-фильтр tb_q. Значения
// мультизначных фильтров (f5023=a;b;c) сортируются для стабильного ключа.
func (s *WildberriesSellerScraper) parseSellerParams(rawURL string) (supplierID string, filters url.Values, text string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	m := wbSellerURLRe.FindStringSubmatch(u.Path)
	if len(m) < 2 {
		return "", nil, "", fmt.Errorf("%w: not a wildberries seller URL", ErrInvalidURL)
	}
	supplierID = m[1]

	// Парсим RawQuery вручную: url.Query() с Go 1.17 ОТБРАСЫВАЕТ пары, содержащие
	// «;» (а фильтры WB вида f5023=a;b;c именно такие) — стандартный парсер их бы
	// потерял. Сами делим только по «&».
	filters = url.Values{}
	for k, vs := range parseRawQuery(u.RawQuery) {
		if k == tbTextParam {
			text = normalizeText(vs[0])
			continue
		}
		if !isSellerFilterKey(k) {
			continue
		}
		for _, v := range vs {
			filters.Add(k, sortSemicolonValues(v))
		}
	}
	return supplierID, filters, text, nil
}

// parseRawQuery разбирает query-строку, сохраняя «;» внутри значений (в отличие
// от url.ParseQuery, который с Go 1.17 отбрасывает такие пары).
func parseRawQuery(raw string) url.Values {
	vals := url.Values{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		key, err := url.QueryUnescape(k)
		if err != nil {
			continue
		}
		val, err := url.QueryUnescape(v)
		if err != nil {
			continue
		}
		vals.Add(key, val)
	}
	return vals
}

// isSellerFilterKey — значимо ли влияет query-параметр на состав выдачи (и потому
// прокидывается в API и входит в ключ дедупа). Динамические фильтры WB имеют вид
// f{цифры}/fbrand/fdlvr (префикс «f»); плюс явные xsubject/sort/priceU.
func isSellerFilterKey(k string) bool {
	switch k {
	case "xsubject", "sort", "priceU":
		return true
	}
	return strings.HasPrefix(k, "f") && len(k) > 1
}

// sortSemicolonValues упорядочивает значения вида "b;a;c" → "a;b;c", чтобы
// одинаковый набор фильтров давал один ключ независимо от порядка в ссылке.
func sortSemicolonValues(v string) string {
	if !strings.Contains(v, ";") {
		return v
	}
	parts := strings.Split(v, ";")
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func (s *WildberriesSellerScraper) buildSellerAPIURL(supplierID string, filters url.Values, page int) string {
	q := url.Values{}
	q.Set("appType", "1")
	q.Set("curr", "rub")
	q.Set("dest", "-1257786")
	q.Set("page", strconv.Itoa(page))
	q.Set("sort", "popular")
	q.Set("spp", "30")
	q.Set("supplier", supplierID)
	// Фильтры из ссылки имеют приоритет (в т.ч. могут переопределить sort).
	for k, vs := range filters {
		q[k] = vs
	}
	return s.apiBase + "?" + q.Encode()
}

// ── Клиентский текст-фильтр ──────────────────────────────────────────────────

func normalizeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func textTokens(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Fields(text)
}

// sellerTextMatches — все слова фильтра присутствуют в name/brand (AND, регистр
// игнорируем). Пустой фильтр пропускает всё.
func sellerTextMatches(it SearchItem, tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	hay := strings.ToLower(it.Name + " " + it.Brand)
	for _, t := range tokens {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}
