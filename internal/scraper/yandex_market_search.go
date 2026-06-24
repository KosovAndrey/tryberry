package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
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
		// Выдача тяжёлая (~2.5 МБ), но идёт direct без прокси (см.
		// docs/YANDEX-WARMED-COOKIES.md) — прежний 0.5 был из-за одного proxy-IP.
		// Поднимаем до 2; фолбэк-прокси и метрика source=proxy страхуют.
		limiter: rate.NewLimiter(rate.Limit(2), 1),
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

// ymSellerSearchURL — search-форма витрины продавца: /search?generalContext=
// t=merchant;mrch=<id> . Тот же /search SSR, что текстовый поиск (парсится
// parseSearch'ом), и в отличие от /business--*/<id> не флапает в лёгкий рендер.
func ymSellerSearchURL(id string) string {
	v := url.Values{}
	v.Set("text", "")
	v.Set("generalContext", "t=merchant;mrch="+id)
	return "https://market.yandex.ru/search?" + v.Encode()
}

// ymBusinessSlugRe — слаг витрины из /business--<slug>/<id>.
var ymBusinessSlugRe = regexp.MustCompile(`/business--([^/]+)/\d+`)

// ymOgTitleRe — og:title из head-блока витрины: "<Имя> – купить товары…".
// В HTML это JSON-описание тега ({"property":"og:title","content":"…"}), а не
// настоящий <meta>, поэтому матчим JSON-форму.
var ymOgTitleRe = regexp.MustCompile(`"property":"og:title","content":"([^"]+)"`)

