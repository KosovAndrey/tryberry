package scraper

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// AliexpressSearchScraper — скрейпер поисковой выдачи aliexpress.ru по ссылке вида
// aliexpress.ru/wholesale?SearchText=... . Транспорт переиспользуем у карточного
// AliexpressScraper (direct+proxy-fallback с общим cookie-jar, см. aliexpress.go).
//
// СТАТУС: first-pass. MatchesSearch/NormalizeSearchURL готовы и покрыты тестами,
// а ScrapeSearch ещё не реализован — где aliexpress.ru отдаёт позиции выдачи
// (SSR-стейт как у Я.Маркета или отдельный XHR aer-api) НЕ подтверждено
// эмпирически. Структуру снимаем probe'ом на проде (cmd/ali-search-probe), затем
// пишем парсер. Поэтому скрейпер пока НЕ регистрируется в реестре.
type AliexpressSearchScraper struct {
	*AliexpressScraper
	maxItems int
}

var _ SearchScraper = (*AliexpressSearchScraper)(nil)

// NewAliexpressSearchScraper оборачивает карточный скрейпер (переиспользуем его
// tls-client'ы/прокси/jar). maxItems<=0 → 60.
func NewAliexpressSearchScraper(base *AliexpressScraper, maxItems int) *AliexpressSearchScraper {
	if maxItems <= 0 {
		maxItems = 60
	}
	return &AliexpressSearchScraper{AliexpressScraper: base, maxItems: maxItems}
}

// MatchesSearch — поисковая ссылка aliexpress.ru: путь /wholesale с параметром
// SearchText, либо /w/wholesale-<text>.html. Карточка (/item/<id>.html) сюда НЕ
// попадает — её разбирает обычный Scrape.
func (s *AliexpressSearchScraper) MatchesSearch(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if !isAliHost(u.Host) {
		return false
	}
	if strings.Contains(u.Path, "/item/") {
		return false
	}
	if strings.HasPrefix(u.Path, "/wholesale") && strings.TrimSpace(u.Query().Get("SearchText")) != "" {
		return true
	}
	return strings.HasPrefix(u.Path, "/w/wholesale-") && strings.HasSuffix(u.Path, ".html")
}

// NormalizeSearchURL — канонический ключ дедупликации.
//
// FIRST-PASS: ключ = SearchText (нормализованный). Фильтры (размер/цвет/пол)
// aliexpress.ru кодирует в pvid/searchInfo — это, судя по виду, волатильные
// токены (вероятно протухают), поэтому в ключ их пока НЕ включаем: две
// «футболки» с разными фильтрами схлопнутся в одну подписку. Подтвердить
// стабильность фильтр-параметров и доработать ключ — после probe (как hid у YM).
func (s *AliexpressSearchScraper) NormalizeSearchURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	text := aliSearchText(u)
	if text == "" {
		return "", fmt.Errorf("%w: no SearchText in aliexpress search URL", ErrInvalidURL)
	}
	text = strings.Join(strings.Fields(strings.ToLower(text)), " ")
	canon := url.Values{}
	canon.Set("SearchText", text)
	return aliBaseURL + "/wholesale?" + canon.Encode(), nil
}

// ScrapeSearch — TODO: реализовать после probe (cmd/ali-search-probe) на проде.
func (s *AliexpressSearchScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	return nil, fmt.Errorf("%w: aliexpress search not implemented yet", ErrNotImplemented)
}

// aliSearchText достаёт текст запроса из /wholesale?SearchText=... или из
// /w/wholesale-<text>.html (URL-декодированный).
func aliSearchText(u *url.URL) string {
	if t := strings.TrimSpace(u.Query().Get("SearchText")); t != "" {
		return t
	}
	if strings.HasPrefix(u.Path, "/w/wholesale-") && strings.HasSuffix(u.Path, ".html") {
		raw := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/w/wholesale-"), ".html")
		if dec, err := url.PathUnescape(raw); err == nil {
			return strings.TrimSpace(dec)
		}
		return strings.TrimSpace(raw)
	}
	return ""
}

func isAliHost(host string) bool {
	h := strings.ToLower(host)
	return strings.Contains(h, "aliexpress.ru") || strings.Contains(h, "aliexpress.com")
}
