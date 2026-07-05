package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

// AliexpressSearchScraper — скрейпер поисковой выдачи aliexpress.ru по ссылке вида
// aliexpress.ru/wholesale?SearchText=... . Транспорт переиспользуем у карточного
// AliexpressScraper (direct+proxy-fallback с общим cookie-jar, см. aliexpress.go).
//
// Выдача грузится XHR'ом: POST /aer-webapi/v1/search (JSON-API, не SSR), товары
// в data.productsFeed.productsV2[].snippetContainer.itemData. Подтверждено
// probe'ом (cmd/ali-search-probe): API отдаёт результаты direct, с минимальным
// телом (searchText/page), без волатильного searchInfo.
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

const aliSearchAPI = aliBaseURL + "/aer-webapi/v1/search"

// maxAliSearchPages — потолок страниц (по 20 позиций), чтобы не уходить в цикл.
const maxAliSearchPages = 5

// ScrapeSearch — выдача через JSON-API POST /aer-webapi/v1/search (тот же
// транспорт, что у карточки: direct + proxy-fallback на X5SEC, общий jar с
// прогретой aer-cookie). Тело минимальное (searchText/page) — без волатильного
// searchInfo: probe подтвердил, что API так отдаёт результаты. Пагинация по page,
// частичный результат при сбое на поздних страницах допустим.
func (s *AliexpressSearchScraper) ScrapeSearch(ctx context.Context, rawURL string) (*SearchResultSet, error) {
	if s.AliexpressScraper == nil || !s.configured {
		return nil, fmt.Errorf("%w: aliexpress search not configured (no proxy)", ErrNotImplemented)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	text := aliSearchText(u)
	if text == "" {
		return nil, fmt.Errorf("%w: no SearchText", ErrInvalidURL)
	}
	referer := aliBaseURL + "/wholesale?SearchText=" + url.QueryEscape(text)

	out := &SearchResultSet{}
	seen := make(map[string]bool)
	for page := 1; page <= maxAliSearchPages; page++ {
		if err := s.limiter.Wait(ctx); err != nil {
			return out, err
		}
		status, body, err := s.searchWithFallback(ctx, aliSearchBody(text, page), referer)
		if err != nil {
			if len(out.Items) > 0 {
				break // частичный результат сохраняем
			}
			return nil, fmt.Errorf("aliexpress search request: %w", err)
		}
		if isAliBlocked(body) {
			if len(out.Items) > 0 {
				break
			}
			s.log.Warn("aliexpress search: X5SEC block", "text", text, "page", page, "body", snippet(body, 160))
			return nil, ErrMarketplaceBlocked
		}
		if status != 200 {
			if len(out.Items) > 0 {
				break
			}
			return nil, fmt.Errorf("aliexpress search status %d", status)
		}
		items, perr := parseAliexpressSearch(body)
		if perr != nil {
			if len(out.Items) > 0 {
				break
			}
			s.log.Warn("aliexpress search: parse failed", "text", text, "page", page, "len", len(body))
			return out, ErrParseFailed
		}
		added := 0
		for _, it := range items {
			if it.ArticleID == "" || seen[it.ArticleID] {
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
			break
		}
	}
	s.log.Info("aliexpress search scraped", "text", text, "items", len(out.Items), "pages", out.PagesRead)
	return out, nil
}

// aliSearchBody — минимальное тело запроса выдачи (см. probe). source=direct,
// g=y как у веб-фронта; фильтры/searchInfo не передаём.
func aliSearchBody(text string, page int) []byte {
	payload := map[string]interface{}{
		"page":          page,
		"searchText":    text,
		"source":        "direct",
		"g":             "y",
		"catId":         "",
		"storeIds":      []string{},
		"pgChildren":    []interface{}{},
		"aeBrainIds":    []interface{}{},
		"mainFilters":   "",
		"searchTrigger": "search_bar",
	}
	b, _ := json.Marshal(payload)
	return b
}

// searchWithFallback — POST выдачи: direct, при X5SEC/не-200 один проход через
// прокси (обновляет aer-cookie в общем jar). Зеркало AliexpressScraper.Scrape.
func (s *AliexpressSearchScraper) searchWithFallback(ctx context.Context, body []byte, referer string) (int, []byte, error) {
	status, b, err := s.fetchSearch(ctx, s.direct, body, referer)
	if err != nil {
		return 0, nil, err
	}
	// Холодная сессия: X5SEC иногда отдаёт 200 без товаров и без явного блок-маркера
	// (cookie ещё не прогрета) — отсутствие snippetContainer трактуем как cold и
	// идём через прокси (он проходит антибот и кладёт aer-cookie в общий jar).
	// В direct-only режиме (proxy=nil) фолбэка нет — отдаём direct-ответ как есть,
	// блок/пустую выдачу разберёт вызывающий код (как у карточного Scrape).
	if (status != 200 || isAliBlocked(b) || !aliHasProducts(b)) && s.proxy != nil {
		return s.fetchSearch(ctx, s.proxy, body, referer)
	}
	return status, b, nil
}

// aliHasProducts — быстрый признак непустой выдачи (без полного парса).
func aliHasProducts(b []byte) bool {
	return strings.Contains(string(b), `"snippetContainer"`)
}

func (s *AliexpressSearchScraper) fetchSearch(ctx context.Context, client tls_client.HttpClient, body []byte, referer string) (int, []byte, error) {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodPost, aliSearchAPI, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, err
	}
	req.Header = fhttp.Header{
		"accept":           {"application/json, text/plain, */*"},
		"accept-language":  {"ru,en;q=0.9"},
		"bx-v":             {"2.5.36"},
		"content-type":     {"application/json"},
		"origin":           {aliBaseURL},
		"referer":          {referer},
		"x-requested-with": {"XMLHttpRequest"},
		"user-agent":       {aliUserAgent},
		fhttp.HeaderOrderKey: {
			"accept", "accept-language", "bx-v", "content-type", "origin",
			"referer", "x-requested-with", "user-agent",
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("aliexpress search: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	return resp.StatusCode, b, nil
}

// ── Парсинг ответа /aer-webapi/v1/search ─────────────────────────────────────

type aliSearchResponse struct {
	Data struct {
		ProductsFeed struct {
			ProductsV2 []struct {
				SnippetContainer struct {
					ItemData struct {
						PdpInfo struct {
							PreloadedData struct {
								Title string `json:"title"`
								Price struct {
									Value float64 `json:"value"`
								} `json:"price"`
							} `json:"preloadedData"`
						} `json:"pdpInfo"`
						Properties struct {
							ID string `json:"id"`
						} `json:"properties"`
						TrackingInfo struct {
							WebTrackInfo struct {
								AerEvent struct {
									ItemID     string  `json:"itemId"`
									FinalPrice float64 `json:"finalPrice"`
									Price      float64 `json:"price"`
								} `json:"aerEvent"`
							} `json:"webTrackInfo"`
						} `json:"trackingInfo"`
					} `json:"itemData"`
					// Презентационное дерево — оттуда вытаскиваем картинку (в
					// preloadedData.imageUrl приходит битый URL без CDN-хоста).
					Presentations json.RawMessage `json:"presentations"`
				} `json:"snippetContainer"`
			} `json:"productsV2"`
		} `json:"productsFeed"`
	} `json:"data"`
}

// aliMediaImgRe — полный CDN-URL картинки в presentations (gallery.images).
var aliMediaImgRe = regexp.MustCompile(`https://[a-z0-9.\-]+aliexpress-media\.com/kf/[A-Za-z0-9]+\.(?:jpg|png|webp)[^"]*`)

func parseAliexpressSearch(body []byte) ([]SearchItem, error) {
	var resp aliSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%w: aliexpress search json: %v", ErrParseFailed, err)
	}
	items := make([]SearchItem, 0, len(resp.Data.ProductsFeed.ProductsV2))
	for _, p := range resp.Data.ProductsFeed.ProductsV2 {
		d := p.SnippetContainer.ItemData
		ev := d.TrackingInfo.WebTrackInfo.AerEvent
		id := d.Properties.ID
		if id == "" {
			id = ev.ItemID
		}
		if id == "" {
			continue
		}
		// Цена покупателя: finalPrice (аналитика) → preloadedData.price.value.
		final := ev.FinalPrice
		if final <= 0 {
			final = d.PdpInfo.PreloadedData.Price.Value
		}
		if final <= 0 {
			continue // без цены позиция бесполезна
		}
		name := strings.TrimSpace(d.PdpInfo.PreloadedData.Title)
		if name == "" {
			name = "Товар AliExpress"
		}
		it := SearchItem{
			ArticleID:    id,
			Name:         name,
			URL:          aliBaseURL + "/item/" + id + ".html",
			ImageURL:     aliMediaImgRe.FindString(string(p.SnippetContainer.Presentations)),
			PriceKopecks: int64(final*100 + 0.5),
		}
		if ev.Price > final {
			it.OldPriceKopecks = int64(ev.Price*100 + 0.5)
		}
		items = append(items, it)
	}
	return items, nil
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
