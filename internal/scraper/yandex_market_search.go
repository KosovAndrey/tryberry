package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	"golang.org/x/time/rate"
)

// YandexMarketSearchScraper — скрейпер поисковой выдачи Я.Маркета по ссылке вида
// market.yandex.ru/search?text=... . Транспорт тот же, что у карточки
// (tls-client + RU-прокси, см. YandexMarketScraper): SmartCaptcha рубит голый
// Go-TLS на датацентровом IP. Реализует SearchScraper.
//
// ВАЖНО: выдача Я.Маркета — не публичный JSON-API (как у WB), а SSR-страница с
// инлайн-стейтом marketfront. Структура стейта дрейфует и НЕ зафиксирована
// эмпирически (локально страница недоступна — антибот). Поэтому ScrapeSearch
// сделан best-effort + насыщенное диагностическое логирование: по прод-логам
// дорабатываем парсер (ymSearchDiag). Цены — в КОПЕЙКАХ (как контракт SearchItem).
type YandexMarketSearchScraper struct {
	*YandexMarketScraper
	maxItems int
	limiter  *rate.Limiter
}

var _ SearchScraper = (*YandexMarketSearchScraper)(nil)

// NewYandexMarketSearchScraper оборачивает уже сконфигуренный карточный скрейпер
// (переиспользуем его tls-client/прокси). maxItems<=0 → 60.
func NewYandexMarketSearchScraper(base *YandexMarketScraper, maxItems int) *YandexMarketSearchScraper {
	if maxItems <= 0 {
		maxItems = 60
	}
	return &YandexMarketSearchScraper{
		YandexMarketScraper: base,
		maxItems:            maxItems,
		// Выдача тяжёлая (~2.5 МБ) и идёт через один IP — держим темп низким.
		limiter: rate.NewLimiter(rate.Limit(0.5), 1),
	}
}

// ymBusinessPathRe — путь витрины продавца /business--<slug>/<id> (id — то же,
// что mrch/bi в generalContext; слаг для YM не важен, резолв по id).
var ymBusinessPathRe = regexp.MustCompile(`/business--[^/]+/(\d+)`)

// ymMerchantCtxRe — id продавца в generalContext: t=merchant;mrch=<id> или
// t=shopInShop;...;bi=<id>.
var ymMerchantCtxRe = regexp.MustCompile(`(?:mrch|bi)=(\d+)`)

