package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
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
	configured bool
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
	Logger      *slog.Logger
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

	s := &OzonScraper{
		rc:      opts.RedisClient,
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
		log:     log,
		slotKey: prefix + "pool:0",
	}

	// Рабочий режим — только если есть Redis (источник ETC). Прокси формально
	// опционален (без него почти наверняка прилетит fab_nmk_), но клиент строим.
	if opts.RedisClient != nil {
		client, err := newOzonTLSClient(opts.ProxyURL)
		if err != nil {
			log.Error("ozon: tls-client init failed, scraper disabled", "err", err)
		} else {
			s.client = client
			s.configured = true
		}
	}
	return s
}

func newOzonTLSClient(proxyURL string) (tls_client.HttpClient, error) {
	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(20),
		// Профиль под Chromium, которым ozon-miner добывает ETC: TLS-отпечаток
		// запроса должен совпадать с тем, что прошёл FAB при минте cookie.
		tls_client.WithClientProfile(profiles.Chrome_124),
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

	cookie, ua, err := s.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: no ozon ETC token (miner not ready?)", ErrMarketplaceBlocked)
	}

	api := "https://www.ozon.ru/api/entrypoint-api.bx/page/json/v2?url=/product/" + id + "/"
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, api, nil)
	if err != nil {
		return nil, err
	}
	if ua == "" {
		ua = ozonDefaultUA
	}
	req.Header = fhttp.Header{
		"accept":             {"application/json"},
		"accept-language":    {"ru,en;q=0.9"},
		"user-agent":         {ua},
		"cookie":             {cookie},
		"x-o3-app-name":      {"ozon"},
		"referer":            {"https://www.ozon.ru/product/" + id + "/"},
		fhttp.HeaderOrderKey: {"accept", "accept-language", "user-agent", "cookie", "x-o3-app-name", "referer"},
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ozon request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	// FAB-блок / протухший ETC: помечаем слот битым, чтобы майнер перевыдал.
	if resp.StatusCode == 403 || bytesHasFAB(body) {
		s.markBad(ctx)
		s.log.Warn("ozon: FAB block / stale ETC", "status", resp.StatusCode, "id", id)
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

// token читает свежий ETC-слот, выданный майнером.
func (s *OzonScraper) token(ctx context.Context) (cookie, ua string, err error) {
	if s.rc == nil {
		return "", "", fmt.Errorf("redis unavailable")
	}
	h, err := s.rc.HGetAll(ctx, s.slotKey).Result()
	if err != nil {
		return "", "", err
	}
	if h["status"] != "ok" || h["cookie"] == "" {
		return "", "", fmt.Errorf("no healthy ozon ETC")
	}
	return h["cookie"], h["ua"], nil
}

// markBad помечает слот битым — майнер перевыдаст ETC на следующем цикле.
func (s *OzonScraper) markBad(ctx context.Context) {
	if s.rc == nil {
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

	var res Result
	for name, raw := range env.WidgetStates {
		switch {
		case strings.HasPrefix(name, "webPrice") && res.Price == 0:
			res.Price = parseOzonPrice(raw)
		case strings.HasPrefix(name, "webProductHeading") && res.Name == "":
			res.Name = parseOzonTitle(raw)
		case strings.HasPrefix(name, "webGallery") && res.ImageURL == "":
			res.ImageURL = parseOzonImage(raw)
		}
	}

	if res.Price == 0 {
		return nil, fmt.Errorf("%w: price not found in widgetStates", ErrProductNotFound)
	}
	if res.Name == "" {
		res.Name = "Товар Ozon"
	}
	return &res, nil
}

func parseOzonPrice(raw string) float64 {
	var w struct {
		Price         string `json:"price"`
		CardPrice     string `json:"cardPrice"`
		OriginalPrice string `json:"originalPrice"`
	}
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return 0
	}
	for _, s := range []string{w.Price, w.CardPrice, w.OriginalPrice} {
		if v := parseRubles(s); v > 0 {
			return v
		}
	}
	return 0
}

func parseOzonTitle(raw string) string {
	var w struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return ""
	}
	return strings.TrimSpace(w.Title)
}

func parseOzonImage(raw string) string {
	var w struct {
		CoverImage string `json:"coverImage"`
		Images     []struct {
			Src string `json:"src"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return ""
	}
	if w.CoverImage != "" {
		return w.CoverImage
	}
	if len(w.Images) > 0 {
		return w.Images[0].Src
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
