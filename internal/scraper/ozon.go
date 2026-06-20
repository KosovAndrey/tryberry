package scraper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/time/rate"
)

// Режимы запроса к Ozon. mobile = api.ozon.ru/composer-api.bx + okhttp-TLS +
// заголовки приложения (рецепт, дававший живой 200); web = www.ozon.ru/
// entrypoint-api.bx + Chrome-TLS (на нём FAB отдавал fab_cp_ даже с валидным ETC).
const (
	ozonModeMobile = "mobile"
	ozonModeWeb    = "web"
	// browser = браузер-как-транспорт: запрос к storefront-API делает живой
	// залогиненный Chromium (сайдкар ozon-miner) через мобильный прокси —
	// он проходит FAB и сам рефрешит токен. Go только зовёт сайдкар по HTTP и
	// парсит тот же widgetStates. Масштабируется пулом дорожек на стороне сайдкара.
	ozonModeBrowser = "browser"

	// Заголовки мобильного приложения (из снятого живого 200-запроса).
	ozonAppUA   = "ozonapp_android/19.20.0+2684"
	ozonAppName = "ozonapp_android"
	ozonAppVer  = "19.20.0(2684)"
	// x-o3-fp — статическая константа приложения (не device-fp), см. research.
	ozonStaticFP = "1.01ae145142fa31f9"
)

// OzonScraper получает цену/название/картинку товара Ozon через storefront-API
// (composer-api.bx → widgetStates), под залогиненным аккаунтом (путь B).
//
// Почему так, а не чистый Go-запрос: эндпоинт закрыт антиботом FAB, который на
// первом контакте отдаёт обфусцированный JS-VM challenge (чистым Go не пройти) и
// режет по TLS-отпечатку + репутации IP. Рабочая схема: cookie залогиненной
// аккаунт-сессии (снятая из приложения, OZON_COOKIE) + okhttp-TLS
// (bogdanfinn/tls-client, профиль Okhttp4Android13) + RU-мобильный прокси →
// доверенная сессия проходит FAB. Браузер не нужен вообще.
type OzonScraper struct {
	client     tls_client.HttpClient // nil → browser-режим или только Matches
	limiter    *rate.Limiter
	log        *slog.Logger
	mode       string // ozonModeMobile | ozonModeWeb | ozonModeBrowser
	configured bool

	// cookie аккаунт-сессии (путь B, mobile/web). "" → не сконфигурён (только Matches).
	accountCookie string

	// browser-режим: базовый URL сайдкара ozon-miner (http://ozon-miner:PORT) и
	// http-клиент для походов к нему. Сам сайдкар держит пул залогиненных дорожек.
	browserURL    string
	browserClient *http.Client
}

// OzonOptions — конфигурация рабочего скрейпера. Нулевое значение даёт
// «облегчённый» скрейпер: Matches работает (нужно боту/api для разбора URL),
// а Scrape вернёт ErrNotImplemented. Реальный скрейп включается, когда заданы
// Cookie (или AccessToken) и ProxyURL (RU-мобильный прокси).
type OzonOptions struct {
	ProxyURL string  // http://user:pass@host:port мобильного прокси
	RPS      float64 // лимит запросов к Ozon (один IP → держим низким), 0 → 1
	Mode     string  // "mobile" (по умолчанию) | "web"
	Logger   *slog.Logger

	// Путь B (аккаунт-токены): если задан AccessToken — скрейпер ходит под
	// залогиненным аккаунтом (cookie __Secure-access-token/__Secure-refresh-token).
	// Доверенная сессия проходит FAB. Секреты — из .env.
	AccessToken  string
	RefreshToken string
	// Cookie — готовая cookie-строка целиком (снятая из запроса приложения через
	// HTTP Toolkit): самый надёжный вариант, несёт все куки аккаунт-сессии
	// (access/refresh-token, __Secure-user-id, abt_data). Приоритетнее токенов.
	Cookie string

	// BrowserURL — базовый URL сайдкара ozon-miner (например http://ozon-miner:8080).
	// Задан вместе с Mode="browser" → скрейпер ходит через живой браузер-пул, а не
	// tls-client. Cookie/прокси в этом режиме не нужны (их держат дорожки сайдкара).
	BrowserURL string
}

