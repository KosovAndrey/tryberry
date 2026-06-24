package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"testing"
)

func newYMSearch() *YandexMarketSearchScraper {
	// base без прокси: client поднимется (tls-client не требует прокси для init),
	// но сетевых вызовов в этих тестах нет — проверяем только разбор URL/стейта.
	return NewYandexMarketSearchScraper(NewYandexMarketScraper(YandexMarketOptions{}), 60, 5)
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

func TestYandexSeller_Storefront(t *testing.T) {
	s := newYMSearch()
	// Распознавание витрины продавца (business-URL и merchant/shopInShop search).
	match := map[string]bool{
		"https://market.yandex.ru/business--yandex-fabrika/83022309":                       true,
		"https://market.yandex.ru/business--x/83022309":                                    true,
		"https://market.yandex.ru/search?generalContext=t%3Dmerchant%3Bmrch%3D83022309%3B": true,
		"https://market.yandex.ru/business--befree/1001084?generalContext=t%3DshopInShop%3Bi%3D1%3Bbi%3D1001084%3B": true,
	}
	for u, want := range match {
		if got := s.MatchesSearch(u); got != want {
			t.Errorf("MatchesSearch(%q) = %v, want %v", u, got, want)
		}
	}
	// Обе формы одного продавца → один ключ /business--m/<id>.
	want := "https://market.yandex.ru/business--m/83022309"
	for _, u := range []string{
		"https://market.yandex.ru/business--yandex-fabrika/83022309",
		"https://market.yandex.ru/business--x/83022309?foo=bar",
		"https://market.yandex.ru/search?generalContext=t%3Dmerchant%3Bmrch%3D83022309%3B&rs=abc",
	} {
		got, err := s.NormalizeSearchURL(u)
		if err != nil {
			t.Fatalf("Normalize(%q): %v", u, err)
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestYandexSeller_ExtractName(t *testing.T) {
	cases := map[string]string{
		// og:title (JSON-описание тега в head) → имя до «– купить…».
		`x{"property":"og:title","content":"Befree – купить товары в каталоге на Яндекс Маркете"}y`: "Befree",
		`{"property":"og:title","content":"Яндекс Фабрика – купить товары"}`:                        "Яндекс Фабрика",
		// og:title мусорный → фолбэк на <h1>.
		`{"property":"og:title","content":"Яндекс Маркет"} <h1 class="z">Магазин КАПИБАРА</h1>`: "Магазин КАПИБАРА",
		// ничего → "".
		`<div>нет имени</div>`: "",
	}
	for body, want := range cases {
		if got := ymExtractSellerName([]byte(body)); got != want {
			t.Errorf("ymExtractSellerName(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestYandexSeller_PrettifySlug(t *testing.T) {
	cases := map[string]string{"yandex-fabrika": "Yandex Fabrika", "befree": "Befree"}
	for slug, want := range cases {
		if got := prettifyYMSlug(slug); got != want {
			t.Errorf("prettifyYMSlug(%q) = %q, want %q", slug, got, want)
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
	if _, err := s.ScrapeSearch(context.Background(), "https://www.ozon.ru/search/?text=x"); !errors.Is(err, ErrMarketplaceBlocked) {
		t.Errorf("want ErrMarketplaceBlocked, got %v", err)
	}
}

// TestOzonSearch_ParseTiles проверяет парсер на РЕАЛЬНОЙ структуре тайла
// tileGridDesktop (снято с прода, text=iphone): id/sku → SKU, action.link → URL,
// mainState priceV2 (PRICE/ORIGINAL_PRICE), textDS id=="name" → название,
// tileImage → картинка. Плюс дедуп по SKU и обрезка query из URL.
func TestOzonSearch_ParseTiles(t *testing.T) {
	s := NewOzonSearchScraper(NewOzonScraper(OzonOptions{}), 60)
	// Тайл со скидкой (PRICE+ORIGINAL_PRICE) + дубль того же SKU (должен схлопнуться).
	tile := `{"items":[
		{"id":"3592847546","sku":3592847546,
		 "action":{"link":"/product/apple-iphone-17e-3592847546/?at=BrtzXYZ"},
		 "mainState":[
		   {"type":"priceV2","priceV2":{"price":[
		     {"text":"45 961 ₽","textStyle":"PRICE"},
		     {"text":"66 280 ₽","textStyle":"ORIGINAL_PRICE"}]}},
		   {"type":"labelListV2","labelListV2":{"items":[{"type":"text","text":{"text":"1043 и 1 ₽"}}]}},
		   {"type":"textDS","id":"name","textDS":{"text":"Apple Смартфон iPhone 17e"}}],
		 "tileImage":{"items":[{"type":"image","image":{"link":"https://ir.ozone.ru/s3/multimedia-1-0/10351985688.jpg"}}]}},
		{"id":"3592847546","sku":3592847546,
		 "action":{"link":"/product/apple-iphone-17e-3592847546/"},
		 "mainState":[{"type":"priceV2","priceV2":{"price":[{"text":"45 961 ₽","textStyle":"PRICE"}]}}]}
	]}`
	body, err := json.Marshal(map[string]any{
		"widgetStates": map[string]string{"tileGridDesktop-3669724-default-1": tile},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := s.parseSearch(body)
	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1 (дедуп по SKU)", len(out.Items))
	}
	it := out.Items[0]
	if it.ArticleID != "3592847546" {
		t.Errorf("id = %q, want 3592847546", it.ArticleID)
	}
	if it.URL != "https://www.ozon.ru/product/apple-iphone-17e-3592847546/" {
		t.Errorf("url = %q (query должен отрезаться)", it.URL)
	}
	if it.PriceKopecks != 4596100 {
		t.Errorf("price = %d, want 4596100", it.PriceKopecks)
	}
	if it.OldPriceKopecks != 6628000 {
		t.Errorf("old = %d, want 6628000", it.OldPriceKopecks)
	}
	if it.Name != "Apple Смартфон iPhone 17e" {
		t.Errorf("name = %q", it.Name)
	}
	if it.ImageURL != "https://ir.ozone.ru/s3/multimedia-1-0/10351985688.jpg" {
		t.Errorf("image = %q", it.ImageURL)
	}
	if it.Position != 1 {
		t.Errorf("position = %d, want 1", it.Position)
	}
}
