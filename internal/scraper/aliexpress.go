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
)

// AliexpressScraper получает цену/название/картинку товара aliexpress.ru по URL
// карточки через внутренний JSON-API (aer-jsonapi productData) — БЕЗ браузера.
//
// Почему так. Карточка aliexpress.ru — CSR: в HTML цены нет, данные грузит
// fetch к /aer-jsonapi/v1/bx/pdp/web/productData. Сайт прикрыт антиботом X5SEC,
// но решает РЕПУТАЦИЯ IP, а не headless-сигналы: с датацентрового egress —
// «punish»-капча, с RU-резидентского/мобильного прокси API отдаётся напрямую
// (проверено). JA3 Chrome (tls-client) + RU-прокси достаточно — браузер
// (Camoufox-сайдкар) оказался не нужен, в отличие от Ozon с FAB.
//
// Сессия. Первый запрос к productData отвечает 302 на cookie-sync
// (login.aliexpress.ru/.../aer-cookie/set-directly?cookie_sync_parameters=...),
// который ставит сессионные aer-cookie и редиректит обратно на API → 200 JSON.
// tls-client следует редиректам и держит cookie в jar, поэтому handshake
// одноразовый: он амортизируется на последующих товарах (jar переиспользуется).
type AliexpressScraper struct {
	client     tls_client.HttpClient // nil → не сконфигурён (только Matches)
	limiter    *rate.Limiter
	log        *slog.Logger
	configured bool
}

// AliexpressOptions — конфигурация скрейпера. Нулевое значение даёт «облегчённый»
// скрейпер: Matches работает (нужно боту/api для разбора URL), а Scrape вернёт
// ErrNotImplemented. Реальный скрейп включается, ТОЛЬКО когда задан ProxyURL:
// с датацентрового IP X5SEC отдаёт капчу, поэтому без RU-прокси смысла нет.
type AliexpressOptions struct {
	ProxyURL string  // http://user:pass@host:port RU-резидентского/мобильного прокси (ОБЯЗАТЕЛЕН)
	RPS      float64 // лимит запросов к Ali (один IP → держим низким), 0 → 1
	Logger   *slog.Logger
}

func NewAliexpressScraper(opts AliexpressOptions) *AliexpressScraper {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	rps := opts.RPS
	if rps <= 0 {
		rps = 1
	}

	s := &AliexpressScraper{
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
		log:     log,
	}

	if opts.ProxyURL == "" {
		// Без RU-прокси X5SEC завернёт датацентровый egress → не поднимаем клиент,
		// Scrape отдаст ErrNotImplemented (бот покажет «скоро будет»).
		log.Warn("aliexpress: ALI_PROXY_URL пуст — скрейпер disabled (нужен RU-резидентский/мобильный прокси)")
		return s
	}
	client, err := newAliexpressTLSClient(opts.ProxyURL)
	if err != nil {
		log.Error("aliexpress: tls-client init failed, scraper disabled", "err", err)
		return s
	}
	s.client = client
	s.configured = true
	log.Info("aliexpress scraper configured", "proxy", true)
	return s
}

// newAliexpressTLSClient — Chrome-профиль TLS (JA3 проходит X5SEC на чистом IP),
// cookie jar (нужен для cookie-sync handshake), RU-прокси. Редиректы tls-client
// следует по умолчанию — это и проводит handshake. В jar заранее кладём locale-
// cookie aep_usuc_f (рынок RU / валюта RUB), как делает сайт.
func newAliexpressTLSClient(proxyURL string) (tls_client.HttpClient, error) {
	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(25),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(jar),
		tls_client.WithProxyUrl(proxyURL),
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(aliBaseURL)
	client.SetCookies(u, []*fhttp.Cookie{{
		Name:   "aep_usuc_f",
		Value:  "site=rus&c_tp=RUB&region=RU&b_locale=ru_RU",
		Domain: ".aliexpress.ru",
		Path:   "/",
	}})
	return client, nil
}