func NewOzonScraper(opts OzonOptions) *OzonScraper {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	rps := opts.RPS
	if rps <= 0 {
		rps = 1
	}
	mode := opts.Mode
	if mode != ozonModeWeb && mode != ozonModeBrowser {
		mode = ozonModeMobile
	}

	s := &OzonScraper{
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
		log:     log,
		mode:    mode,
	}

	// browser-режим: транспорт — сайдкар ozon-miner (пул залогиненных дорожек).
	// Не нужны ни tls-client, ни cookie/прокси — всё это держат дорожки сайдкара.
	if mode == ozonModeBrowser {
		if opts.BrowserURL == "" {
			log.Error("ozon: mode=browser, но OZON_BROWSER_URL пуст — скрейпер disabled")
			return s
		}
		s.browserURL = strings.TrimRight(opts.BrowserURL, "/")
		// Таймаут с запасом: дорожка может прогревать сессию/решать FAB на холодном старте.
		s.browserClient = &http.Client{Timeout: 60 * time.Second}
		s.configured = true
		log.Info("ozon scraper configured", "mode", mode, "transport", "browser-pool",
			"sidecar", s.browserURL)
		return s
	}

	// Путь B: готовая cookie-строка или аккаунт-токены → ходим под залогиненной
	// сессией (mobile/okhttp).
	if opts.Cookie != "" {
		s.accountCookie = opts.Cookie // полная cookie-строка из приложения
	} else if opts.AccessToken != "" {
		s.accountCookie = "__Secure-access-token=" + opts.AccessToken
		if opts.RefreshToken != "" {
			s.accountCookie += "; __Secure-refresh-token=" + opts.RefreshToken
		}
	}

	// Без аккаунт-cookie скрейпер не сконфигурён (только Matches): аноним FAB не проходит.
	if s.accountCookie != "" {
		s.mode = ozonModeMobile // аккаунт-API живёт на composer-api.bx (okhttp)
		mode = ozonModeMobile
		client, err := newOzonTLSClient(opts.ProxyURL, mode)
		if err != nil {
			log.Error("ozon: tls-client init failed, scraper disabled", "err", err)
		} else {
			s.client = client
			s.configured = true
			// длину cookie логируем (НЕ значение) — подтвердить, что .env подхватился.
			log.Info("ozon scraper configured", "mode", mode, "auth", "account-token",
				"cookie_len", len(s.accountCookie), "proxy", opts.ProxyURL != "")
		}
	}
	return s
}

func newOzonTLSClient(proxyURL, mode string) (tls_client.HttpClient, error) {
	// TLS-профиль обязан совпадать с клиентом, под которого заточен эндпоинт:
	// mobile → okhttp (как приложение); web → Chrome (как Chromium майнера).
	profile := profiles.Okhttp4Android13
	if mode == ozonModeWeb {
		profile = profiles.Chrome_146
	}
	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(20),
		tls_client.WithClientProfile(profile),
		tls_client.WithCookieJar(jar),
	}
	if proxyURL != "" {
		options = append(options, tls_client.WithProxyUrl(proxyURL))
	}
	return tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
}

func (s *OzonScraper) Marketplace() Marketplace { return MarketplaceOzon }

func (s *OzonScraper) Matches(url string) bool {
	return strings.Contains(url, "ozon.ru/product/")
}

var ozonProductRe = regexp.MustCompile(`ozon\.ru/product/(?:[^/?#]*-)?(\d+)`)

// extractOzonID достаёт числовой id товара из URL вида
// https://www.ozon.ru/product/nazvanie-tovara-123456789/
func extractOzonID(url string) (string, error) {
	m := ozonProductRe.FindStringSubmatch(url)
	if len(m) < 2 {
		return "", fmt.Errorf("%w: not an ozon product URL", ErrInvalidURL)
	}
	return m[1], nil
}

