package scraper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// Режимы запроса к Ozon. mobile = api.ozon.ru/composer-api.bx + okhttp-TLS +
// заголовки приложения (рецепт, дававший живой 200); web = www.ozon.ru/
// entrypoint-api.bx + Chrome-TLS (на нём FAB отдавал fab_cp_ даже с валидным ETC).
const (
	ozonModeMobile = "mobile"
	ozonModeWeb    = "web"

	// Заголовки мобильного приложения (из снятого живого 200-запроса).
	ozonAppUA   = "ozonapp_android/19.20.0+2684"
	ozonAppName = "ozonapp_android"
	ozonAppVer  = "19.20.0(2684)"
	// x-o3-fp — статическая константа приложения (не device-fp), см. research.
	ozonStaticFP = "1.01ae145142fa31f9"
)

// OzonScraper получает цену/название/картинку товара Ozon через storefront-API
// (entrypoint-api.bx → widgetStates), переиспользуя антибот-cookie __Secure-ETC,
// добытый сайдкаром ozon-miner (см. ozon-miner/miner.py).
//
// Почему так, а не чистый Go-запрос: эндпоинт закрыт антиботом FAB, который на
// первом контакте отдаёт обфусцированный JS-VM challenge.html (чистым Go не
// пройти) и режет по TLS-отпечатку + репутации IP. Решение: браузер-майнер
// изредка решает челлендж в WebView через мобильный РФ-прокси и кладёт cookie в
// Redis; здесь мы шлём дешёвые частые запросы с этим cookie через ТОТ ЖЕ прокси
// (ETC привязан к IP) и с браузерным TLS (bogdanfinn/tls-client, Chrome-профиль
// под Chromium майнера). Браузер — НЕ на каждый скрейп.
type OzonScraper struct {
	client     tls_client.HttpClient // nil → режим только Matches (бот/api)
	rc         *redis.Client
	limiter    *rate.Limiter
	log        *slog.Logger
	slotKey    string // ozon:etc:pool:0
	mode       string // ozonModeMobile | ozonModeWeb
	configured bool

	// account-режим (путь B): cookie из аккаунт-токенов вместо ETC из Redis.
	accountCookie string // "" → используем ETC-слот из Redis
}

// OzonOptions — конфигурация рабочего скрейпера. Нулевое значение даёт
// «облегчённый» скрейпер: Matches работает (нужно боту/api для разбора URL),
// а Scrape вернёт ошибку. Реальный скрейп включается, когда заданы RedisClient
// и ProxyURL (мобильный РФ-прокси — тот же, что у ozon-miner).
type OzonOptions struct {
	RedisClient *redis.Client
	ProxyURL    string  // http://user:pass@host:port мобильного прокси
	PoolPrefix  string  // префикс ключей Redis; пусто → "ozon:etc:"
	RPS         float64 // лимит запросов к Ozon (один IP → держим низким), 0 → 1
	Mode        string  // "mobile" (по умолчанию) | "web"
	Logger      *slog.Logger

	// Путь B (аккаунт-токены): если задан AccessToken — скрейпер ходит под
	// залогиненным аккаунтом (cookie __Secure-access-token/__Secure-refresh-token),
	// без ETC-майнера и Redis. Доверенная сессия проходит FAB. Секреты — из .env.
	AccessToken  string
	RefreshToken string
	// Cookie — готовая cookie-строка целиком (снятая из запроса приложения через
	// HTTP Toolkit): самый надёжный вариант, несёт все куки аккаунт-сессии
	// (access/refresh-token, __Secure-user-id, abt_data). Приоритетнее токенов.
	Cookie string
}

