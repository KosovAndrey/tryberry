package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/time/rate"

	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

// YandexMarketScraper получает цену/название/картинку товара Я.Маркета по URL
// карточки — БЕЗ аккаунта (цена публичная, в отличие от Ozon).
//
// Почему так, а не чистый net/http: market.yandex.ru закрыт SmartCaptcha, который
// на «голом» Go-TLS-отпечатке отдаёт страницу капчи вместо карточки (из-за этого
// старую реализацию пришлось выключить, коммит 86b1712). Рабочий рецепт — хороший
// TLS-отпечаток (bogdanfinn/tls-client, профиль Chrome). Аккаунт не нужен — берём
// публичную карточку и парсим встроенный JSON-LD (<script type="application/
// ld+json"> → @type:Product → offers.price) с фолбэком на стейт marketfront.
//
// Транспорт — схема direct+proxy с ОБЩИМ cookie-jar (как у AliExpress):
//   - direct (без прокси, датацентр-IP) — основной путь. Probe-эксперимент показал:
//     с хорошим TLS-отпечатком датацентр-IP держит поток карточек без капчи и без
//     прогрева cookie (100/100 запросов, 0 блоков — см. docs/YANDEX-WARMED-COOKIES.md).
//   - proxy (RU-мобильный/резидентский) — fallback ТОЛЬКО когда direct упёрся в
//     SmartCaptcha: один запрос через прокси проходит антибот и попутно обновляет
//     cookie в общем jar. Дальше снова direct. Прокси опционален: без него работает
//     только direct (если IP однажды заблокируют — скрейпер начнёт отдавать blocked).
type YandexMarketScraper struct {
	direct     tls_client.HttpClient // без прокси — основной путь (датацентр-IP)
	proxy      tls_client.HttpClient // через RU-прокси — fallback на капчу (общий jar); nil без прокси
	limiter    *rate.Limiter
	log        *slog.Logger
	configured bool
}

// YandexMarketOptions — конфигурация скрейпера. Нулевое значение даёт «облегчённый»
// скрейпер: Matches работает (нужно боту/api для разбора URL), а Scrape вернёт
// ErrNotImplemented. Реальный скрейп включается всегда, когда удаётся поднять
// tls-client (аккаунт не требуется). ProxyURL опционален: direct — основной путь,
// прокси нужен лишь как fallback, если датацентр-IP однажды начнёт ловить капчу.
type YandexMarketOptions struct {
	ProxyURL string  // http://user:pass@host:port RU-прокси (fallback на капчу; опционален)
	RPS      float64 // лимит запросов к Я.Маркету (один IP → держим низким), 0 → 1
	Logger   *slog.Logger
}

func NewYandexMarketScraper(opts YandexMarketOptions) *YandexMarketScraper {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	rps := opts.RPS
	if rps <= 0 {
		rps = 1
	}

	s := &YandexMarketScraper{
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
		log:     log,
	}

	direct, proxy, err := newYandexTLSClients(opts.ProxyURL)
	if err != nil {
		log.Error("yandex market: tls-client init failed, scraper disabled", "err", err)
		return s
	}
	s.direct = direct
	s.proxy = proxy
	s.configured = true
	log.Info("yandex market scraper configured", "transport", "direct+proxy-fallback", "proxy", proxy != nil)
	return s
}

// newYandexTLSClients — Chrome-профиль TLS (карточку отдаёт web), таймаут с запасом
// под тяжёлый HTML (~2.5 МБ) и редиректы. Два клиента с ОБЩИМ cookie-jar: direct
// (основной) и proxy (fallback на капчу). Общий jar — чтобы cookie, добытые через
// прокси при обходе SmartCaptcha, сразу были видны direct-клиенту. proxy == nil,
// если ProxyURL пуст (тогда работает только direct).
func newYandexTLSClients(proxyURL string) (direct, proxy tls_client.HttpClient, err error) {
	jar := tls_client.NewCookieJar()
	base := func() []tls_client.HttpClientOption {
		return []tls_client.HttpClientOption{
			tls_client.WithTimeoutSeconds(25),
			tls_client.WithClientProfile(profiles.Chrome_146),
			tls_client.WithCookieJar(jar),
		}
	}
	direct, err = tls_client.NewHttpClient(tls_client.NewNoopLogger(), base()...)
	if err != nil {
		return nil, nil, err
	}
	if proxyURL != "" {
		proxy, err = tls_client.NewHttpClient(tls_client.NewNoopLogger(), append(base(), tls_client.WithProxyUrl(proxyURL))...)
		if err != nil {
			return nil, nil, err
		}
	}
	return direct, proxy, nil
}