func (s *AliexpressScraper) Marketplace() Marketplace { return MarketplaceAliexpress }

func (s *AliexpressScraper) Matches(url string) bool {
	return strings.Contains(url, "aliexpress.ru/item/") ||
		strings.Contains(url, "aliexpress.com/item/")
}

const aliBaseURL = "https://aliexpress.ru"

var aliItemRe = regexp.MustCompile(`/item/(\d+)\.html`)

// ExtractAliexpressID — артикул товара из URL карточки (/item/<id>.html).
func ExtractAliexpressID(productURL string) (string, error) {
	m := aliItemRe.FindStringSubmatch(productURL)
	if len(m) < 2 {
		return "", fmt.Errorf("%w: not an aliexpress product URL", ErrInvalidURL)
	}
	return m[1], nil
}

func (s *AliexpressScraper) Scrape(ctx context.Context, productURL string) (*Result, error) {
	if !s.configured {
		return nil, fmt.Errorf("%w: aliexpress scraper not configured (no proxy)", ErrNotImplemented)
	}
	id, err := ExtractAliexpressID(productURL)
	if err != nil {
		return nil, err
	}
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	itemURL := aliBaseURL + "/item/" + id + ".html"

	status, body, err := s.fetchProductData(ctx, id, itemURL)
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, ErrProductNotFound
	}
	// Сессия не установлена (cookie-sync не сошёлся) → прогреваем item-страницу
	// (она ставит сессионные aer-cookie и проходит X5SEC), повторяем API один раз.
	// Прогрев одноразовый: jar держит cookie, дальше товары идут прямо в API.
	if status != 200 || isAliBlocked(body) {
		if werr := s.warmSession(ctx, itemURL); werr != nil {
			s.log.Warn("aliexpress: warm session failed", "url", productURL, "err", werr)
		}
		status, body, err = s.fetchProductData(ctx, id, itemURL)
		if err != nil {
			return nil, err
		}
	}
	if status == 404 {
		return nil, ErrProductNotFound
	}
	if isAliBlocked(body) {
		s.log.Warn("aliexpress: X5SEC block / cookie-sync not resolved",
			"status", status, "url", productURL, "body", snippet(body, 180))
		return nil, ErrMarketplaceBlocked
	}
	if status != 200 {
		return nil, fmt.Errorf("aliexpress status %d", status)
	}
	return parseAliexpressProductData(body, productURL)
}

