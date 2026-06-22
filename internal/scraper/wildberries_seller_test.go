package scraper

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func newTestSellerScraper() *WildberriesSellerScraper {
	return NewWildberriesSellerScraper(nil, 5, 0)
}

func TestMatchesSeller(t *testing.T) {
	s := newTestSellerScraper()
	cases := []struct {
		url  string
		want bool
	}{
		{"https://www.wildberries.ru/seller/250000206", true},
		{"https://www.wildberries.ru/seller/250000206?sort=popular&xsubject=515&fbrand=6049", true},
		{"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone", false}, // текстовый поиск
		{"https://www.wildberries.ru/catalog/268509658/detail.aspx", false},       // карточка товара
		{"https://ozon.ru/seller/123", false},                                     // другой маркетплейс
		{"not a url at all", false},
	}
	for _, c := range cases {
		if got := s.MatchesSearch(c.url); got != c.want {
			t.Errorf("MatchesSearch(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestParseSellerParams(t *testing.T) {
	s := newTestSellerScraper()
	raw := "https://www.wildberries.ru/seller/250000206?sort=popular&page=3&dest=-1257786&spp=30" +
		"&xsubject=515&fbrand=6049&f5023=2532298030;1345910513;2546296969&tb_q=iPhone%2017"

	id, filters, text, err := s.parseSellerParams(raw)
	if err != nil {
		t.Fatalf("parseSellerParams: %v", err)
	}
	if id != "250000206" {
		t.Errorf("supplier id = %q, want 250000206", id)
	}
	// Значимые фильтры сохранены.
	if filters.Get("xsubject") != "515" || filters.Get("fbrand") != "6049" {
		t.Errorf("фильтры потеряны: %v", filters)
	}
	// Значения мультизначного фильтра отсортированы.
	if got := filters.Get("f5023"); got != "1345910513;2532298030;2546296969" {
		t.Errorf("f5023 не отсортирован: %q", got)
	}
	// sort — значимый фильтр, сохраняется.
	if filters.Get("sort") != "popular" {
		t.Errorf("sort потерян: %v", filters)
	}
	// Навигационный шум отброшен.
	if filters.Has("page") || filters.Has("dest") || filters.Has("spp") {
		t.Errorf("навигационный шум не отброшен: %v", filters)
	}
	// Клиентский текст нормализован (lower, trim).
	if text != "iphone 17" {
		t.Errorf("text = %q, want %q", text, "iphone 17")
	}
}

func TestParseSellerParams_NotSeller(t *testing.T) {
	s := newTestSellerScraper()
	if _, _, _, err := s.parseSellerParams("https://www.wildberries.ru/catalog/0/search.aspx?search=x"); err == nil {
		t.Error("ожидалась ошибка для не-seller URL")
	}
}

func TestNormalizeSellerURL(t *testing.T) {
	s := newTestSellerScraper()

	// Семантически одинаковые ссылки → один ключ (разный порядок параметров,
	// навигационный шум, порядок значений в f5023).
	equivalent := []string{
		"https://www.wildberries.ru/seller/250000206?xsubject=515&fbrand=6049&f5023=1345910513;2532298030;2546296969",
		"https://www.wildberries.ru/seller/250000206?page=2&dest=-1257786&fbrand=6049&xsubject=515&f5023=2546296969;1345910513;2532298030&spp=30",
	}
	var first string
	for i, u := range equivalent {
		got, err := s.NormalizeSearchURL(u)
		if err != nil {
			t.Fatalf("NormalizeSearchURL(%q): %v", u, err)
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("normalized mismatch:\n  %q ->\n  %q\nwant\n  %q", u, got, first)
		}
	}

	// Разные фильтры → разные ключи.
	a, _ := s.NormalizeSearchURL("https://www.wildberries.ru/seller/100?xsubject=515")
	b, _ := s.NormalizeSearchURL("https://www.wildberries.ru/seller/100?xsubject=999")
	if a == b {
		t.Errorf("ожидались разные ключи для разных фильтров, оба = %q", a)
	}

	// Разный клиентский текст → разные ключи.
	c, _ := s.NormalizeSearchURL("https://www.wildberries.ru/seller/100?tb_q=iphone")
	d, _ := s.NormalizeSearchURL("https://www.wildberries.ru/seller/100?tb_q=samsung")
	if c == d {
		t.Errorf("ожидались разные ключи для разного текста, оба = %q", c)
	}

	// Разные продавцы → разные ключи.
	e, _ := s.NormalizeSearchURL("https://www.wildberries.ru/seller/100")
	f, _ := s.NormalizeSearchURL("https://www.wildberries.ru/seller/200")
	if e == f {
		t.Errorf("ожидались разные ключи для разных продавцов, оба = %q", e)
	}
}

func TestBuildSellerAPIURL(t *testing.T) {
	s := newTestSellerScraper()
	filters := url.Values{"xsubject": {"515"}, "fbrand": {"6049"}}
	got := s.buildSellerAPIURL("250000206", filters, 2)
	for _, want := range []string{
		wbSellerAPIBase,
		"supplier=250000206",
		"page=2",
		"xsubject=515",
		"fbrand=6049",
		"dest=-1257786",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("API URL %q не содержит %q", got, want)
		}
	}
}

func TestSellerTextMatches(t *testing.T) {
	cases := []struct {
		name, brand, text string
		want              bool
	}{
		{"Смартфон Apple iPhone 17 128GB", "Apple", "iphone 17", true},
		{"iPhone 17 Pro Max", "Apple", "iphone 17", true},
		{"Чехол для iPhone 16", "X", "iphone 17", false}, // нет «17»
		{"Apple Watch", "Apple", "iphone", false},        // нет «iphone»
		{"Тренчкот", "ELSY", "elsy", true},               // матч по бренду
		{"Любой товар", "Любой", "", true},               // пустой фильтр — пропускает всё
	}
	for _, c := range cases {
		it := SearchItem{Name: c.name, Brand: c.brand}
		if got := sellerTextMatches(it, textTokens(c.text)); got != c.want {
			t.Errorf("sellerTextMatches(name=%q brand=%q text=%q) = %v, want %v",
				c.name, c.brand, c.text, got, c.want)
		}
	}
}

// ── Логика пагинации/фильтрации (сетевой слой подменён фейком) ────────────────
//
// Сеть к loopback в тестовой среде недоступна (нет httptest), поэтому fetch
// подменяем стабом: проверяем логику ScrapeSearch/SellerTotal герметично.

// sellerPageJSON — страница ответа seller-catalog v4 (форма как у u-search).
func sellerPageJSON(total int, products string) string {
	return fmt.Sprintf(`{"total":%d,"products":[%s]}`, total, products)
}

// stubFetch — fetch-функция, отдающая заранее заданное тело по номеру страницы
// (из ?page= в URL). Порядок запрошенных страниц пишется в pages (если не nil).
func stubFetch(byPage map[string]string, pages *[]string) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, apiURL string) ([]byte, error) {
		u, _ := url.Parse(apiURL)
		p := u.Query().Get("page")
		if pages != nil {
			*pages = append(*pages, p)
		}
		body, ok := byPage[p]
		if !ok {
			return nil, fmt.Errorf("no stub for page %q", p)
		}
		return []byte(body), nil
	}
}

