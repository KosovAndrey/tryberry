package scraper

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// LinkResolver разворачивает «короткие» ссылки из мобильных приложений
// маркетплейсов (кнопка «Поделиться» даёт не товарный URL, а редирект-обёртку
// вида ozon.ru/t/… или a.aliexpress.com/_…). Сами по себе они не матчатся ни
// одним скрейпером, и /track падал в главное меню — теряя пользователя на первом
// контакте. Резолвер идёт по 3xx до канонического товарного URL, который дальше
// распознаётся обычным Registry.FindByURL.
//
// Резолвим ТОЛЬКО известные хосты-редиректоры (allowlist shortLinkPatterns), а не
// любой http-URL из текста: иначе бот слал бы исходящий запрос на произвольный
// хост, который пользователь вставил в чат (SSRF / шум). Для обычных ссылок
// (полный товарный URL) и текста без коротких ссылок сетевых запросов нет.
type LinkResolver struct {
	client *http.Client
}

// shortLinkPattern — маркер ссылки-редиректора: host (точное совпадение либо
// поддомен) и опциональный префикс пути (пусто = любой путь на этом хосте).
type shortLinkPattern struct {
	host string
	path string
}

// Подтверждённые форматы share-ссылок мобильных приложений:
//   - Ozon:       https://ozon.ru/t/XXXXXXX        (тот же хост, путь /t/)
//   - AliExpress: https://a.aliexpress.com/_XXXXXX  (мобильный шэр)
//     https://s.click.aliexpress.com/…  (партнёрский редирект)
//     https://aliexpress.ru/e/_XXXXXX   (RU-шэр)
//
// Wildberries и Я.Маркет в приложениях отдают уже полный товарный URL
// (wildberries.ru/catalog/…/detail.aspx, market.yandex.ru/…) — он матчится
// напрямую, в резолве не нуждается; добавим сюда, если всплывёт реальный шортнер.
var shortLinkPatterns = []shortLinkPattern{
	{host: "a.aliexpress.com"},
	{host: "s.click.aliexpress.com"},
	{host: "aliexpress.ru", path: "/e/"},
	{host: "ozon.ru", path: "/t/"},
}

const (
	resolverMaxRedirects = 8
	resolverUA           = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0 Mobile Safari/537.36"
)

var resolverURLRe = regexp.MustCompile(`https?://[^\s]+`)

// NewLinkResolver строит резолвер с собственным http-клиентом (прямой, без прокси:
// редиректоры отдают 3xx с Location без антибота — для самого редиректа IP-репутация
// не нужна, а тяжёлую товарную страницу мы не тянем, её скрейпит уже нужный скрейпер
// со своим egress). timeout<=0 → 8с.
func NewLinkResolver(timeout time.Duration) *LinkResolver {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	return &LinkResolver{
		client: &http.Client{
			Timeout: timeout,
			// Явный direct-transport: Proxy=nil. НЕ наследуем HTTPS_PROXY из окружения
			// (в bot-worker он указывает на xray — немецкий VLESS-exit для egress в
			// Telegram). Гонять через него RU-редиректоры (ozon.ru/t/, aliexpress) —
			// неверный гео-egress; резолвим напрямую с IP сервера, как direct-клиент
			// Ali-скрейпера.
			Transport: &http.Transport{
				Proxy:               nil,
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= resolverMaxRedirects {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
	}
}

// ExpandInText заменяет в тексте каждую короткую ссылку на её канонический URL.
// Текст без ссылок-редиректоров возвращается без изменений и без сетевых запросов.
func (r *LinkResolver) ExpandInText(ctx context.Context, text string) string {
	if !strings.Contains(text, "://") {
		return text
	}
	return resolverURLRe.ReplaceAllStringFunc(text, func(u string) string {
		return r.Expand(ctx, u)
	})
}

// Expand разворачивает одну короткую ссылку в канонический товарный URL. Если raw
// не из allowlist — возвращает его как есть (без сети). При любой ошибке резолва
// (таймаут, сеть, битый редирект) возвращает исходный raw: пусть дальше его честно
// отвергнет FindByURL, а не молча проглотит.
func (r *LinkResolver) Expand(ctx context.Context, raw string) string {
	if !isShortLink(raw) {
		return raw
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return raw
	}
	req.Header.Set("User-Agent", resolverUA)
	resp, err := r.client.Do(req)
	if err != nil {
		return raw
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL == nil {
		return raw
	}
	final := resp.Request.URL.String()
	if final == "" {
		return raw
	}
	return final
}

// isShortLink сообщает, относится ли URL к известным хостам-редиректорам.
func isShortLink(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(strings.TrimPrefix(u.Host, "www."))
	for _, p := range shortLinkPatterns {
		if host != p.host && !strings.HasSuffix(host, "."+p.host) {
			continue
		}
		if p.path == "" || strings.HasPrefix(u.Path, p.path) {
			return true
		}
	}
	return false
}
