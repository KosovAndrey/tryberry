package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/time/rate"
)

// YandexMarketScraper получает цену/название/картинку товара Я.Маркета по URL
// карточки — БЕЗ аккаунта (цена публичная, в отличие от Ozon).
//
// Почему так, а не чистый net/http: market.yandex.ru закрыт SmartCaptcha, который
// на «голом» Go-TLS-отпечатке + датацентровом IP отдаёт страницу капчи вместо
// карточки (из-за этого старую реализацию пришлось выключить, коммит 86b1712).
// Рабочий рецепт — тот же, что вытащил Ozon: хороший TLS-отпечаток
// (bogdanfinn/tls-client, профиль Chrome) + RU-мобильный/резидентский прокси.
// Аккаунт не нужен — мы берём публичную карточку и парсим встроенный JSON-LD
// (<script type="application/ld+json"> → @type:Product → offers.price), как и
// раньше; меняется только транспорт.
type YandexMarketScraper struct {
	client     tls_client.HttpClient // nil → не сконфигурён (только Matches)
	limiter    *rate.Limiter
	log        *slog.Logger
	configured bool
}

// YandexMarketOptions — конфигурация скрейпера. Нулевое значение даёт «облегчённый»
// скрейпер: Matches работает (нужно боту/api для разбора URL), а Scrape вернёт
// ErrNotImplemented. Реальный скрейп включается всегда, когда удаётся поднять
// tls-client (аккаунт не требуется); ProxyURL опционален, но без RU-прокси
// SmartCaptcha почти наверняка зарежет.
type YandexMarketOptions struct {
	ProxyURL string  // http://user:pass@host:port RU-мобильного/резидентского прокси
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

	client, err := newYandexTLSClient(opts.ProxyURL)
	if err != nil {
		log.Error("yandex market: tls-client init failed, scraper disabled", "err", err)
		return s
	}
	s.client = client
	s.configured = true
	log.Info("yandex market scraper configured", "proxy", opts.ProxyURL != "")
	return s
}

// newYandexTLSClient — Chrome-профиль TLS (карточку отдаёт web), таймаут с запасом
// под тяжёлый HTML (~2.5 МБ) и редиректы. Прокси опционален.
func newYandexTLSClient(proxyURL string) (tls_client.HttpClient, error) {
	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(25),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(jar),
	}
	if proxyURL != "" {
		options = append(options, tls_client.WithProxyUrl(proxyURL))
	}
	return tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
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

	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Браузерные заголовки + порядок — под Chrome-профиль TLS (см. ozon web-ветку).
	req.Header = fhttp.Header{
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

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("yandex market request: %w", err)
	}
	defer resp.Body.Close()
	// HTML карточки тяжёлый (~2.5 МБ) — ограничиваем разумным потолком.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 6<<20))

	if resp.StatusCode == 404 {
		return nil, ErrProductNotFound
	}

	// Парсим В ПЕРВУЮ ОЧЕРЕДЬ. SmartCaptcha отдаёт капчу со статусом 200, а на
	// настоящей странице товара в JS-бандле всё равно встречается слово "captcha"
	// — поэтому блок/успех решаем ПО СОДЕРЖИМОМУ (достали ли товар), а не по
	// наличию подстроки (раньше это давало ложный «blocked» на живой странице).
	if res, perr := parseYandexMarketHTML(string(body)); perr == nil {
		return res, nil
	}

	// Товар не распарсился — различаем блок антибота и «нет данных».
	if isYandexCaptcha(body) {
		s.log.Warn("yandex market: SmartCaptcha block",
			"status", resp.StatusCode, "url", url, "body", snippet(body, 200))
		return nil, ErrMarketplaceBlocked
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("yandex market status %d", resp.StatusCode)
	}
	// 200 + реальная страница, но цены не нашли: диагностика структуры (есть ли
	// JSON-LD, где лежит price) — чтобы поправить парсер под актуальную вёрстку.
	s.log.Warn("yandex market: product not parsed (no JSON-LD price?)",
		"url", url, "len", len(body),
		"ld_json", strings.Count(string(body), "application/ld+json"),
		"ld_types", ymLDTypes(string(body)),
		"price_ctx", ymPriceContext(body),
		"cur_ctx", ymCurrencyContext(body))
	return nil, ErrProductNotFound
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

func parseYandexMarketHTML(html string) (*Result, error) {
	matches := ymJSONLDRe.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return nil, ErrProductNotFound
	}

	for _, m := range matches {
		// Блок JSON-LD бывает: объект Product, массив объектов, либо обёртка
		// {"@graph":[...]} — разбираем все варианты (ymJSONLDNodes).
		for _, node := range ymJSONLDNodes(strings.TrimSpace(m[1])) {
			if !node.isProduct() {
				continue
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
			}, nil
		}
	}

	return nil, ErrProductNotFound
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