const (
	prodApple17 = `{"id":111,"name":"Смартфон Apple iPhone 17 128GB","brand":"Apple","sizes":[{"price":{"basic":12000000,"product":9990000}}]}`
	prodApple16 = `{"id":222,"name":"Apple iPhone 16","brand":"Apple","sizes":[{"price":{"basic":9000000,"product":7990000}}]}`
	prodZero    = `{"id":333,"name":"Нет цены","brand":"X","sizes":[{"price":{"basic":0,"product":0}}]}`
)

func TestScrapeSearch_FilterAndZeroPrice(t *testing.T) {
	s := NewWildberriesSellerScraper(nil, 1, 0)
	// total > 0, страница со смесью: нужный, не подходящий по тексту, нулевой.
	s.fetch = stubFetch(map[string]string{
		"1": sellerPageJSON(3, strings.Join([]string{prodApple17, prodApple16, prodZero}, ",")),
	}, nil)

	out, err := s.ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/seller/100?tb_q=iphone+17")
	if err != nil {
		t.Fatalf("ScrapeSearch: %v", err)
	}
	if out.TotalFound != 3 {
		t.Errorf("TotalFound = %d, want 3", out.TotalFound)
	}
	// Остаётся только iPhone 17 (16 отсеян текстом, нулевой — ценой).
	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1: %+v", len(out.Items), out.Items)
	}
	if out.Items[0].ArticleID != "111" {
		t.Errorf("оставлен не тот товар: %+v", out.Items[0])
	}
	if out.Items[0].Position != 1 {
		t.Errorf("position = %d, want 1", out.Items[0].Position)
	}
}