func (s *OzonScraper) Scrape(ctx context.Context, url string) (*Result, error) {
	if !s.configured {
		return nil, fmt.Errorf("%w: ozon scraper not configured (no account cookie)", ErrNotImplemented)
	}
	id, err := extractOzonID(url)
	if err != nil {
		return nil, err
	}
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	statusCode, body, err := s.fetch(ctx, id)
	if err != nil {
		return nil, err
	}

	// FAB-блок: incident + тело подскажут причину (fab_chlg_ = сессия/токен не
	// признаны; иное = признаны, но запрос режут — обычно репутация IP). current_ip
	// показывает, какой egress прокси заблокирован.
	if statusCode == 403 || bytesHasFAB(body) {
		incident := fabIncidentRe.FindString(string(body))
		s.log.Warn("ozon: FAB block",
			"status", statusCode, "id", id, "mode", s.mode,
			"current_ip", s.currentEgressIP(ctx),
			"incident", incident, "body", snippet(body, 300))
		return nil, ErrMarketplaceBlocked
	}
	if statusCode == 401 {
		s.log.Warn("ozon: auth expired (401) — нужен свежий cookie/рефреш токена", "id", id)
		return nil, ErrAuthExpired
	}
	if statusCode == 404 {
		return nil, ErrProductNotFound
	}
	if statusCode != 200 {
		return nil, fmt.Errorf("ozon status %d", statusCode)
	}

	res, err := parseOzonWidgets(body)
	if err != nil {
		return nil, err
	}
	if res.ImageURL == "" {
		names, gallery := ozonImageDiag(body)
		s.log.Warn("ozon: image not found in widgetStates",
			"name", res.Name, "widgets", names, "gallery", snippet([]byte(gallery), 600))
	}
	return res, nil
}

// fetch выполняет запрос к storefront-API и возвращает (HTTP-статус, тело). Ветка
// по режиму: browser → через сайдкар-пул (живой Chromium), иначе → tls-client
// (mobile/web). Дальше Scrape единообразно разбирает статус/тело независимо от
// транспорта.
func (s *OzonScraper) fetch(ctx context.Context, id string) (int, []byte, error) {
	if s.mode == ozonModeBrowser {
		return s.fetchViaBrowser(ctx, id)
	}
	return s.fetchViaTLS(ctx, id)
}