func (s *YandexMarketScraper) Marketplace() Marketplace { return MarketplaceYandexMarket }

func (s *YandexMarketScraper) Matches(url string) bool {
	return strings.Contains(url, "market.yandex.ru/")
}

func (s *YandexMarketScraper) Scrape(ctx context.Context, url string) (*Result, error) {
	if !s.configured {
		return nil, fmt.Errorf("%w: yandex market scraper not configured", ErrNotImplemented)
	}
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	status, body, source, err := s.getWithFallback(ctx, url, ymCardHeader(), 6<<20)
	if err != nil {
		return nil, err
	}
	metrics.YandexPriceSource.WithLabelValues(source).Inc()

	if status == 404 {
		return nil, ErrProductNotFound
	}

	// Парсим В ПЕРВУЮ ОЧЕРЕДЬ. SmartCaptcha отдаёт капчу со статусом 200, а на
	// настоящей странице товара в JS-бандле всё равно встречается слово "captcha"
	// — поэтому блок/успех решаем ПО СОДЕРЖИМОМУ (достали ли товар), а не по
	// наличию подстроки (раньше это давало ложный «blocked» на живой странице).
	if res, perr := parseYandexMarketHTML(string(body), url); perr == nil {
		if !res.InStock {
			// Диагностика «последней цены» при OOS: сколько ценовых сниппетов в
			// стейте и какую выбрали (по близости к SKU). Если last_price скачет —
			// видно, сколько кандидатов и подхватился ли SKU-якорь.
			s.log.Info("yandex market: out of stock, last price from state",
				"url", url, "sku", ymExtractSKU(url), "last_price", res.Price,
				"price_candidates", len(ymStatePriceRe.FindAllStringIndex(string(body), -1)))
		}
		return res, nil
	}

	// Товар не распарсился — различаем блок антибота и «нет данных».
	if isYandexCaptcha(body) {
		s.log.Warn("yandex market: SmartCaptcha block",
			"status", status, "source", source, "url", url, "body", snippet(body, 200))
		return nil, ErrMarketplaceBlocked
	}
	if status != 200 {
		return nil, fmt.Errorf("yandex market status %d", status)
	}
	// Новый OOS-шаблон (редизайн лета-2026): страница «Нет в продаже» ВООБЩЕ без
	// JSON-LD Product, но со ссылкой на полную карточку (showOriginalKmEmptyOffer=1),
	// которая отдаёт старую вёрстку — Product без offers + стейт-цена. Один
	// повторный GET, и её разбирает существующий OOS-путь парсера. У выпиленных
	// карточек (второй вариант шаблона) ссылки нет — они остаются parse_error.
	if fullURL, ok := ymOOSFullCardURL(url, string(body)); ok {
		if err := s.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		st2, body2, src2, err2 := s.getWithFallback(ctx, fullURL, ymCardHeader(), 6<<20)
		if err2 == nil && st2 == 200 && !isYandexCaptcha(body2) {
			metrics.YandexPriceSource.WithLabelValues(src2).Inc()
			if res, perr := parseYandexMarketHTML(string(body2), url); perr == nil {
				s.log.Info("yandex market: OOS-страница разобрана через полную карточку",
					"url", url, "in_stock", res.InStock, "last_price", res.Price)
				return res, nil
			}
		}
	}
	// 200 + реальная страница, но цены не нашли: диагностика структуры (есть ли
	// JSON-LD, где лежит price) — чтобы поправить парсер под актуальную вёрстку.
	s.log.Warn("yandex market: product not parsed (no JSON-LD price?)",
		"url", url, "len", len(body),
		"ld_json", strings.Count(string(body), "application/ld+json"),
		"ld_types", ymLDTypes(string(body)),
		"price_ctx", ymPriceContext(body),
		"cur_ctx", ymCurrencyContext(body))
	// status="parse_error" (а не not_found): антибот пройден, но цены нет —
	// видно на дашборде как отдельный сигнал дрейфа вёрстки.
	return nil, ErrParseFailed
}