func NewOzonScraper(opts OzonOptions) *OzonScraper {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	prefix := opts.PoolPrefix
	if prefix == "" {
		prefix = "ozon:etc:"
	}
	rps := opts.RPS
	if rps <= 0 {
		rps = 1
	}
	mode := opts.Mode
	if mode != ozonModeWeb {
		mode = ozonModeMobile
	}

	s := &OzonScraper{
		rc:      opts.RedisClient,
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
		log:     log,
		slotKey: prefix + "pool:0",
		mode:    mode,
	}

	// Путь B: готовая cookie-строка или аккаунт-токены заданы → ходим под
	// залогиненной сессией (mobile/okhttp), ETC/Redis не нужны. Иначе — ETC-майнер.
	if opts.Cookie != "" {
		s.accountCookie = opts.Cookie // полная cookie-строка из приложения
	} else if opts.AccessToken != "" {
		s.accountCookie = "__Secure-access-token=" + opts.AccessToken
		if opts.RefreshToken != "" {
			s.accountCookie += "; __Secure-refresh-token=" + opts.RefreshToken
		}
	}
	if s.accountCookie != "" {
		s.mode = ozonModeMobile // аккаунт-API живёт на composer-api.bx (okhttp)
		mode = ozonModeMobile
	}

	// Клиент нужен и для account-режима, и для ETC-режима.
	if s.accountCookie != "" || opts.RedisClient != nil {
		client, err := newOzonTLSClient(opts.ProxyURL, mode)
		if err != nil {
			log.Error("ozon: tls-client init failed, scraper disabled", "err", err)
		} else {
			s.client = client
			s.configured = true
			auth := "etc-miner"
			if s.accountCookie != "" {
				auth = "account-token"
			}
			// длину cookie логируем (НЕ значение) — подтвердить, что .env подхватился.
			log.Info("ozon scraper configured", "mode", mode, "auth", auth,
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
		return nil, fmt.Errorf("%w: ozon scraper not configured (no redis/tls-client)", ErrNotImplemented)
	}
	id, err := extractOzonID(url)
	if err != nil {
		return nil, err
	}
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	cookie, ua, mintedIP, err := s.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: no ozon ETC token (miner not ready?)", ErrMarketplaceBlocked)
	}
	if ua == "" {
		ua = ozonDefaultUA
	}

	req, err := s.buildRequest(ctx, id, cookie, ua)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ozon request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	// FAB-блок / протухший ETC: помечаем слот битым, чтобы майнер перевыдал.
	// Диагностика: сверяем IP минта ETC с текущим egress — расхождение значит,
	// что прокси отротировал IP (ETC привязан к IP) → лечится sticky-режимом
	// прокси, а не кодом. Совпадение → дело в TLS/заголовках/сессии (incident +
	// тело подскажут: fab_chlg_ = токен не признан, иное = признан, но запрос режут).
	if resp.StatusCode == 403 || bytesHasFAB(body) {
		incident := fabIncidentRe.FindString(string(body))
		curIP := s.currentEgressIP(ctx)
		s.markBad(ctx)
		s.log.Warn("ozon: FAB block / stale ETC",
			"status", resp.StatusCode, "id", id, "mode", s.mode,
			"minted_ip", mintedIP, "current_ip", curIP,
			"ip_rotated", mintedIP != "" && curIP != "" && mintedIP != curIP,
			"incident", incident, "body", snippet(body, 300))
		return nil, ErrMarketplaceBlocked
	}
	if resp.StatusCode == 404 {
		return nil, ErrProductNotFound
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ozon status %d", resp.StatusCode)
	}

	res, err := parseOzonWidgets(body)
	if err != nil {
		return nil, err
	}
	return res, nil
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

// token отдаёт cookie+UA для запроса. Путь B (account): cookie из аккаунт-токенов.
// Путь ETC: свежий слот майнера (+ IP, на котором он добыт).
func (s *OzonScraper) token(ctx context.Context) (cookie, ua, mintedIP string, err error) {
	if s.accountCookie != "" {
		return s.accountCookie, ozonAppUA, "", nil
	}
	if s.rc == nil {
		return "", "", "", fmt.Errorf("redis unavailable")
	}
	h, err := s.rc.HGetAll(ctx, s.slotKey).Result()
	if err != nil {
		return "", "", "", err
	}
	if h["status"] != "ok" || h["cookie"] == "" {
		return "", "", "", fmt.Errorf("no healthy ozon ETC")
	}
	return h["cookie"], h["ua"], h["ip"], nil
}

// currentEgressIP узнаёт текущий exit-IP прокси (через тот же tls-client) —
// только для диагностики ротации на FAB-блоке.
func (s *OzonScraper) currentEgressIP(ctx context.Context) string {
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

// markBad помечает слот битым — майнер перевыдаст ETC на следующем цикле.
// В account-режиме слота нет — ничего не делаем.
func (s *OzonScraper) markBad(ctx context.Context) {
	if s.accountCookie != "" || s.rc == nil {
		return
	}
	if err := s.rc.HSet(ctx, s.slotKey,
		"status", "broken",
		"broken_at", time.Now().Unix(),
	).Err(); err != nil {
		s.log.Warn("ozon: mark slot broken failed", "err", err)
	}
}

func bytesHasFAB(b []byte) bool {
	return strings.Contains(string(b), "incidentId") || strings.Contains(string(b), "fab_")
}

// Ловит и подтипы (fab_chlg_/fab_cp_/fab_nmk_), и общий формат (fab_<timestamp>_).
var fabIncidentRe = regexp.MustCompile(`fab_[A-Za-z0-9]+_[A-Za-z0-9]+`)

const ozonDefaultUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// ── Разбор widgetStates ──────────────────────────────────────────────────────
//
// Ответ entrypoint-api: {"widgetStates": {"<widgetName>-<hash>": "<json-строка>"}}.
// Цена/название/галерея лежат в виджетах с устойчивыми префиксами имён:
//   webPrice*          → {"price":"1 299 ₽","cardPrice":"1 199 ₽","originalPrice":"2 000 ₽"}
//   webProductHeading* → {"title":"..."}
//   webGallery*        → {"images":[{"src":"https://..."}],"coverImage":"https://..."}
// Префиксы стабильны у Ozon много лет; точные поля сверим по первому живому 200
// (см. OZON_VALIDATE_PRODUCT в ozon-miner) и при необходимости подправим.

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
	}

	if res.Price == 0 {
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
			ErrProductNotFound, keys, priceKey, snippet([]byte(priceRaw), 2500))
	}
	if res.Name == "" {
		res.Name = "Товар Ozon"
	}
	return res, nil
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

// extractOzonPrice ищет цену рекурсивно по полям price/cardPrice/originalPrice в
// ценовом виджете (имя webPrice* / содержит "price" / со знаком ₽). Берём «price»
// (текущая цена), фолбэк cardPrice (цена с Ozon Картой), затем originalPrice.
func extractOzonPrice(ws map[string]string) float64 {
	for _, raw := range candidateWidgets(ws, "webPrice", "price", true) {
		var data any
		if json.Unmarshal([]byte(raw), &data) != nil {
			continue
		}
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

func extractOzonName(ws map[string]string) string {
	for _, raw := range candidateWidgets(ws, "webProductHeading", "heading", false) {
		var data any
		if json.Unmarshal([]byte(raw), &data) != nil {
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
	return ""
}

func extractOzonImage(ws map[string]string) string {
	for _, raw := range candidateWidgets(ws, "webGallery", "gallery", false) {
		var data any
		if json.Unmarshal([]byte(raw), &data) != nil {
			continue
		}
		// имена в lowercase: findFirstString сравнивает с уже lowercase-ключом JSON.
		for _, key := range []string{"coverimage", "src", "image", "link", "url"} {
			if s := findFirstString(data, key); strings.HasPrefix(s, "http") {
				return s
			}
		}
	}
	return ""
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

// findFirstString рекурсивно возвращает первое непустое строковое значение для
// поля name (lowercase).
func findFirstString(v any, name string) string {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if strings.ToLower(k) == name {
				if s, ok := val.(string); ok && s != "" {
					return s
				}
			}
		}
		for _, val := range t {
			if s := findFirstString(val, name); s != "" {
				return s
			}
		}
	case []any:
		for _, e := range t {
			if s := findFirstString(e, name); s != "" {
				return s
			}
		}
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