// fetchViaTLS — путь B (mobile/web): tls-client под аккаунт-cookie через мобильный
// прокси.
func (s *OzonScraper) fetchViaTLS(ctx context.Context, id string) (int, []byte, error) {
	req, err := s.buildRequest(ctx, id, s.accountCookie, ozonAppUA)
	if err != nil {
		return 0, nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("ozon request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, nil
}

// fetchViaBrowser — browser-режим: просим сайдкар ozon-miner сделать запрос к
// storefront-API из живой залогиненной сессии (она проходит FAB и сама рефрешит
// токен). Сайдкар возвращает сырое тело widgetStates и зеркалит upstream-статус
// (включая 403 при FAB), поэтому дальнейшая обработка в Scrape — общая.
func (s *OzonScraper) fetchViaBrowser(ctx context.Context, id string) (int, []byte, error) {
	api := s.browserURL + "/scrape?id=" + url.QueryEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := s.browserClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("ozon browser sidecar: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	// 502/504 от сайдкара = его внутренняя беда (нет живых дорожек/таймаут), не FAB.
	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout {
		return 0, nil, fmt.Errorf("ozon browser sidecar unavailable: status %d: %s",
			resp.StatusCode, snippet(body, 200))
	}
	return resp.StatusCode, body, nil
}

// buildRequest собирает запрос к storefront-API по режиму:
//
//	mobile → api.ozon.ru/composer-api.bx + заголовки приложения (okhttp-TLS)
//	web    → www.ozon.ru/entrypoint-api.bx + браузерные заголовки (Chrome-TLS)
//
// В обоих ответ — один формат widgetStates. ua — UA из слота (Chrome), для
// mobile он не используется (там UA приложения).
func (s *OzonScraper) buildRequest(ctx context.Context, id, cookie, ua string) (*fhttp.Request, error) {
	if s.mode == ozonModeWeb {
		api := "https://www.ozon.ru/api/entrypoint-api.bx/page/json/v2?url=/product/" + id + "/"
		req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, api, nil)
		if err != nil {
			return nil, err
		}
		req.Header = fhttp.Header{
			"accept":             {"application/json"},
			"accept-language":    {"ru,en;q=0.9"},
			"sec-ch-ua":          {`"Chromium";v="148", "Google Chrome";v="148", "Not.A/Brand";v="24"`},
			"sec-ch-ua-mobile":   {"?0"},
			"sec-ch-ua-platform": {`"Linux"`},
			"sec-fetch-dest":     {"empty"},
			"sec-fetch-mode":     {"cors"},
			"sec-fetch-site":     {"same-origin"},
			"x-requested-with":   {"XMLHttpRequest"},
			"user-agent":         {ua},
			"referer":            {"https://www.ozon.ru/product/" + id + "/"},
			"cookie":             {cookie},
			fhttp.HeaderOrderKey: {
				"accept", "accept-language", "sec-ch-ua", "sec-ch-ua-mobile",
				"sec-ch-ua-platform", "sec-fetch-dest", "sec-fetch-mode",
				"sec-fetch-site", "x-requested-with", "user-agent", "referer", "cookie",
			},
		}
		return req, nil
	}

	// mobile: путь /products/ (множественное!) + layout-параметры приложения.
	// page_index=1 — ВЕРХ карточки (цена/заголовок/галерея); index=2 это вторичный
	// контент (характеристики/описание/рекомендации, цены там нет).
	inner := "/products/" + id + "/?layout_container=pdppage2copy&layout_page_index=1"
	api := "https://api.ozon.ru/composer-api.bx/page/json/v2?url=" + url.QueryEscape(inner)
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, api, nil)
	if err != nil {
		return nil, err
	}
	req.Header = fhttp.Header{
		"accept":            {"application/json; charset=utf-8"},
		"user-agent":        {ozonAppUA},
		"x-o3-app-name":     {ozonAppName},
		"x-o3-app-version":  {ozonAppVer},
		"x-o3-device-type":  {"mobile"},
		"x-o3-fp":           {ozonStaticFP},
		"x-o3-language":     {"ru"},
		"x-o3-sample-trace": {"false"},
		"mobile-gaid":       {randGAID()},
		"mobile-lat":        {"0"},
		"cookie":            {cookie},
		fhttp.HeaderOrderKey: {
			"accept", "user-agent", "x-o3-app-name", "x-o3-app-version",
			"x-o3-device-type", "x-o3-fp", "x-o3-language", "x-o3-sample-trace",
			"mobile-gaid", "mobile-lat", "cookie",
		},
	}
	return req, nil
}

// randGAID — случайный Google Advertising ID (uuid-подобный) для заголовка
// MOBILE-GAID. Значение нефиксированное и приложением не проверяется на сервере.
func randGAID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func snippet(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n]
	}
	return s
}

// currentEgressIP узнаёт текущий exit-IP прокси (через тот же tls-client) —
// только для диагностики ротации на FAB-блоке.
func (s *OzonScraper) currentEgressIP(ctx context.Context) string {
	// В browser-режиме tls-client нет: egress держат дорожки сайдкара (он сам
	// логирует свой exit-IP). Диагностику egress отсюда пропускаем.
	if s.client == nil {
		return ""
	}
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, "https://api.ipify.org?format=json", nil)
	if err != nil {
		return ""
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	var r struct {
		IP string `json:"ip"`
	}
	_ = json.Unmarshal(b, &r)
	return r.IP
}

func bytesHasFAB(b []byte) bool {
	return strings.Contains(string(b), "incidentId") || strings.Contains(string(b), "fab_")
}

// Ловит и подтипы (fab_chlg_/fab_cp_/fab_nmk_), и общий формат (fab_<timestamp>_).
var fabIncidentRe = regexp.MustCompile(`fab_[A-Za-z0-9]+_[A-Za-z0-9]+`)

// ── Разбор widgetStates ──────────────────────────────────────────────────────
//
// Ответ entrypoint-api: {"widgetStates": {"<widgetName>-<hash>": "<json-строка>"}}.
// Цена/название/галерея лежат в виджетах (имена нестабильны между web/mobile,
// полагаемся на содержимое — см. extractOzonPrice/Name/Image):
//   price* / webPrice* → price.price[] из {text,textStyle} (mobile) или плоско (web)
//   navTitle* / webProductHeading* → {"title":"..."}
//   galleryPreview* / webGallery*  → URL фото на ir.ozone.ru/.../multimedia-…

type ozonEnvelope struct {
	WidgetStates map[string]string `json:"widgetStates"`
}