func TestScrapeSearch_Pagination(t *testing.T) {
	// Полная страница (100 шт) → читаем дальше; неполная → стоп. Эмулируем 2
	// полные страницы и 3-ю неполную; maxPages=5 не должен дочитывать до 5.
	full := make([]string, wbSellerPageSize)
	for i := range full {
		full[i] = fmt.Sprintf(`{"id":%d,"name":"Товар","brand":"B","sizes":[{"price":{"basic":200,"product":100}}]}`, 1000+i)
	}
	fullPage := strings.Join(full, ",")
	tail := `{"id":9001,"name":"Хвост","brand":"B","sizes":[{"price":{"basic":200,"product":100}}]}`

	var pages []string
	s := NewWildberriesSellerScraper(nil, 5, 0)
	s.fetch = stubFetch(map[string]string{
		"1": sellerPageJSON(201, fullPage),
		"2": sellerPageJSON(201, fullPage),
		"3": sellerPageJSON(201, tail),
	}, &pages)

	out, err := s.ScrapeSearch(context.Background(), "https://www.wildberries.ru/seller/100")
	if err != nil {
		t.Fatalf("ScrapeSearch: %v", err)
	}
	if out.PagesRead != 3 {
		t.Errorf("PagesRead = %d, want 3 (стоп на неполной странице)", out.PagesRead)
	}
	if want := 2*wbSellerPageSize + 1; len(out.Items) != want {
		t.Errorf("items = %d, want %d", len(out.Items), want)
	}
	if strings.Join(pages, ",") != "1,2,3" {
		t.Errorf("запрошены страницы %v, want [1 2 3]", pages)
	}
}

func TestSellerTotal(t *testing.T) {
	var pages []string
	s := NewWildberriesSellerScraper(nil, 5, 0)
	s.fetch = stubFetch(map[string]string{"1": sellerPageJSON(42033, prodApple17)}, &pages)

	total, err := s.SellerTotal(context.Background(), "https://www.wildberries.ru/seller/250000206")
	if err != nil {
		t.Fatalf("SellerTotal: %v", err)
	}
	if total != 42033 {
		t.Errorf("total = %d, want 42033", total)
	}
	// Только одна страница — гейт дешёвый.
	if len(pages) != 1 || pages[0] != "1" {
		t.Errorf("ожидался один запрос page=1, got %v", pages)
	}
}

func TestScrapeSearch_FetchErrorOnFirstPage(t *testing.T) {
	s := NewWildberriesSellerScraper(nil, 5, 0)
	s.fetch = func(context.Context, string) ([]byte, error) { return nil, ErrMarketplaceBlocked }

	if _, err := s.ScrapeSearch(context.Background(), "https://www.wildberries.ru/seller/100"); err == nil {
		t.Error("ожидалась ошибка при сбое на первой странице")
	}
}

func TestScrapeSearch_PartialOnLatePageError(t *testing.T) {
	// Сбой на 2-й странице после успешной 1-й → частичный результат, без ошибки.
	full := make([]string, wbSellerPageSize)
	for i := range full {
		full[i] = fmt.Sprintf(`{"id":%d,"name":"Товар","brand":"B","sizes":[{"price":{"basic":200,"product":100}}]}`, 1000+i)
	}
	s := NewWildberriesSellerScraper(nil, 5, 0)
	s.fetch = stubFetch(map[string]string{"1": sellerPageJSON(500, strings.Join(full, ","))}, nil) // page 2 → no stub → ошибка

	out, err := s.ScrapeSearch(context.Background(), "https://www.wildberries.ru/seller/100")
	if err != nil {
		t.Fatalf("частичный результат не должен возвращать ошибку: %v", err)
	}
	if out.PagesRead != 1 || len(out.Items) != wbSellerPageSize {
		t.Errorf("PagesRead=%d items=%d; хотим 1/%d", out.PagesRead, len(out.Items), wbSellerPageSize)
	}
}

func TestMaxItems(t *testing.T) {
	if got := NewWildberriesSellerScraper(nil, 5, 0).MaxItems(); got != 500 {
		t.Errorf("MaxItems = %d, want 500", got)
	}
	if got := NewWildberriesSellerScraper(nil, 3, 0).MaxItems(); got != 300 {
		t.Errorf("MaxItems = %d, want 300", got)
	}
}
