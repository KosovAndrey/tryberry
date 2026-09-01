package scraper

import (
	"context"
	"errors"
	"testing"
)

func TestParseYandexMarketHTML(t *testing.T) {
	html := `<html><head>
<script type="application/ld+json">{"@type":"BreadcrumbList","name":"x"}</script>
<script type="application/ld+json">
{"@type":"Product","name":"Смартфон Apple iPhone 15","image":["https://avatars.mds.yandex.net/get-mpic/1/img.jpg","https://avatars.mds.yandex.net/get-mpic/2/img.jpg"],"offers":{"price":"79990.00"}}
</script>
</head><body>...</body></html>`

	r, err := parseYandexMarketHTML(html, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Name != "Смартфон Apple iPhone 15" {
		t.Errorf("name = %q", r.Name)
	}
	if r.Price != 79990 {
		t.Errorf("price = %v, want 79990", r.Price)
	}
	if r.ImageURL != "https://avatars.mds.yandex.net/get-mpic/1/img.jpg" {
		t.Errorf("image = %q", r.ImageURL)
	}
}

func TestParseYandexMarketHTML_StringImage(t *testing.T) {
	// image как строка (не массив) — JSON-LD отдаёт оба варианта.
	html := `<script type="application/ld+json">{"@type":"Product","name":"X","image":"https://im.jpg","offers":{"price":"100"}}</script>`
	r, err := parseYandexMarketHTML(html, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ImageURL != "https://im.jpg" || r.Price != 100 {
		t.Errorf("got %+v", r)
	}
}

func TestParseYandexMarketHTML_NumericPrice(t *testing.T) {
	// Цена числом (не строкой) — раньше роняла Unmarshal блока.
	html := `<script type="application/ld+json">{"@type":"Product","name":"X","offers":{"price":12990}}</script>`
	r, err := parseYandexMarketHTML(html, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Price != 12990 {
		t.Errorf("price = %v, want 12990", r.Price)
	}
}

func TestParseYandexMarketHTML_AggregateOfferGraph(t *testing.T) {
	// @graph-обёртка + AggregateOffer с lowPrice + @type массивом.
	html := `<script type="application/ld+json">{"@context":"https://schema.org","@graph":[
		{"@type":"BreadcrumbList"},
		{"@type":["Product","IndividualProduct"],"name":"Y","image":"https://im.jpg",
		 "offers":{"@type":"AggregateOffer","lowPrice":3499,"priceCurrency":"RUR"}}
	]}</script>`
	r, err := parseYandexMarketHTML(html, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Price != 3499 || r.Name != "Y" {
		t.Errorf("got %+v, want price 3499 name Y", r)
	}
}

func TestParseYandexMarketHTML_OffersArray(t *testing.T) {
	html := `<script type="application/ld+json">{"@type":"Product","name":"Z","offers":[{"price":"550"},{"price":"600"}]}</script>`
	r, err := parseYandexMarketHTML(html, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Price != 550 {
		t.Errorf("price = %v, want 550 (first offer)", r.Price)
	}
}

func TestParseYandexMarketHTML_StatePriceIsLastKnownOOS(t *testing.T) {
	// Карточка модели с пустым buy-box: JSON-LD Product БЕЗ offers, цена — только
	// в стейте marketfront. Это НЕ «в наличии»: offers нет → InStock=false, а
	// стейт-цена несётся как последняя известная (для «последняя цена X» + опоры
	// below_target/discount_pct).
	html := `<script type="application/ld+json">{"@type":"BreadcrumbList"}</script>` +
		`<script type="application/ld+json">{"@type":"Product","name":"Кофемашина Jura E8","image":"https://im.jpg"}</script>` +
		`<script>window.__state={"img":"/orig","price":{"value":"128931","currency":"RUR"},"size":24}</script>`
	r, err := parseYandexMarketHTML(html, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.InStock {
		t.Error("InStock should be false — нет offers в JSON-LD (пустой buy-box)")
	}
	if r.Price != 128931 || r.Name != "Кофемашина Jura E8" || r.ImageURL != "https://im.jpg" {
		t.Errorf("got %+v, want last price 128931 / name Jura E8 / image im.jpg", r)
	}
}

func TestParseYandexMarketHTML_StatePriceAnchoredToSKU(t *testing.T) {
	// На странице много ценовых сниппетов (аксессуар 50000 ИДЁТ ПЕРВЫМ, затем сам
	// товар рядом со своим SKU). Без якоря взяли бы 50000 (первое вхождение) —
	// именно отсюда «скачущая цена». С SKU-якорем берём 128931 (ближе к SKU).
	sku := "5193397317"
	html := `<script type="application/ld+json">{"@type":"Product","name":"Jura E8"}</script>` +
		`<script>var s={"accessory":{"price":{"value":"50000","currency":"RUR"}},` +
		`"main":{"sku":"` + sku + `","title":"Jura E8","price":{"value":"128931","currency":"RUR"}},` +
		`"reco":{"price":{"value":"131000","currency":"RUR"}}}</script>`
	url := "https://market.yandex.ru/card/kofemashina-jura-e8-15584/" + sku + "?showOriginalKmEmptyOffer=1"
	r, err := parseYandexMarketHTML(html, url)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.InStock {
		t.Error("InStock should be false")
	}
	if r.Price != 128931 {
		t.Errorf("price = %v, want 128931 (ближайшая к SKU, не первое вхождение 50000)", r.Price)
	}
}

func TestYMExtractSKU(t *testing.T) {
	cases := map[string]string{
		"https://market.yandex.ru/card/kofemashina-jura-e8-15584/5193397317?x=1": "5193397317",
		"https://market.yandex.ru/product--slug/123456":                          "123456",
		"https://market.yandex.ru/search?text=кофемашина":                        "",
	}
	for u, want := range cases {
		if got := ymExtractSKU(u); got != want {
			t.Errorf("ymExtractSKU(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestParseYandexMarketHTML_OutOfStock(t *testing.T) {
	// Карточка модели без активного оффера: JSON-LD Product есть, но цены нет
	// нигде (ни offers, ни стейт). Это УСПЕХ с InStock=false (не ошибка) —
	// товар можно добавить с триггером back_in_stock.
	html := `<script type="application/ld+json">{"@type":"Product","name":"Кофемашина Jura","image":"https://im.jpg"}</script>` +
		`<script>window.__state={"foo":"bar"}</script>`
	r, err := parseYandexMarketHTML(html, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.InStock {
		t.Error("InStock should be false for card without offer")
	}
	if r.Price != 0 {
		t.Errorf("price = %v, want 0", r.Price)
	}
	if r.Name != "Кофемашина Jura" || r.ImageURL != "https://im.jpg" {
		t.Errorf("got %+v, want name/image from Product node", r)
	}
}

func TestParseYandexMarketHTML_InStockFlag(t *testing.T) {
	// Обычная карточка с ценой → InStock=true.
	html := `<script type="application/ld+json">{"@type":"Product","name":"X","offers":{"price":"100"}}</script>`
	r, err := parseYandexMarketHTML(html, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.InStock {
		t.Error("InStock should be true when price found")
	}
}

func TestParseYandexMarketHTML_StatePriceNeedsProduct(t *testing.T) {
	// Страница поиска: цена в стейте есть, но JSON-LD Product НЕТ → не трекаем
	// (цена принадлежит чужому сниппету выдачи, а не отслеживаемому товару).
	html := `<script type="application/ld+json">{"@type":"WebSite"}</script>` +
		`<script type="application/ld+json">{"@type":"BreadcrumbList"}</script>` +
		`<script>window.__state={"price":{"value":"25997","currency":"RUR"}}</script>`
	if _, err := parseYandexMarketHTML(html, ""); !errors.Is(err, ErrProductNotFound) {
		t.Errorf("want ErrProductNotFound (no Product node), got %v", err)
	}
}

func TestParseYandexMarketHTML_NoProduct(t *testing.T) {
	html := `<script type="application/ld+json">{"@type":"WebSite","name":"Я.Маркет"}</script>`
	if _, err := parseYandexMarketHTML(html, ""); !errors.Is(err, ErrProductNotFound) {
		t.Errorf("want ErrProductNotFound, got %v", err)
	}
}

func TestIsYandexCaptcha(t *testing.T) {
	if !isYandexCaptcha([]byte(`<div class="SmartCaptcha">подтвердите, что запросы отправляли вы`)) {
		t.Error("should detect SmartCaptcha challenge page")
	}
	if isYandexCaptcha([]byte(`<script type="application/ld+json">{"@type":"Product"}</script>`)) {
		t.Error("false positive on product page")
	}
	// Реальная страница приложения Я.Маркета со словом captcha в бандле — НЕ блок
	// (это и был баг ложного "blocked").
	real := `<!DOCTYPE html><!--BEGIN [@marketfront/Root]--><html data-baobab-name="$page">` +
		`<script src="https://yastatic.net/captcha/captcha.js"></script>`
	if isYandexCaptcha([]byte(real)) {
		t.Error("false positive on real @marketfront page with captcha.js in bundle")
	}
}

func TestYandexMatches(t *testing.T) {
	s := &YandexMarketScraper{}
	// Живая форма /card/ — карточка, и ExtractYandexMarketID обязан её принимать:
	// с 01-09-2026 именно она единственная открывается.
	if id, err := ExtractYandexMarketID("https://market.yandex.ru/card/kofemashina-jura/5193397317"); err != nil || id != "5193397317" {
		t.Errorf("ExtractYandexMarketID(/card/) = %q, %v; want 5193397317, nil", id, err)
	}
	if id, err := ExtractYandexMarketID("https://market.yandex.ru/card/x/5193397317"); err != nil || id != "5193397317" {
		t.Errorf("ExtractYandexMarketID(/card/x/) = %q, %v; want 5193397317, nil", id, err)
	}
	if !s.Matches("https://market.yandex.ru/product--slug/123") {
		t.Error("should match yandex market url")
	}
	if s.Matches("https://www.ozon.ru/product/123") {
		t.Error("should not match ozon url")
	}
	// Неразвёрнутый шорт /cc/: резолв упал (SmartCaptcha), URL остался сырым.
	// Не матчим — иначе Scrape уйдёт с пустым sku и возьмёт чужую цену (ozon-oos
	// класс). Пусть FindByURL отвергнет, юзеру — «пришли полную ссылку».
	if s.Matches("https://market.yandex.ru/cc/9w7AHT") {
		t.Error("should NOT match unresolved /cc/ short link")
	}
}

// Новый OOS-шаблон («Нет в продаже» без JSON-LD Product): ретраим полной
// карточкой только при наличии ссылки showOriginalKmEmptyOffer и без рекурсии.
func TestYMOOSFullCardURL(t *testing.T) {
	oosBody := `<div data-baobab-name="notOnSale"><a href="/product--x/1?showOriginalKmEmptyOffer=1" data-auto="link-to-full-card"></a></div>`

	got, ok := ymOOSFullCardURL("https://market.yandex.ru/product--x/1", oosBody)
	if !ok || got != "https://market.yandex.ru/product--x/1?showOriginalKmEmptyOffer=1" {
		t.Errorf("ждём URL полной карточки, got %q ok=%v", got, ok)
	}

	// URL уже с query — параметр добавляется через &.
	got, ok = ymOOSFullCardURL("https://market.yandex.ru/product--x/1?sku=2", oosBody)
	if !ok || got != "https://market.yandex.ru/product--x/1?sku=2&showOriginalKmEmptyOffer=1" {
		t.Errorf("ждём &-конкатенацию, got %q ok=%v", got, ok)
	}

	// Выпиленная карточка: ссылки на полную карточку нет — ретрая нет.
	if _, ok := ymOOSFullCardURL("https://market.yandex.ru/product--x/1", "<html>пусто</html>"); ok {
		t.Error("без маркера ретрая быть не должно")
	}

	// Запрос уже по полной карточке — не рекурсим.
	if _, ok := ymOOSFullCardURL("https://market.yandex.ru/product--x/1?showOriginalKmEmptyOffer=1", oosBody); ok {
		t.Error("рекурсивный ретрай запрещён")
	}
}

// Рубильник proxy-first включается ТОЛЬКО при заданном прокси: без него
// переворачивать нечего, и скрейпер должен остаться на direct-пути.
func TestYandexProxyPrimaryRequiresProxy(t *testing.T) {
	withProxy := NewYandexMarketScraper(YandexMarketOptions{
		ProxyURL:     "http://user:pass@127.0.0.1:1",
		ProxyPrimary: true,
	})
	if withProxy.proxy == nil || !withProxy.proxyPrimary {
		t.Fatalf("с прокси и ProxyPrimary=true ждём proxy-first, got proxy=%v primary=%v",
			withProxy.proxy != nil, withProxy.proxyPrimary)
	}

	noProxy := NewYandexMarketScraper(YandexMarketOptions{ProxyPrimary: true})
	if noProxy.proxy != nil || noProxy.proxyPrimary {
		t.Fatalf("без прокси ProxyPrimary должен игнорироваться, got proxy=%v primary=%v",
			noProxy.proxy != nil, noProxy.proxyPrimary)
	}

	def := NewYandexMarketScraper(YandexMarketOptions{ProxyURL: "http://user:pass@127.0.0.1:1"})
	if def.proxyPrimary {
		t.Fatal("по умолчанию порядок прежний: direct первый, прокси — фолбэк на капчу")
	}
}

// TestYandexMarket_DeadURLFormRejectedWithoutNetwork — ссылки в закрытых формах
// отвергаются ДО похода в сеть. Это не педантизм: такой URL иначе ловит капчу,
// считается blocked и двигает брейкер, а брейкер один на площадку — пара старых
// ссылок глушит скрейп живых карточек (инцидент 01-09-2026).
func TestYandexMarket_DeadURLFormRejectedWithoutNetwork(t *testing.T) {
	s := NewYandexMarketScraper(YandexMarketOptions{RPS: 100})

	dead := []string{
		"https://market.yandex.ru/product/923133126",
		"https://market.yandex.ru/product--smartfon-xiaomi/923133126",
		"https://market.yandex.ru/product/923133126?sku=1",
	}
	for _, u := range dead {
		_, err := s.Scrape(context.Background(), u)
		if !errors.Is(err, ErrDeadURLForm) {
			t.Errorf("Scrape(%q) = %v, want ErrDeadURLForm", u, err)
		}
	}

	// Живую форму отказ не задевает: она уходит в сеть (здесь запрос не удастся,
	// но ошибка обязана быть ЛЮБОЙ другой, не ErrDeadURLForm).
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // не ходим в сеть в тесте — отменённый контекст
	if _, err := s.Scrape(ctx, "https://market.yandex.ru/card/x/5193397317"); errors.Is(err, ErrDeadURLForm) {
		t.Error("живая форма /card/ не должна отвергаться как мёртвая")
	}
}