// getWithFallback — GET по схеме direct+proxy с общим jar. Основной путь — direct
// (датацентр-IP, прокси не тратим). Если direct упирается в SmartCaptcha, а прокси
// сконфигурён — один запрос через прокси: он проходит антибот, обновляет cookie в
// общем jar и отдаёт страницу. Возвращает источник ("direct"/"proxy") для метрики.
// Без прокси остаёмся на direct-ответе (выше распознаётся как blocked).
func (s *YandexMarketScraper) getWithFallback(ctx context.Context, url string, header fhttp.Header, bodyCap int64) (int, []byte, string, error) {
	status, body, err := s.do(ctx, s.direct, url, header, bodyCap)
	if err != nil {
		return 0, nil, "", err
	}
	if status != 404 && isYandexCaptcha(body) && s.proxy != nil {
		s.log.Info("yandex market: direct hit SmartCaptcha, retrying via proxy", "url", url)
		status, body, err = s.do(ctx, s.proxy, url, header, bodyCap)
		if err != nil {
			return 0, nil, "", err
		}
		return status, body, "proxy", nil
	}
	return status, body, "direct", nil
}

// do — низкоуровневый GET переданным клиентом с заданными заголовками и лимитом тела.
func (s *YandexMarketScraper) do(ctx context.Context, client tls_client.HttpClient, url string, header fhttp.Header, bodyCap int64) (int, []byte, error) {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header = header
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("yandex market request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyCap))
	return resp.StatusCode, body, nil
}

// ymCardHeader — браузерные заголовки + порядок под Chrome-профиль TLS для карточки.
func ymCardHeader() fhttp.Header {
	return fhttp.Header{
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"accept-language":           {"ru,en;q=0.9"},
		"sec-ch-ua":                 {`"Chromium";v="148", "Google Chrome";v="148", "Not.A/Brand";v="24"`},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {`"Linux"`},
		"sec-fetch-dest":            {"document"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-site":            {"none"},
		"sec-fetch-user":            {"?1"},
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"},
		fhttp.HeaderOrderKey: {
			"accept", "accept-language", "sec-ch-ua", "sec-ch-ua-mobile",
			"sec-ch-ua-platform", "sec-fetch-dest", "sec-fetch-mode",
			"sec-fetch-site", "sec-fetch-user", "upgrade-insecure-requests", "user-agent",
		},
	}
}

// ymCurrencyContext возвращает фрагмент вокруг первого вхождения кода валюты
// (RUR/RUB) — обычно рядом лежит реальное значение цены в embedded-стейте.
// Для диагностики, если JSON-LD не содержит цены и придётся парсить стейт.
func ymCurrencyContext(body []byte) string {
	s := string(body)
	for _, marker := range []string{"RUR", "RUB"} {
		if i := strings.Index(s, marker); i >= 0 {
			start := i - 90
			if start < 0 {
				start = 0
			}
			end := i + 40
			if end > len(s) {
				end = len(s)
			}
			return s[start:end]
		}
	}
	return ""
}

// isYandexCaptcha распознаёт страницу SmartCaptcha. Вызывается ТОЛЬКО когда товар
// не распарсился, и сперва отсекает реальную страницу приложения Я.Маркета
// (@marketfront / data-baobab-name="$page") — на ней слово "captcha" живёт в
// JS-бандле и не означает блок. Маркеры самой капчи — узкие.
func isYandexCaptcha(body []byte) bool {
	s := string(body)
	if strings.Contains(s, "@marketfront/") || strings.Contains(s, `data-baobab-name="$page"`) {
		return false
	}
	ls := strings.ToLower(s)
	return strings.Contains(ls, "smartcaptcha") ||
		strings.Contains(ls, "checkbox-captcha") ||
		strings.Contains(ls, "showcaptcha") ||
		strings.Contains(ls, "подтвердите, что запросы отправляли вы")
}

// ymPriceContext возвращает фрагмент вокруг первого вхождения "price" — для
// диагностики, где Я.Маркет прячет цену, если JSON-LD не сработал.
func ymPriceContext(body []byte) string {
	s := string(body)
	i := strings.Index(s, `"price"`)
	if i < 0 {
		return ""
	}
	end := i + 200
	if end > len(s) {
		end = len(s)
	}
	return s[i:end]
}

var ymJSONLDRe = regexp.MustCompile(`(?s)<script type="application/ld\+json"[^>]*>(.*?)</script>`)

func parseYandexMarketHTML(html, productURL string) (*Result, error) {
	matches := ymJSONLDRe.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return nil, ErrProductNotFound
	}

	// Запоминаем первый Product-узел (имя/картинка), даже если в нём нет цены:
	// у карточек модели с пустым buy-box (showOriginalKmEmptyOffer=1) JSON-LD
	// содержит Product без offers, а реальную цену продавца кладут в стейт
	// marketfront — подхватим её фолбэком ниже.
	var product *ymJSONLDProduct

	for _, m := range matches {
		// Блок JSON-LD бывает: объект Product, массив объектов, либо обёртка
		// {"@graph":[...]} — разбираем все варианты (ymJSONLDNodes).
		for _, node := range ymJSONLDNodes(strings.TrimSpace(m[1])) {
			if !node.isProduct() {
				continue
			}
			if product == nil {
				n := node
				product = &n
			}
			price := node.bestPrice()
			if price <= 0 {
				continue
			}
			name := strings.TrimSpace(node.Name)
			if name == "" {
				name = "Товар Я.Маркета"
			}
			return &Result{
				Name:     name,
				Price:    price,
				ImageURL: ymFirstImage(node.Image),
				InStock:  true,
			}, nil
		}
	}

	// Есть Product-узел, но в JSON-LD НЕТ offers.price → активного buy-box нет,
	// то есть товара НЕТ В НАЛИЧИИ. При этом в стейте marketfront может лежать
	// цена ("price":{"value":"128931","currency":"RUR"}) — это «последняя/
	// справочная» цена, НЕ признак наличия. Поэтому InStock=false, а Price несём
	// как last-known (0, если стейт-цены тоже нет). Наличие определяется
	// наличием offers, а не присутствием цены где-либо. Гейт на Product важен —
	// на странице поиска цены из стейта принадлежат чужим сниппетам.
	if product != nil {
		name := strings.TrimSpace(product.Name)
		if name == "" {
			name = "Товар Я.Маркета"
		}
		return &Result{
			Name:     name,
			ImageURL: ymFirstImage(product.Image),
			Price:    ymStatePrice(html, ymExtractSKU(productURL)), // последняя известная цена (0, если нет)
			InStock:  false,
		}, nil
	}

	return nil, ErrProductNotFound
}

// ymOOSFullCardURL — URL полной карточки для нового OOS-шаблона. Возвращает
// ok=false, если на странице нет ссылки showOriginalKmEmptyOffer (не тот шаблон
// или карточка выпилена насовсем) либо запрос уже был по полной карточке
// (защита от рекурсии повторного GET).
func ymOOSFullCardURL(productURL, body string) (string, bool) {
	const marker = "showOriginalKmEmptyOffer"
	if strings.Contains(productURL, marker) || !strings.Contains(body, marker) {
		return "", false
	}
	sep := "?"
	if strings.Contains(productURL, "?") {
		sep = "&"
	}
	return productURL + sep + marker + "=1", true
}

// ymStatePriceRe вытаскивает цену из стейта marketfront для карточек, где JSON-LD
// отдаёт Product без offers (карточка модели с пустым buy-box). Формат стейта:
// "price":{"value":"128931","currency":"RUR"}.
var ymStatePriceRe = regexp.MustCompile(`"price":\{"value":"(\d+(?:\.\d+)?)","currency":"(?:RUR|RUB)"`)

// ymStatePrice — «последняя известная» цена основного товара из стейта.
//
// На карточке таких сниппетов МНОГО (сам товар + рекомендации + аксессуары +
// предложения разных продавцов), и Яндекс тасует блоки между загрузками — поэтому
// «первое вхождение» давало скачущую цену (то 128k, то 50k). Привязываемся к SKU
// из URL: берём ценовой сниппет, ближайший к вхождению SKU в стейте (данные товара
// держат его цену рядом со своим id). Без SKU — фолбэк на первое вхождение.
func ymStatePrice(html, sku string) float64 {
	locs := ymStatePriceRe.FindAllStringSubmatchIndex(html, -1)
	if len(locs) == 0 {
		return 0
	}
	pick := locs[0] // фолбэк: первое вхождение
	if sku != "" {
		if skuIdxs := allIndexes(html, sku); len(skuIdxs) > 0 {
			bestDist := int(^uint(0) >> 1)
			for _, loc := range locs {
				for _, si := range skuIdxs {
					d := loc[0] - si
					if d < 0 {
						d = -d
					}
					if d < bestDist {
						bestDist = d
						pick = loc
					}
				}
			}
		}
	}
	price, err := parsePriceString(html[pick[2]:pick[3]])
	if err != nil {
		return 0
	}
	return price
}

// ymExtractSKU — SKU товара из URL карточки: последний числовой сегмент пути
// (market.yandex.ru/card/<slug>-15584/5193397317 → "5193397317";
// /product--<slug>/123 → "123"). Пусто, если не нашли.
func ymExtractSKU(productURL string) string {
	u, err := url.Parse(productURL)
	if err != nil {
		return ""
	}
	for _, seg := range reverseSplit(u.Path, "/") {
		if len(seg) >= 4 && isAllDigits(seg) {
			return seg
		}
	}
	return ""
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func reverseSplit(s, sep string) []string {
	parts := strings.Split(s, sep)
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return parts
}

// allIndexes — все позиции вхождений substr в s (для поиска ближайшей цены к SKU).
func allIndexes(s, substr string) []int {
	var idxs []int
	for off := 0; ; {
		i := strings.Index(s[off:], substr)
		if i < 0 {
			break
		}
		idxs = append(idxs, off+i)
		off += i + len(substr)
	}
	return idxs
}

// ymLDTypes перечисляет @type всех JSON-LD блоков — диагностика на случай, когда
// цену не нашли: видно, есть ли вообще Product-блок на странице.
func ymLDTypes(html string) string {
	var types []string
	for _, m := range ymJSONLDRe.FindAllStringSubmatch(html, -1) {
		for _, n := range ymJSONLDNodes(strings.TrimSpace(m[1])) {
			t := strings.Join(n.Type, "|")
			if t == "" {
				t = "?"
			}
			types = append(types, t)
		}
	}
	return strings.Join(types, ",")
}

// parsePriceString парсит цену из строки в float64.
// JSON-LD может отдавать "22002" или "22002.00".
func parsePriceString(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty price")
	}
	var price float64
	if _, err := fmt.Sscanf(s, "%f", &price); err != nil {
		return 0, err
	}
	if price <= 0 {
		return 0, fmt.Errorf("non-positive price: %v", price)
	}
	return price, nil
}

// ── JSON-LD structure ────────────────────────────────────────────────────────

// ymImage — image в JSON-LD бывает строкой ИЛИ массивом строк. Принимаем оба.
type ymImage []string

func (i *ymImage) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*i = ymImage{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*i = many
	return nil
}

func ymFirstImage(img ymImage) string {
	for _, u := range img {
		if u = strings.TrimSpace(u); u != "" {
			return u
		}
	}
	return ""
}

// ymNum принимает цену и строкой ("12990"/"12990.00"), и числом (12990) —
// Я.Маркет отдаёт по-разному в зависимости от вёрстки. Раньше числовая цена
// роняла Unmarshal всего блока → товар «не найден».
type ymNum string

func (n *ymNum) UnmarshalJSON(b []byte) error {
	*n = ymNum(strings.Trim(string(b), `"`))
	return nil
}

// ymType — @type строкой ИЛИ массивом строк.
type ymType []string

func (t *ymType) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*t = ymType{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*t = many
	return nil
}

type ymOffer struct {
	Price    ymNum `json:"price"`
	LowPrice ymNum `json:"lowPrice"` // AggregateOffer кладёт минимальную цену сюда
}

// ymOffers — offers объектом ИЛИ массилом офферов.
type ymOffers []ymOffer

func (o *ymOffers) UnmarshalJSON(b []byte) error {
	var one ymOffer
	if err := json.Unmarshal(b, &one); err == nil {
		*o = ymOffers{one}
		return nil
	}
	var many []ymOffer
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*o = many
	return nil
}

type ymJSONLDProduct struct {
	Type   ymType            `json:"@type"`
	Name   string            `json:"name"`
	Image  ymImage           `json:"image"`
	Offers ymOffers          `json:"offers"`
	Graph  []ymJSONLDProduct `json:"@graph"` // обёртка {"@graph":[...]}
}

func (p ymJSONLDProduct) isProduct() bool {
	for _, t := range p.Type {
		if strings.EqualFold(t, "Product") {
			return true
		}
	}
	return false
}

func (p ymJSONLDProduct) bestPrice() float64 {
	for _, o := range p.Offers {
		if v, err := parsePriceString(string(o.Price)); err == nil && v > 0 {
			return v
		}
		if v, err := parsePriceString(string(o.LowPrice)); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// ymJSONLDNodes разбирает блок JSON-LD в список узлов: объект, массив объектов
// или обёртку {"@graph":[...]}.
func ymJSONLDNodes(raw string) []ymJSONLDProduct {
	if strings.HasPrefix(raw, "[") {
		var arr []ymJSONLDProduct
		if json.Unmarshal([]byte(raw), &arr) == nil {
			return arr
		}
		return nil
	}
	var one ymJSONLDProduct
	if json.Unmarshal([]byte(raw), &one) != nil {
		return nil
	}
	if len(one.Graph) > 0 {
		return append([]ymJSONLDProduct{one}, one.Graph...)
	}
	return []ymJSONLDProduct{one}
}
