package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// rtFunc — RoundTripper из функции (канонические ответы без сети).
type rtFunc func(*http.Request) *http.Response

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

func stubResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     make(http.Header),
	}
}

// TestScrapeSearchFallbackOn403 — direct отдаёт 403 (горячий запрос), фолбэк-прокси
// отдаёт 200: выдача должна собраться через прокси, direct не должен ронять запрос.
func TestScrapeSearchFallbackOn403(t *testing.T) {
	var directHits, proxyHits int
	directClient := &http.Client{Transport: rtFunc(func(*http.Request) *http.Response {
		directHits++
		return stubResp(http.StatusForbidden, "blocked")
	})}
	proxyCl := &http.Client{Transport: rtFunc(func(*http.Request) *http.Response {
		proxyHits++
		return stubResp(http.StatusOK, sampleSearchJSON)
	})}

	s := &WildberriesSearchScraper{
		WildberriesScraper: NewWildberriesScraper(5),
		pool:               &ProxyPool{clients: []proxyClient{{label: "direct", client: directClient}}},
		tokens:             StaticTokenProvider{T: SearchToken{Cookie: "x_wbaas_token=abc", Slot: -1}},
		maxPages:           1,
		fallbackClient:     proxyCl,
	}

	set, err := s.ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone")
	if err != nil {
		t.Fatalf("ScrapeSearch с рабочим фолбэком вернул ошибку: %v", err)
	}
	if len(set.Items) != 2 {
		t.Fatalf("собрано %d товаров, want 2 (через прокси-фолбэк)", len(set.Items))
	}
	if directHits == 0 || proxyHits == 0 {
		t.Errorf("ожидались обращения и к direct (403), и к прокси (200): direct=%d proxy=%d", directHits, proxyHits)
	}
}

// TestScrapeSearchNoFallbackReturnsBlocked — без фолбэка 403 остаётся ошибкой.
func TestScrapeSearchNoFallbackReturnsBlocked(t *testing.T) {
	directClient := &http.Client{Transport: rtFunc(func(*http.Request) *http.Response {
		return stubResp(http.StatusForbidden, "blocked")
	})}
	s := &WildberriesSearchScraper{
		WildberriesScraper: NewWildberriesScraper(5),
		pool:               &ProxyPool{clients: []proxyClient{{label: "direct", client: directClient}}},
		tokens:             StaticTokenProvider{T: SearchToken{Cookie: "x_wbaas_token=abc", Slot: -1}},
		maxPages:           1,
	}
	if _, err := s.ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone"); err == nil {
		t.Fatal("ожидалась ошибка (403 без фолбэка), получили nil")
	}
}

func newTestSearchScraper() *WildberriesSearchScraper {
	return NewWildberriesSearchScraper(nil, nil, nil, 5, 0)
}