func parseOzonWidgets(body []byte) (*Result, error) {
	var env ozonEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("ozon decode envelope: %w", err)
	}
	if len(env.WidgetStates) == 0 {
		return nil, fmt.Errorf("ozon: empty widgetStates")
	}

	res := &Result{
		Price:    extractOzonPrice(env.WidgetStates),
		Name:     extractOzonName(env.WidgetStates),
		ImageURL: extractOzonImage(env.WidgetStates),
		InStock:  true,
	}

	if res.Price == 0 {
		// Протухшая сессия: вместо карточки пришла страница логина (200, но цены нет).
		// Отдельный сигнал — НЕ путать с «товар не найден».
		if isOzonLoginGate(env.WidgetStates) {
			return nil, ErrAuthExpired
		}
		// 18+ гейт: цены нет, потому что Ozon прячет товар за подтверждением возраста.
		if isOzonAgeGated(env.WidgetStates) {
			return nil, ErrAgeRestricted
		}
		// Диагностика на случай неудачи: имена виджетов + сырой JSON виджета с ₽.
		keys := make([]string, 0, len(env.WidgetStates))
		var priceRaw, priceKey string
		for k, v := range env.WidgetStates {
			keys = append(keys, k)
			if priceRaw == "" && strings.Contains(v, "₽") {
				priceRaw, priceKey = v, k
			}
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("%w: price not found; widgets=%v; priceKey=%s; priceRaw=%s",
			ErrProductNotFound, keys, priceKey, snippet([]byte(priceRaw), 700))
	}
	if res.Name == "" {
		res.Name = "Товар Ozon"
	}
	return res, nil
}

// isOzonLoginGate эвристически распознаёт страницу/виджет логина — признак
// протухшей аккаунт-сессии (Ozon отдал 200, но просит авторизоваться вместо
// карточки товара). Маркеры best-effort.
func isOzonLoginGate(ws map[string]string) bool {
	for k, v := range ws {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "login") || strings.Contains(lk, "signin") ||
			strings.Contains(lk, "auth") {
			return true
		}
		lv := strings.ToLower(v)
		if strings.Contains(lv, "войдите") || strings.Contains(lv, "авториз") ||
			strings.Contains(lv, "войти в озон") || strings.Contains(lv, "войти в ozon") {
			return true
		}
	}
	return false
}

// isOzonAgeGated эвристически распознаёт возрастной гейт 18+ (нож/алкоголь): когда
// аккаунт не подтвердил 18+, Ozon вместо цены отдаёт виджет/текст подтверждения
// возраста. Маркеры best-effort (точную сигнатуру 18+-ответа вживую не снимали).
func isOzonAgeGated(ws map[string]string) bool {
	for k, v := range ws {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "adult") || strings.Contains(lk, "ageverif") ||
			strings.Contains(lk, "age_verif") {
			return true
		}
		lv := strings.ToLower(v)
		if strings.Contains(lv, "adultmodal") ||
			strings.Contains(lv, "для взрослых") ||
			strings.Contains(lv, "вам есть 18") ||
			strings.Contains(lv, "вам уже есть 18") ||
			strings.Contains(lv, "подтвердите возраст") {
			return true
		}
	}
	return false
}

// candidateWidgets возвращает значения виджетов в порядке приоритета: сперва с
// именем, начинающимся на namePrefix, затем чьё имя содержит nameSub, затем (если
// withRuble) любые со знаком ₽ в значении. Имена нестабильны между web/mobile,
// поэтому полагаемся не на точное имя, а на содержимое.
func candidateWidgets(ws map[string]string, namePrefix, nameSub string, withRuble bool) []string {
	var prio, mid, low []string
	for k, v := range ws {
		lk := strings.ToLower(k)
		switch {
		case namePrefix != "" && strings.HasPrefix(k, namePrefix):
			prio = append(prio, k)
		case nameSub != "" && strings.Contains(lk, nameSub):
			mid = append(mid, k)
		case withRuble && strings.Contains(v, "₽"):
			low = append(low, k)
		}
	}
	sort.Strings(prio)
	sort.Strings(mid)
	sort.Strings(low)
	out := make([]string, 0, len(prio)+len(mid)+len(low))
	for _, list := range [][]string{prio, mid, low} {
		for _, k := range list {
			out = append(out, ws[k])
		}
	}
	return out
}