// fetchProductData дёргает aer-jsonapi productData. sourceId=0 — как у веб-фронта.
// Параметр sk (токен страницы) НЕ обязателен: API сам проводит cookie-sync
// редиректом, tls-client его следует, а jar держит сессию.
func (s *AliexpressScraper) fetchProductData(ctx context.Context, id, itemURL string) (int, []byte, error) {
	apiURL := aliBaseURL + "/aer-jsonapi/v1/bx/pdp/web/productData?productId=" + id + "&sourceId=0"
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, apiURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header = fhttp.Header{
		"accept":           {"application/json, text/plain, */*"},
		"accept-language":  {"ru,en;q=0.9"},
		"referer":          {itemURL},
		"x-requested-with": {"XMLHttpRequest"},
		"sec-ch-ua":        {`"Chromium";v="146", "Google Chrome";v="146", "Not.A/Brand";v="24"`},
		"sec-ch-ua-mobile": {"?0"},
		"sec-fetch-dest":   {"empty"},
		"sec-fetch-mode":   {"cors"},
		"sec-fetch-site":   {"same-origin"},
		"user-agent":       {aliUserAgent},
		fhttp.HeaderOrderKey: {
			"accept", "accept-language", "referer", "x-requested-with",
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-fetch-dest", "sec-fetch-mode",
			"sec-fetch-site", "user-agent",
		},
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("aliexpress request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, nil
}

// warmSession открывает страницу товара как браузер: проходит X5SEC (на хорошем
// IP — без капчи) и набирает сессионные aer-cookie в jar, чтобы следующий
// API-запрос прошёл cookie-sync. Тело не нужно — важны cookie.
func (s *AliexpressScraper) warmSession(ctx context.Context, itemURL string) error {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, itemURL, nil)
	if err != nil {
		return err
	}
	req.Header = fhttp.Header{
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"accept-language":           {"ru,en;q=0.9"},
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {aliUserAgent},
		fhttp.HeaderOrderKey: {
			"accept", "accept-language", "upgrade-insecure-requests", "user-agent",
		},
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

const aliUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

func isAliBlocked(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "x5secdata") ||
		strings.Contains(s, "_____tmd_____") ||
		strings.Contains(s, "aer-cookie/set-directly")
}

// ── Парсинг productData ──────────────────────────────────────────────────────

type aliAmount struct {
	Value    float64 `json:"value"`
	Currency string  `json:"currency"`
}

type aliProductData struct {
	Data struct {
		Name    string `json:"name"`
		Gallery []struct {
			ImageURL string `json:"imageUrl"`
		} `json:"gallery"`
		Price struct {
			// activity = промо/итоговая цена (что платит покупатель); amount =
			// зачёркнутая. Для вариативных товаров — диапазон min/max, трекаем min.
			MinActivityAmount *aliAmount `json:"minActivityAmount"`
			MinAmount         *aliAmount `json:"minAmount"`
		} `json:"price"`
		// Наличие: непустой блок → товар (или предвыбранный SKU) не в продаже.
		PreselectSkuOutOfStockInfo json.RawMessage `json:"preselectSkuOutOfStockInfo"`
		Analytics                  struct {
			ViewProduct struct {
				TrackingInfo struct {
					Available  *bool    `json:"available"`
					FinalPrice *float64 `json:"finalPrice"`
				} `json:"trackingInfo"`
			} `json:"viewProduct"`
		} `json:"analytics"`
	} `json:"data"`
}

func parseAliexpressProductData(body []byte, productURL string) (*Result, error) {
	var pd aliProductData
	if err := json.Unmarshal(body, &pd); err != nil {
		return nil, fmt.Errorf("%w: aliexpress json: %v", ErrParseFailed, err)
	}
	d := pd.Data

	// Цена: активная (что платит покупатель) → фолбэк зачёркнутая → фолбэк
	// finalPrice из аналитики.
	var price float64
	switch {
	case d.Price.MinActivityAmount != nil && d.Price.MinActivityAmount.Value > 0:
		price = d.Price.MinActivityAmount.Value
	case d.Price.MinAmount != nil && d.Price.MinAmount.Value > 0:
		price = d.Price.MinAmount.Value
	case d.Analytics.ViewProduct.TrackingInfo.FinalPrice != nil:
		price = *d.Analytics.ViewProduct.TrackingInfo.FinalPrice
	}

	// Наличие: явный флаг аналитики приоритетнее; иначе — по OOS-блоку.
	inStock := true
	if a := d.Analytics.ViewProduct.TrackingInfo.Available; a != nil {
		inStock = *a
	} else if len(d.PreselectSkuOutOfStockInfo) > 0 && string(d.PreselectSkuOutOfStockInfo) != "null" {
		inStock = false
	}

	if price <= 0 {
		// 200 + валидный JSON, но цены нет: либо OOS без last-known, либо дрейф
		// схемы. Отдаём parse_error (отдельный сигнал на дашборде).
		if !inStock {
			return &Result{Name: aliName(d.Name), Price: 0, ImageURL: aliFirstImage(d.Gallery), InStock: false}, nil
		}
		return nil, ErrParseFailed
	}

	return &Result{
		Name:     aliName(d.Name),
		Price:    price,
		ImageURL: aliFirstImage(d.Gallery),
		InStock:  inStock,
	}, nil
}

func aliName(name string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	return "Товар AliExpress"
}

func aliFirstImage(gallery []struct {
	ImageURL string `json:"imageUrl"`
}) string {
	if len(gallery) > 0 {
		return gallery[0].ImageURL
	}
	return ""
}
