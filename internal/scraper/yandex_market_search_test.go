package scraper

import (
	"errors"
	"net/url"
	"os"
	"testing"
)

func newYMSearch() *YandexMarketSearchScraper {
	// base без прокси: client поднимется (tls-client не требует прокси для init),
	// но сетевых вызовов в этих тестах нет — проверяем только разбор URL/стейта.
	return NewYandexMarketSearchScraper(NewYandexMarketScraper(YandexMarketOptions{}), 60)
}

func TestYandexSearch_MatchesSearch(t *testing.T) {
	s := newYMSearch()
	cases := map[string]bool{
		"https://market.yandex.ru/search?text=кофемашина":           true,
		"https://market.yandex.ru/search?text=кофемашина&hid=90555": true,
		"https://market.yandex.ru/card/kofemashina-jura/5193397317": false, // карточка
		"https://www.wildberries.ru/catalog/0/search.aspx?search=x": false, // другой МП
		"https://market.yandex.ru/product--slug/123":                false, // карточка
	}
	for url, want := range cases {
		if got := s.MatchesSearch(url); got != want {
			t.Errorf("MatchesSearch(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestYandexSearch_NormalizeSearchURL(t *testing.T) {
	s := newYMSearch()
	// text нормализуется (lower + схлопывание пробелов), hid сохраняется. Сравниваем
	// по разобранным параметрам — Encode() percent-кодирует кириллицу.
	got, err := s.NormalizeSearchURL("https://market.yandex.ru/search?text=Кофемашина%20%20JURA&hid=90555&foo=bar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, _ := url.Parse(got)
	if u.Scheme+"://"+u.Host+u.Path != "https://market.yandex.ru/search" {
		t.Errorf("base = %q", u.Scheme+"://"+u.Host+u.Path)
	}
	if q := u.Query(); q.Get("text") != "кофемашина jura" || q.Get("hid") != "90555" || q.Get("foo") != "" {
		t.Errorf("query = %v, want text='кофемашина jura' hid=90555 без foo", u.Query())
	}

	// Без text — ErrInvalidURL (нельзя дедуплицировать).
	if _, err := s.NormalizeSearchURL("https://market.yandex.ru/search?hid=90555"); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("want ErrInvalidURL for URL without text, got %v", err)
	}
}

// TestYandexSearch_parseSearch — разбор выдачи на срезе реального стейта
// marketfront (testdata/yandex_search.html, снят с прода 2026-06-18). Проверяем
// извлечение идентичности товара (артикул/URL/имя/цена/картинка), пропуск
// notify-only модели без оффера и OldPrice при max>min.
func TestYandexSearch_parseSearch(t *testing.T) {
	b, err := os.ReadFile("testdata/yandex_search.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	s := newYMSearch()
	out := s.parseSearch(string(b))

	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2 (notify-only модель без оффера пропускается)", len(out.Items))
	}

	a := out.Items[0]
	if a.ArticleID != "7118070" {
		t.Errorf("art = %q, want 7118070", a.ArticleID)
	}
	if a.URL != "https://market.yandex.ru/product--stiralnaya-mashina-leran-wad-85148-awd3/7118070" {
		t.Errorf("url = %q", a.URL)
	}
	if a.PriceKopecks != 3199000 {
		t.Errorf("price = %d, want 3199000", a.PriceKopecks)
	}
	if a.OldPriceKopecks != 0 {
		t.Errorf("old = %d, want 0 (min==max)", a.OldPriceKopecks)
	}
	if a.ImageURL != "https://avatars.mds.yandex.net/get-mpic/20417112/2a0000019dd7fea13e77277abca0a1ab9e28/orig" {
		t.Errorf("image = %q", a.ImageURL)
	}
	if a.Name == "" {
		t.Errorf("name empty")
	}

	b2 := out.Items[1]
	if b2.ArticleID != "683611777" {
		t.Errorf("art = %q, want 683611777", b2.ArticleID)
	}
	if b2.PriceKopecks != 3815300 {
		t.Errorf("price = %d, want 3815300", b2.PriceKopecks)
	}
	if b2.OldPriceKopecks != 4100000 {
		t.Errorf("old = %d, want 4100000 (max>min)", b2.OldPriceKopecks)
	}
}

func TestOzonSearch_URLHandling(t *testing.T) {
	s := NewOzonSearchScraper(NewOzonScraper(OzonOptions{}), 60)
	if !s.MatchesSearch("https://www.ozon.ru/search/?text=наушники") {
		t.Error("should match ozon search URL")
	}
	if s.MatchesSearch("https://www.ozon.ru/product/foo-123/") {
		t.Error("should not match ozon product URL")
	}
	got, err := s.NormalizeSearchURL("https://www.ozon.ru/search/?text=Наушники&sorting=price")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ou, _ := url.Parse(got)
	if ou.Path != "/search/" || ou.Query().Get("text") != "наушники" || ou.Query().Get("sorting") != "price" {
		t.Errorf("normalize = %q (query %v)", got, ou.Query())
	}
	// Без browser-сайдкара (OzonOptions{} → not configured) ScrapeSearch отдаёт
	// blocked: поиск Ozon работает только через ozon-miner (mode=browser).
	if _, err := s.ScrapeSearch(nil, "https://www.ozon.ru/search/?text=x"); !errors.Is(err, ErrMarketplaceBlocked) {
		t.Errorf("want ErrMarketplaceBlocked, got %v", err)
	}
}

// TestOzonSearch_ParseTiles проверяет плумбинг парсера выдачи на представительном
// widgetStates: извлечение item'ов, link→SKU, цены по textStyle, абсолютный URL,
// дедуп и лимит. Точные имена полей searchResultsV2 доводятся по прод-логам —
// тест фиксирует разбор предполагаемой структуры (items[]/action.link/price[]).
func TestOzonSearch_ParseTiles(t *testing.T) {
	s := NewOzonSearchScraper(NewOzonScraper(OzonOptions{}), 60)
	body := []byte(`{"widgetStates":{"searchResultsV2-abc":"{\"items\":[` +
		`{\"action\":{\"link\":\"/product/naushniki-test-456/?asb=1\"},` +
		`\"title\":\"Наушники Test\",` +
		`\"price\":{\"price\":[{\"text\":\"1 299 ₽\",\"textStyle\":\"PRICE\"},{\"text\":\"2 000 ₽\",\"textStyle\":\"ORIGINAL_PRICE\"}]},` +
		`\"image\":\"https://ir.ozone.ru/s3/multimedia-1/foo.jpg\"},` +
		`{\"action\":{\"link\":\"/product/naushniki-test-456/\"},\"price\":{\"price\":[{\"text\":\"1 299 ₽\",\"textStyle\":\"PRICE\"}]}}` +
		`]}"}}`)
	out := s.parseSearch(body)
	if len(out.Items) != 1 { // второй тайл — дубль того же SKU
		t.Fatalf("items = %d, want 1 (dedup by SKU)", len(out.Items))
	}
	it := out.Items[0]
	if it.ArticleID != "456" {
		t.Errorf("id = %q, want 456", it.ArticleID)
	}
	if it.URL != "https://www.ozon.ru/product/naushniki-test-456/" {
		t.Errorf("url = %q (query должен отрезаться)", it.URL)
	}
	if it.PriceKopecks != 129900 {
		t.Errorf("price = %d, want 129900", it.PriceKopecks)
	}
	if it.OldPriceKopecks != 200000 {
		t.Errorf("old = %d, want 200000", it.OldPriceKopecks)
	}
	if it.Name != "Наушники Test" {
		t.Errorf("name = %q", it.Name)
	}
	if it.Position != 1 {
		t.Errorf("position = %d, want 1", it.Position)
	}
}