// ymSellerID — id продавца из ссылки витрины: путь /business--*/<id> либо
// generalContext (merchant/shopInShop). "" если это не витрина.
func ymSellerID(u *url.URL) string {
	if m := ymBusinessPathRe.FindStringSubmatch(u.Path); len(m) == 2 {
		return m[1]
	}
	gc := u.Query().Get("generalContext") // Query() уже декодирует %3D/%3B
	if strings.Contains(gc, "merchant") || strings.Contains(gc, "shopInShop") {
		if m := ymMerchantCtxRe.FindStringSubmatch(gc); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

// ymBusinessSlugRe — слаг витрины из /business--<slug>/<id>.
var ymBusinessSlugRe = regexp.MustCompile(`/business--([^/]+)/\d+`)

// SellerName — имя витрины из слага ссылки (/business--<slug>/<id>):
// "yandex-fabrika" → "Yandex Fabrika". Для merchant-формы (/search?generalContext)
// слага нет → "" (ярлык останется «Магазин #id»). Сети не требует. Настоящее
// кириллическое имя зашито в schema-сжатый стейт marketfront — отдельная задача.
func (s *YandexMarketSearchScraper) SellerName(_ context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	m := ymBusinessSlugRe.FindStringSubmatch(u.Path)
	if len(m) != 2 || m[1] == "" || m[1] == "m" {
		return "", nil
	}
	return prettifyYMSlug(m[1]), nil
}

// prettifyYMSlug: "yandex-fabrika" → "Yandex Fabrika".
func prettifyYMSlug(slug string) string {
	parts := strings.Split(slug, "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// MatchesSearch — ссылка на выдачу Я.Маркета: /search (или параметр text), либо
// витрина продавца (/business--*/<id> или generalContext с merchant/shopInShop).
// Карточка (market.yandex.ru/card/...) сюда НЕ попадает — её разбирает Scrape.
func (s *YandexMarketSearchScraper) MatchesSearch(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if !strings.Contains(strings.ToLower(u.Host), "market.yandex.ru") {
		return false
	}
	if strings.HasPrefix(u.Path, "/card") || strings.Contains(u.Path, "/product") {
		return false // это карточка, не выдача
	}
	if ymSellerID(u) != "" {
		return true // витрина продавца
	}
	return strings.Contains(u.Path, "/search") || strings.TrimSpace(u.Query().Get("text")) != ""
}

// NormalizeSearchURL — канонический ключ дедупликации: text + (опц.) hid/категория.
// Без text (например, чистая категорийная ссылка без запроса) — ErrInvalidURL:
// такую выдачу нельзя надёжно дедуплицировать между пользователями.
func (s *YandexMarketSearchScraper) NormalizeSearchURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	// Витрина продавца → канонический ключ по id (слаг для YM не важен, резолв по
	// id — проверено probe'ом: /business--<любой>/<id> отдаёт те же товары). Обе
	// формы (business-страница и /search?generalContext=merchant) схлопываются.
	if id := ymSellerID(u); id != "" {
		return "https://market.yandex.ru/business--m/" + id, nil
	}
	text := strings.TrimSpace(u.Query().Get("text"))
	if text == "" {
		return "", fmt.Errorf("%w: no text query in yandex search URL", ErrInvalidURL)
	}
	text = strings.Join(strings.Fields(strings.ToLower(text)), " ")
	canon := url.Values{}
	canon.Set("text", text)
	// hid (категория) сужает выдачу — сохраняем в ключе, чтобы «кофемашина в
	// категории X» и «...в категории Y» не схлопывались в одну подписку.
	if hid := strings.TrimSpace(u.Query().Get("hid")); hid != "" {
		canon.Set("hid", hid)
	}
	return "https://market.yandex.ru/search?" + canon.Encode(), nil
}

// ymSearchPriceRe — те же сниппеты стейта, что у карточки, но их много (по одному
// на товар выдачи): "price":{"value":"25997","currency":"RUR"}.
var ymSearchPriceRe = regexp.MustCompile(`"price":\{"value":"(\d+(?:\.\d+)?)","currency":"(?:RUR|RUB)"`)

// ScrapeSearch — забрать выдачу. FIRST-PASS: структура SSR-стейта Я.Маркета не
// подтверждена на живой странице (локально антибот), поэтому парсер минимальный
// (цены из сниппетов) + диагностика в лог для доводки по проду. См. ymSearchDiag.
func (s *YandexMarketSearchScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	if s.YandexMarketScraper == nil || s.direct == nil {
		return nil, fmt.Errorf("%w: yandex search scraper not configured", ErrNotImplemented)
	}
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	// Тот же транспорт direct+proxy-fallback, что у карточки (общий jar).
	header := fhttp.Header{
		"accept":             {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"accept-language":    {"ru,en;q=0.9"},
		"user-agent":         {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"},
		fhttp.HeaderOrderKey: {"accept", "accept-language", "user-agent"},
	}
	status, body, _, err := s.getWithFallback(ctx, rawURL, header, 8<<20)
	if err != nil {
		return nil, fmt.Errorf("yandex search request: %w", err)
	}

	if isYandexCaptcha(body) {
		return nil, ErrMarketplaceBlocked
	}

	out := s.parseSearch(string(body))
	if len(out.Items) == 0 {
		// Диагностика для доводки парсера по прод-логам (как у карточки).
		s.log.Warn("yandex search: no items parsed",
			"url", rawURL, "status", status, "len", len(body),
			"price_hits", len(ymSearchPriceRe.FindAllStringIndex(string(body), -1)),
			"price_ctx", ymPriceContext(body),
			"cur_ctx", ymCurrencyContext(body))
		return out, ErrParseFailed
	}
	s.log.Info("yandex search scraped", "url", rawURL, "items", len(out.Items))
	return out, nil
}

// ymProductStartRe — начало объекта товарной модели в стейте marketfront:
// {"id":<число>,"entity":"product"... . id у модели — ЧИСЛО (у картинок/прочих
// сущностей — строка), поэтому якорь по `:\d+,` надёжно отбирает только модели.
var ymProductStartRe = regexp.MustCompile(`\{"id":\d+,"entity":"product"`)

// ymPictureRe — сущность картинки: {"id":"<hash>","entity":"avatars_picture",
// "origUrl":"https://avatars.mds.yandex.net/..."}. pictures у модели — массив
// этих хэшей; резолвим их в полный URL по этой карте.
var ymPictureRe = regexp.MustCompile(`\{"id":"([0-9a-f]{6,16})","entity":"avatars_picture","origUrl":"([^"]+)"`)

// ymSearchModel — нужные поля товарной модели из стейта marketfront. Цены —
// строки в рублях ("31990"); pictures — хэши, резолвятся через ymPictureRe.
type ymSearchModel struct {
	ID     json.Number `json:"id"`
	Entity string      `json:"entity"`
	Slug   string      `json:"slug"`
	Prices struct {
		Min string `json:"min"`
		Max string `json:"max"`
	} `json:"prices"`
	Titles struct {
		Raw string `json:"raw"`
	} `json:"titles"`
	Pictures []string `json:"pictures"`
}

// parseSearch — позиции выдачи из инлайн-стейта marketfront.
//
// Стейт — это schema-сжатый JSON в десятках <script data-apiary="chunks">, но
// товарные МОДЕЛИ лежат развёрнутыми объектами {"id":N,"entity":"product",...}
// с id/slug/titles/prices/pictures. Их и разбираем: находим каждый объект-модель
// (ymProductStartRe), вычитываем по балансу скобок и json-парсим (порядок полей
// в стейте дрейфует — regex по полям ненадёжен, что и давало мусор).
//
// Берём только модели с ценой (prices.min) и slug — это покупаемые офферы;
// модели без оффера (offersCount=0, кнопка «сообщить о поступлении») пропускаем:
// цены нет, трекать нечего. URL карточки собираем как /product--<slug>/<id> —
// это и стабильный ключ дедупликации (products апсертится по URL).
func (s *YandexMarketSearchScraper) parseSearch(html string) *SearchResultSet {
	out := &SearchResultSet{}

	pics := make(map[string]string)
	for _, m := range ymPictureRe.FindAllStringSubmatch(html, -1) {
		if _, ok := pics[m[1]]; !ok {
			pics[m[1]] = m[2]
		}
	}

	seen := make(map[string]bool)
	pos := 0
	for _, loc := range ymProductStartRe.FindAllStringIndex(html, -1) {
		obj := ymBalancedObject(html, loc[0])
		if obj == "" {
			continue
		}
		var m ymSearchModel
		if err := json.Unmarshal([]byte(obj), &m); err != nil {
			continue
		}
		id := m.ID.String()
		if m.Entity != "product" || id == "" || m.Slug == "" || m.Prices.Min == "" {
			continue
		}
		price, err := parsePriceString(m.Prices.Min)
		if err != nil || price <= 0 {
			continue
		}
		if seen[id] {
			continue // модель может встретиться в стейте повторно
		}
		seen[id] = true
		pos++
		if pos > s.maxItems {
			break
		}
		item := SearchItem{
			ArticleID:    id,
			Name:         m.Titles.Raw,
			URL:          "https://market.yandex.ru/product--" + m.Slug + "/" + id,
			Position:     pos,
			PriceKopecks: int64(price * 100),
		}
		if old, err := parsePriceString(m.Prices.Max); err == nil && old > price {
			item.OldPriceKopecks = int64(old * 100)
		}
		if len(m.Pictures) > 0 {
			item.ImageURL = pics[m.Pictures[0]]
		}
		out.Items = append(out.Items, item)
	}
	out.TotalFound = len(out.Items)
	return out
}

// ymBalancedObject — подстрока сбалансированного JSON-объекта, начинающегося в
// позиции start (html[start] == '{'). Учитывает строковые литералы и экраны,
// чтобы скобки внутри строк не ломали баланс. "" — если объект не закрыт.
func ymBalancedObject(html string, start int) string {
	depth := 0
	inStr := false
	for i := start; i < len(html); i++ {
		c := html[i]
		if inStr {
			switch c {
			case '\\':
				i++ // пропускаем экранированный символ
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return html[start : i+1]
			}
		}
	}
	return ""
}
