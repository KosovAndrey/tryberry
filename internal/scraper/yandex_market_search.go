package scraper

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
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

// MatchesSearch — ссылка на выдачу Я.Маркета: market.yandex.ru с /search в пути
// или параметром text. Карточка (market.yandex.ru/card/...) сюда НЕ попадает —
// её разбирает обычный Scrape.
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
	if s.YandexMarketScraper == nil || s.client == nil {
		return nil, fmt.Errorf("%w: yandex search scraper not configured", ErrNotImplemented)
	}
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = fhttp.Header{
		"accept":             {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"accept-language":    {"ru,en;q=0.9"},
		"user-agent":         {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"},
		fhttp.HeaderOrderKey: {"accept", "accept-language", "user-agent"},
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("yandex search request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if isYandexCaptcha(body) {
		return nil, ErrMarketplaceBlocked
	}

	// Диагностический дамп сырого HTML: путь задаётся env YM_SEARCH_DUMP (по
	// умолчанию выкл). Нужен, чтобы один раз снять реальную страницу выдачи с
	// прод-прокси и вскрыть структуру стейта marketfront (имя/URL/артикул/
	// картинка) — локально страница недоступна (антибот рубит датацентровый IP).
	// Перезаписывает файл, держим только последний захват. Удалить после доводки.
	if dump := strings.TrimSpace(os.Getenv("YM_SEARCH_DUMP")); dump != "" {
		if err := os.WriteFile(dump, body, 0o644); err != nil {
			s.log.Warn("yandex search: dump write failed", "path", dump, "err", err)
		} else {
			s.log.Info("yandex search: raw html dumped", "path", dump, "len", len(body))
		}
	}

	out := s.parseSearch(string(body))
	if len(out.Items) == 0 {
		// Диагностика для доводки парсера по прод-логам (как у карточки).
		s.log.Warn("yandex search: no items parsed",
			"url", rawURL, "status", resp.StatusCode, "len", len(body),
			"price_hits", len(ymSearchPriceRe.FindAllStringIndex(string(body), -1)),
			"price_ctx", ymPriceContext(body),
			"cur_ctx", ymCurrencyContext(body))
		return out, ErrParseFailed
	}
	s.log.Info("yandex search scraped", "url", rawURL, "items", len(out.Items))
	return out, nil
}

// parseSearch — FIRST-PASS извлечение позиций выдачи из SSR-стейта.
//
// ВАЖНО: пока парсер НЕ извлекает идентичность товара (URL/артикул) — только
// цены. Эмитить такие позиции в пайплайн НЕЛЬЗЯ: products апсертится по URL
// (ON CONFLICT (url)), и все позиции с пустым URL схлопываются в один товар →
// baseline/last_notified считаются по одному фантому, current «прыгает» между
// циклами и Decide бесконечно шлёт спам (наблюдали на проде 18-Jun: уведомление
// каждую минуту). Поэтому до доводки возвращаем ПУСТОЙ набор: вызывающий получит
// ErrParseFailed (товары не идут в пайплайн), а сырой HTML всё равно дампится в
// ScrapeSearch для вскрытия структуры стейта. Снять гейт, как только ниже будет
// извлекаться per-item URL/Name/ArticleID/ImageURL.
func (s *YandexMarketSearchScraper) parseSearch(html string) *SearchResultSet {
	// TODO(prod-logs): извлечь Name/URL/ArticleID/ImageURL из стейта marketfront
	// и собрать out.Items, только когда у позиции есть непустой URL.
	return &SearchResultSet{}
}