func TestMatchesSearch(t *testing.T) {
	s := newTestSearchScraper()
	cases := []struct {
		url  string
		want bool
	}{
		{"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone%2016", true},
		{"https://www.wildberries.ru/catalog/0/search.aspx?page=1&sort=popular&search=iphone+16", true},
		{"https://www.wildberries.ru/catalog/268509658/detail.aspx", false}, // карточка товара
		{"https://www.wildberries.ru/catalog/elektronika/smartfony", false}, // категория без search
		{"https://ozon.ru/search?text=iphone", false},                       // другой маркетплейс
		{"not a url at all", false},
	}
	for _, c := range cases {
		if got := s.MatchesSearch(c.url); got != c.want {
			t.Errorf("MatchesSearch(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestNormalizeSearchURL(t *testing.T) {
	s := newTestSearchScraper()

	// Разные по форме, но семантически одинаковые ссылки → один ключ.
	equivalent := []string{
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iPhone%2016",
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone+16&page=3&dest=-1257786&spp=30",
		"https://www.wildberries.ru/catalog/0/search.aspx?search=  iphone   16  &sort=popular",
	}
	var first string
	for i, u := range equivalent {
		got, err := s.NormalizeSearchURL(u)
		if err != nil {
			t.Fatalf("NormalizeSearchURL(%q) error: %v", u, err)
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("normalized mismatch:\n  %q ->\n  %q\nwant\n  %q", u, got, first)
		}
	}

	// Разная сортировка → разные ключи.
	a, _ := s.NormalizeSearchURL("https://www.wildberries.ru/catalog/0/search.aspx?search=iphone&sort=popular")
	b, _ := s.NormalizeSearchURL("https://www.wildberries.ru/catalog/0/search.aspx?search=iphone&sort=pricedown")
	if a == b {
		t.Errorf("ожидались разные ключи для разных sort, оба = %q", a)
	}

	// Без текста запроса → ошибка.
	if _, err := s.NormalizeSearchURL("https://www.wildberries.ru/catalog/0/search.aspx"); err == nil {
		t.Error("ожидалась ошибка для URL без search=")
	}
}

func TestBuildSearchAPIURL(t *testing.T) {
	got := buildSearchAPIURL("iphone 16", "popular", 2)
	for _, want := range []string{
		wbSearchAPIBase,
		"query=iphone+16",
		"page=2",
		"resultset=catalog",
		"sort=popular",
		"dest=-1257786",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("API URL %q не содержит %q", got, want)
		}
	}
}

// Реальная форма ответа v18, снятая с сервера (цены в копейках, products в корне).
const sampleSearchJSON = `{
  "total": 1234,
  "products": [
    {"id":387704100,"name":"iPhone 16 256GB","brand":"Apple","feedbackPoints":0,
     "sizes":[{"price":{"basic":14199000,"product":6673500}}]},
    {"id":314263942,"name":"iPhone 16 128GB","brand":"Apple","feedbackPoints":900,
     "sizes":[{"price":{"basic":12999000,"product":5459500}}]},
    {"id":999999999,"name":"Нет в наличии","brand":"X",
     "sizes":[{"price":{"basic":0,"product":0}}]}
  ]
}`

func TestParseSearchResponse(t *testing.T) {
	var parsed wbSearchResponse
	if err := json.Unmarshal([]byte(sampleSearchJSON), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Total != 1234 {
		t.Errorf("Total = %d, want 1234", parsed.Total)
	}
	if len(parsed.Products) != 3 {
		t.Fatalf("products = %d, want 3", len(parsed.Products))
	}

	// Первый товар: цена product, старая basic, баллов нет.
	it0 := wbProductToItem(parsed.Products[0], 1)
	if it0.PriceKopecks != 6673500 {
		t.Errorf("price = %d, want 6673500", it0.PriceKopecks)
	}
	if it0.OldPriceKopecks != 14199000 {
		t.Errorf("old price = %d, want 14199000", it0.OldPriceKopecks)
	}
	if it0.EffectivePriceKopecks() != 6673500 {
		t.Errorf("effective = %d, want 6673500 (баллов нет)", it0.EffectivePriceKopecks())
	}
	if it0.URL != "https://www.wildberries.ru/catalog/387704100/detail.aspx" {
		t.Errorf("url = %q", it0.URL)
	}
	if !strings.HasPrefix(it0.ImageURL, "https://basket-") || !strings.Contains(it0.ImageURL, "/387704100/") {
		t.Errorf("image url unexpected: %q", it0.ImageURL)
	}

	// Второй товар: 900 баллов → эффективная = product - 900*100.
	it1 := wbProductToItem(parsed.Products[1], 2)
	if !it1.HasFeedbackPoints() {
		t.Error("ожидались баллы у второго товара")
	}
	if want := int64(5459500 - 900*100); it1.EffectivePriceKopecks() != want {
		t.Errorf("effective = %d, want %d", it1.EffectivePriceKopecks(), want)
	}
}

func TestScrapeSearchSkipsZeroPrice(t *testing.T) {
	// Третий товар в сэмпле — нулевая цена; в выдачу попадать не должен.
	var parsed wbSearchResponse
	_ = json.Unmarshal([]byte(sampleSearchJSON), &parsed)

	kept := 0
	pos := 0
	for _, p := range parsed.Products {
		pos++
		if wbProductToItem(p, pos).PriceKopecks == 0 {
			continue
		}
		kept++
	}
	if kept != 2 {
		t.Errorf("оставлено %d товаров, want 2 (нулевая цена отброшена)", kept)
	}
}

func TestProxyPoolRoundRobin(t *testing.T) {
	pool, errs := NewProxyPool([]string{
		"http://u:p@proxy1.example:8080",
		"http://u:p@proxy2.example:8080",
		"  ",        // пустой — пропускается
		"://broken", // битый — в errs
	}, 5*time.Second)

	if len(errs) != 1 {
		t.Errorf("ожидалась 1 ошибка на битый прокси, got %d: %v", len(errs), errs)
	}
	if pool.Size() != 2 {
		t.Fatalf("pool size = %d, want 2", pool.Size())
	}
	if !pool.HasProxies() {
		t.Error("HasProxies() = false, want true")
	}

	// Round-robin: 4 вызова на 2 прокси → чередование a,b,a,b.
	seq := []string{pool.next().label, pool.next().label, pool.next().label, pool.next().label}
	if seq[0] == seq[1] {
		t.Errorf("ожидалось чередование прокси, got %v", seq)
	}
	if seq[0] != seq[2] || seq[1] != seq[3] {
		t.Errorf("ожидался период 2 в round-robin, got %v", seq)
	}
}

func TestProxyPoolEmptyIsDirect(t *testing.T) {
	pool, errs := NewProxyPool(nil, 0)
	if len(errs) != 0 {
		t.Errorf("неожиданные ошибки: %v", errs)
	}
	if pool.Size() != 1 {
		t.Errorf("pool size = %d, want 1 (direct)", pool.Size())
	}
	if pool.HasProxies() {
		t.Error("HasProxies() = true для пустого пула, want false")
	}
	if pool.next().label != "direct" {
		t.Errorf("label = %q, want direct", pool.next().label)
	}
}