// ymH1Re — заголовок витрины (фолбэк к og:title).
var ymH1Re = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>`)

// ymTagRe — для очистки <h1> от вложенных тегов.
var ymTagRe = regexp.MustCompile(`<[^>]+>`)

// ymNameSepRe — разделитель «Имя – купить…»/«Имя — …»/«Имя - …».
var ymNameSepRe = regexp.MustCompile(`\s+[–—-]\s+`)

// SellerName — настоящее имя витрины: фетчим страницу магазина (по
// нормализованному /business--m/<id>) и берём имя из og:title (head, читаем ~1 МБ)
// или <h1>. Работает для обеих форм ссылки (business и merchant). Фолбэк — слаг
// ссылки (/business--<slug>/<id>). "" если ничего не вышло → ярлык «Магазин #id».
func (s *YandexMarketSearchScraper) SellerName(ctx context.Context, rawURL string) (string, error) {
	if norm, err := s.NormalizeSearchURL(rawURL); err == nil {
		if status, body, _, e := s.getWithFallback(ctx, norm, ymCardHeader(), 1<<20); e == nil && status == 200 {
			if name := ymExtractSellerName(body); name != "" {
				return name, nil
			}
		}
	}
	// Фолбэк: слаг из ссылки (для business-формы), если фетч/парс не дал имени.
	if u, err := url.Parse(rawURL); err == nil {
		if m := ymBusinessSlugRe.FindStringSubmatch(u.Path); len(m) == 2 && m[1] != "" && m[1] != "m" {
			return prettifyYMSlug(m[1]), nil
		}
	}
	return "", nil
}

// ymExtractSellerName достаёт имя магазина из HTML витрины (og:title → <h1>),
// отбрасывая хвост «– купить…» и мусорный «Яндекс Маркет».
func ymExtractSellerName(body []byte) string {
	if m := ymOgTitleRe.FindSubmatch(body); len(m) == 2 {
		if name := ymCleanSellerName(string(m[1])); name != "" {
			return name
		}
	}
	if m := ymH1Re.FindSubmatch(body); len(m) == 2 {
		if name := ymCleanSellerName(ymTagRe.ReplaceAllString(string(m[1]), "")); name != "" {
			return name
		}
	}
	return ""
}

func ymCleanSellerName(s string) string {
	if parts := ymNameSepRe.Split(s, 2); len(parts) > 0 {
		s = parts[0]
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "Яндекс Маркет") {
		return ""
	}
	return s
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

// maxYandexPages — потолок страниц пагинации (&page=N) на один скрейп. Каждая
// страница — отдельный SSR-фетч (~2.5 МБ), но YM идёт direct/безлимит. Цикл всё
// равно раньше упрётся в maxItems (SEARCH_MAX_ITEMS_YANDEX, по умолч. 60) или в
// страницу без новых товаров.
const maxYandexPages = 5

// ScrapeSearch — забрать выдачу постранично (&page=1..maxYandexPages), копя товары
// с дедупом по ArticleID до maxItems / пустой страницы / потолка страниц.
//
// Витрину продавца тянем через search-форму (/search?generalContext=merchant), а
// НЕ через /business--*/<id>: business флапает в «лёгкий» SSR без моделей и
// одностраничен, тогда как search-форма стабильна и пагинируется по &page (тот же
// /search SSR, что текстовый поиск). Имя магазина (SellerName) по-прежнему берётся
// из business-страницы — там <h1> есть и в лёгком рендере. Цены — в КОПЕЙКАХ.
func (s *YandexMarketSearchScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	if s.YandexMarketScraper == nil || s.direct == nil {
		return nil, fmt.Errorf("%w: yandex search scraper not configured", ErrNotImplemented)
	}

	// База для пагинации: для витрины — search-форма по id продавца, иначе сама
	// ссылка выдачи (/search?text=...).
	base := rawURL
	if u, perr := url.Parse(rawURL); perr == nil {
		if id := ymSellerID(u); id != "" {
			base = ymSellerSearchURL(id)
		}
	}

	// Тот же транспорт direct+proxy-fallback, что у карточки (общий jar).
	header := fhttp.Header{
		"accept":             {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"accept-language":    {"ru,en;q=0.9"},
		"user-agent":         {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"},
		fhttp.HeaderOrderKey: {"accept", "accept-language", "user-agent"},
	}

	out := &SearchResultSet{}
	seen := make(map[string]bool)
	var lastStatus int
	var lastBody []byte
	for page := 1; page <= maxYandexPages; page++ {
		if err := s.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		status, body, _, err := s.getWithFallback(ctx, ymWithPage(base, page), header, 8<<20)
		if err != nil {
			if page == 1 {
				return nil, fmt.Errorf("yandex search request: %w", err)
			}
			break // частичный результат — отдаём что набрали
		}
		lastStatus, lastBody = status, body
		if isYandexCaptcha(body) {
			if page == 1 {
				return nil, ErrMarketplaceBlocked
			}
			break
		}
		added := 0
		for _, it := range s.parseSearch(string(body)).Items {
			if seen[it.ArticleID] {
				continue
			}
			seen[it.ArticleID] = true
			it.Position = len(out.Items) + 1
			out.Items = append(out.Items, it)
			added++
			if len(out.Items) >= s.maxItems {
				break
			}
		}
		out.PagesRead = page
		if added == 0 || len(out.Items) >= s.maxItems {
			break // конец выдачи (повтор/пусто) или достигли лимита
		}
	}
	out.TotalFound = len(out.Items)

	if len(out.Items) == 0 {
		// Диагностика для доводки парсера по прод-логам (как у карточки).
		s.log.Warn("yandex search: no items parsed",
			"url", base, "status", lastStatus, "len", len(lastBody),
			"price_hits", len(ymSearchPriceRe.FindAllStringIndex(string(lastBody), -1)),
			"price_ctx", ymPriceContext(lastBody),
			"cur_ctx", ymCurrencyContext(lastBody))
		return out, ErrParseFailed
	}
	s.log.Info("yandex search scraped", "url", base, "items", len(out.Items), "pages", out.PagesRead)
	return out, nil
}

// ymWithPage — добавить/заменить &page=N в URL выдачи (сохраняя остальные query,
// включая generalContext витрины). При ошибке парса возвращает base как есть.
func ymWithPage(base string, page int) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()
	return u.String()
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