// extractOzonPrice достаёт цену из ценового виджета (имя webPrice* / содержит
// "price" / со знаком ₽). Реальная структура мобильного API: price.price[] —
// массив строк цены вида {"text":"202 ₽","textStyle":"PRICE"}. Берём по textStyle
// PRICE (текущая) → CARD_PRICE (с Ozon Картой) → ORIGINAL_PRICE (старая). Фолбэк —
// старый рекурсивный поиск строковых полей price/cardPrice/originalPrice.
func extractOzonPrice(ws map[string]string) float64 {
	for _, raw := range candidateWidgets(ws, "webPrice", "price", true) {
		var data any
		if json.Unmarshal([]byte(raw), &data) != nil {
			continue
		}
		byStyle := map[string]string{}
		findPriceTexts(data, byStyle)
		for _, style := range []string{"PRICE", "CARD_PRICE", "ORIGINAL_PRICE"} {
			if v := parseRubles(byStyle[style]); v > 0 {
				return v
			}
		}
		// фолбэк: вдруг цена пришла плоской строкой в поле price/cardPrice/originalPrice.
		found := map[string]string{}
		findStringFields(data, map[string]bool{
			"price": true, "cardprice": true, "originalprice": true,
		}, found)
		for _, f := range []string{"price", "cardprice", "originalprice"} {
			if v := parseRubles(found[f]); v > 0 {
				return v
			}
		}
	}
	return 0
}

// findPriceTexts рекурсивно собирает text по textStyle из объектов вида
// {"text":"202 ₽","textStyle":"PRICE"} (требуем цифру в тексте, чтобы отсечь
// подписи без цены). Первое значение для каждого стиля побеждает.
func findPriceTexts(v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		text, hasText := t["text"].(string)
		style, hasStyle := t["textStyle"].(string)
		if hasText && hasStyle && strings.ContainsAny(text, "0123456789") {
			st := strings.ToUpper(style)
			if _, done := out[st]; !done {
				out[st] = text
			}
		}
		for _, val := range t {
			findPriceTexts(val, out)
		}
	case []any:
		for _, e := range t {
			findPriceTexts(e, out)
		}
	}
}

