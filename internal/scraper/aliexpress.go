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

// AliexpressScraper получает цену/название/картинку товара aliexpress.ru по URL
// карточки через внутренний JSON-API (aer-jsonapi productData) — БЕЗ браузера.
//
// Почему так. Карточка aliexpress.ru — CSR: в HTML цены нет, данные грузит
// fetch к /aer-jsonapi/v1/bx/pdp/web/productData. Сайт прикрыт антиботом X5SEC,
// но для JSON-API решает не IP-репутация на каждом запросе, а наличие сессионной
// aer-cookie: достаточно ОДИН раз получить её через RU-прокси, и дальше API
// отдаётся НАПРЯМУЮ с датацентр-IP (проверено). Браузер (Camoufox-сайдкар) не
// нужен, в отличие от Ozon с FAB.
//
// Экономия прокси. Держим два клиента с ОБЩИМ cookie-jar:
//   - direct (без прокси) — основной путь, дешёвый, ходит с датацентр-IP;
//   - proxy (через RU-прокси) — только когда сессия cold/протухла: один запрос
//     через прокси проходит X5SEC, обновляет aer-cookie в общем jar и попутно
//     отдаёт данные. Дальше снова direct, пока cookie живы.
// Первый запрос к productData отвечает 302 на cookie-sync (set-directly),
// tls-client следует редиректам и кладёт cookie в jar — handshake прозрачен.
type AliexpressScraper struct {
	direct     tls_client.HttpClient // без прокси — основной путь (датацентр-IP)
	proxy      tls_client.HttpClient // через RU-прокси — рефреш cookie (общий jar с direct)
	limiter    *rate.Limiter
	log        *slog.Logger
	configured bool
}

// AliexpressOptions — конфигурация скрейпера. Пустой ProxyURL даёт direct-only
// режим (после отмены мобильного прокси 2026-07-04): основной путь и так direct,
// прокси был только фолбэком на рефреш cookie. Если X5SEC зарубит cold-сессию
// с датацентра, Scrape вернёт ErrMarketplaceBlocked (виден в метриках) — тогда
// задать ALI_PROXY_URL (дешёвый резидентский по ГБ, трафик рефреша копеечный).
// «Облегчённый» скрейпер только под Matches — нулевой &AliexpressScraper{}.
type AliexpressOptions struct {
	ProxyURL string  // (опц.) http://user:pass@host:port RU-прокси — фолбэк для рефреша cookie
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

	direct, proxy, err := newAliexpressClients(opts.ProxyURL)
	if err != nil {
		log.Error("aliexpress: tls-client init failed, scraper disabled", "err", err)
		return s
	}
	s.direct = direct
	s.proxy = proxy
	s.configured = true
	transport := "direct+proxy-refresh"
	if proxy == nil {
		transport = "direct-only"
	}
	log.Info("aliexpress scraper configured", "transport", transport)
	return s
}

// newAliexpressClients строит tls-client'ы (Chrome-JA3) с ОБЩИМ cookie-jar:
// direct (без прокси) и — если задан proxyURL — proxy (RU-прокси для рефреша,
// иначе proxy=nil → direct-only). Общий jar — ключ схемы: cookie, добытые
// proxy-клиентом, сразу видны direct-клиенту. Редиректы tls-client следует
// по умолчанию (это проводит cookie-sync). В jar заранее кладём locale-cookie
// aep_usuc_f (рынок RU / валюта RUB), как делает сайт.
func newAliexpressClients(proxyURL string) (direct, proxy tls_client.HttpClient, err error) {
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
	u, _ := url.Parse(aliBaseURL)
	direct.SetCookies(u, []*fhttp.Cookie{{
		Name:   "aep_usuc_f",
		Value:  "site=rus&c_tp=RUB&region=RU&b_locale=ru_RU",
		Domain: ".aliexpress.ru",
		Path:   "/",
	}})
	return direct, proxy, nil
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
		return nil, fmt.Errorf("%w: aliexpress scraper not configured (lightweight instance or tls-client init failed)", ErrNotImplemented)
	}
	id, err := ExtractAliexpressID(productURL)
	if err != nil {
		return nil, err
	}
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	itemURL := aliBaseURL + "/item/" + id + ".html"

	// Основной путь — direct (без прокси, датацентр-IP): дёшево, по живой cookie.
	status, body, err := s.fetchProductData(ctx, s.direct, id, itemURL)
	if err != nil {
		return nil, err
	}
	source := "direct"
	// Сессия cold/протухла (cookie-sync не сошёлся / X5SEC) → один запрос через
	// прокси: он проходит X5SEC, обновляет aer-cookie в ОБЩЕМ jar и сразу отдаёт
	// данные. Дальше снова direct, пока cookie живы. Прокси платим только тут.
	// В direct-only режиме (proxy=nil) фолбэка нет — блок дойдёт до
	// ErrMarketplaceBlocked ниже и будет виден в метриках.
	if (status != 200 || isAliBlocked(body)) && s.proxy != nil {
		source = "proxy"
		status, body, err = s.fetchProductData(ctx, s.proxy, id, itemURL)
		if err != nil {
			return nil, err
		}
	}
	metrics.AliPriceSource.WithLabelValues(source).Inc()

	if status == 404 {
		return nil, ErrProductNotFound
	}
	if isAliBlocked(body) {
		// Даже через прокси не пробились: капча на cold-сессии (плохой IP прокси) —
		// отдельный сигнал, чтобы отличать от «нет данных».
		s.log.Warn("aliexpress: X5SEC block / cookie-sync not resolved",
			"status", status, "url", productURL, "body", snippet(body, 180))
		return nil, ErrMarketplaceBlocked
	}
	if status != 200 {
		return nil, fmt.Errorf("aliexpress status %d", status)
	}
	return parseAliexpressProductData(body, productURL)
}

// fetchProductData дёргает aer-jsonapi productData через переданный клиент
// (direct или proxy). sourceId=0 — как у веб-фронта. Параметр sk (токен страницы)
// НЕ обязателен: API сам проводит cookie-sync редиректом, tls-client его следует,
// а общий jar держит сессию.
func (s *AliexpressScraper) fetchProductData(ctx context.Context, client tls_client.HttpClient, id, itemURL string) (int, []byte, error) {
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
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("aliexpress request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, nil
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