// extractOzonName достаёт заголовок товара ДЕТЕРМИНИРОВАННО и стабильно между
// запросами. Прежняя «чехарда» заголовков имела две причины: (1) Ozon A/B-шит
// заголовки и иногда подмешивает посторонние title-несущие виджеты (секции «с этим
// покупают», табы, хлебные крошки); (2) сам парсер ходил по map в случайном порядке
// (range по map в Go рандомизирован), поэтому один и тот же ответ мог дать разный
// заголовок. Лечим обе:
//   - виджеты перебираем по ФИКС. приоритету ИМЕНИ — выделенный заголовок товара
//     (мобильный navTitle* / web webProductHeading*) → общий *heading* → любой *title*;
//     внутри приоритета ключи отсортированы → выделенный виджет всегда побеждает фолбэк;
//   - сам title внутри виджета ищем детерминированно (findFirstString теперь
//     shallowest-first), поэтому одинаковый ответ всегда даёт один и тот же заголовок.
func extractOzonName(ws map[string]string) string {
	for _, sub := range []string{"navtitle", "productheading", "heading", "title"} {
		keys := make([]string, 0)
		for k := range ws {
			if strings.Contains(strings.ToLower(k), sub) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			var data any
			if json.Unmarshal([]byte(ws[k]), &data) != nil {
				continue
			}
			// title может быть строкой ИЛИ объектом {text:...}; пробуем оба ключа.
			if s := findFirstString(data, "title"); s != "" {
				return strings.TrimSpace(s)
			}
			if s := findFirstString(data, "text"); s != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// extractOzonImage достаёт URL фото товара. Продуктовые картинки Ozon лежат на CDN
// с "/multimedia" в пути (ir.ozone.ru/s3/multimedia-…), что отличает их от иконок
// банков/доставки (payments-cdn и пр.). Сначала смотрим галерейный виджет, затем
// фолбэком — любой виджет (структура имён нестабильна между web/mobile).
func extractOzonImage(ws map[string]string) string {
	for _, raw := range candidateWidgets(ws, "webGallery", "gallery", false) {
		var data any
		if json.Unmarshal([]byte(raw), &data) != nil {
			continue
		}
		if u := findOzonImageURL(data); u != "" {
			return u
		}
	}
	for _, raw := range ws {
		var data any
		if json.Unmarshal([]byte(raw), &data) != nil {
			continue
		}
		if u := findOzonImageURL(data); u != "" {
			return u
		}
	}
	return ""
}

// findOzonImageURL рекурсивно возвращает первый строковый URL продуктовой картинки.
func findOzonImageURL(v any) string {
	switch t := v.(type) {
	case string:
		if isOzonImageURL(t) {
			return t
		}
	case map[string]any:
		// обложка важнее прочих кадров — пробуем явные ключи первыми (детерминированно).
		for _, k := range []string{"coverImage", "image", "img", "src", "link", "url"} {
			if s, ok := t[k].(string); ok && isOzonImageURL(s) {
				return s
			}
		}
		for _, val := range t {
			if u := findOzonImageURL(val); u != "" {
				return u
			}
		}
	case []any:
		for _, e := range t {
			if u := findOzonImageURL(e); u != "" {
				return u
			}
		}
	}
	return ""
}

func isOzonImageURL(s string) bool {
	if !strings.HasPrefix(s, "http") {
		return false
	}
	ls := strings.ToLower(s)
	return (strings.Contains(ls, "ozone.ru") || strings.Contains(ls, "ozon.ru")) &&
		strings.Contains(ls, "multimedia")
}

// ozonImageDiag достаёт имена виджетов и сырой галерейный виджет для диагностики,
// когда фото не нашлось.
func ozonImageDiag(body []byte) (names []string, gallery string) {
	var env ozonEnvelope
	if json.Unmarshal(body, &env) != nil {
		return
	}
	for k, v := range env.WidgetStates {
		names = append(names, k)
		if gallery == "" && strings.Contains(strings.ToLower(k), "gallery") {
			gallery = v
		}
	}
	sort.Strings(names)
	return
}

// findStringFields рекурсивно собирает первое строковое значение для каждого из
// искомых имён полей (lowercase), если строка содержит цифру (цена/число).
func findStringFields(v any, names map[string]bool, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			lk := strings.ToLower(k)
			if names[lk] {
				if s, ok := val.(string); ok {
					if _, done := out[lk]; !done && strings.ContainsAny(s, "0123456789") {
						out[lk] = s
					}
				}
			}
			findStringFields(val, names, out)
		}
	case []any:
		for _, e := range t {
			findStringFields(e, names, out)
		}
	}
}

// findFirstString ДЕТЕРМИНИРОВАННО возвращает строковое значение поля name
// (lowercase) на МИНИМАЛЬНОЙ глубине; при равной глубине ключи объектов перебираются
// в отсортированном порядке. Детерминизм важен: range по map в Go рандомизирован, и
// прежний рекурсивный обход на одном и том же ответе мог вернуть разный заголовок
// (вложенный title хлебных крошек/табов вместо собственного title виджета).
// Shallowest-first выбирает «собственный» заголовок виджета, лежащий выше вложенных.
func findFirstString(root any, name string) string {
	name = strings.ToLower(name)
	queue := []any{root}
	for len(queue) > 0 {
		var next []any
		for _, v := range queue {
			switch t := v.(type) {
			case map[string]any:
				keys := make([]string, 0, len(t))
				for k := range t {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if strings.ToLower(k) == name {
						if s, ok := t[k].(string); ok && s != "" {
							return s
						}
					}
				}
				for _, k := range keys {
					next = append(next, t[k])
				}
			case []any:
				next = append(next, t...)
			}
		}
		queue = next
	}
	return ""
}

var nonDigitRe = regexp.MustCompile(`[^\d]`)

// parseRubles превращает "1 299 ₽" (с обычными/узкими/неразрывными пробелами и
// символом валюты) в 1299. Копейки у Ozon в этих полях не приходят.
func parseRubles(s string) float64 {
	digits := nonDigitRe.ReplaceAllString(s, "")
	if digits == "" {
		return 0
	}
	v, err := strconv.ParseFloat(digits, 64)
	if err != nil {
		return 0
	}
	return v
}
