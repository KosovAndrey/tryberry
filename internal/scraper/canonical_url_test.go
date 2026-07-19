package scraper

import "testing"

// Канон обязан быть УСТОЙЧИВОЙ ТОЧКОЙ: канон(канон(x)) == канон(x). Иначе
// продукты продолжат двоиться, просто на другом написании.
func TestCanonicalProductURL(t *testing.T) {
	cases := []struct {
		name string
		m    Marketplace
		in   string
		want string
	}{
		{
			name: "WB: slug и хвост detail.aspx сворачиваются",
			m:    MarketplaceWildberries,
			in:   "https://www.wildberries.ru/catalog/221501024/detail.aspx?targetUrl=SG&size=123",
			want: "https://www.wildberries.ru/catalog/221501024/detail.aspx",
		},
		{
			name: "Ozon: тот самый дубль 2274265393 (slug + ?asb=)",
			m:    MarketplaceOzon,
			in:   "https://www.ozon.ru/product/nazvanie-tovara-2274265393/?asb=abc&utm_source=x",
			want: "https://www.ozon.ru/product/2274265393/",
		},
		{
			name: "Ozon: та же карточка голым id — тот же канон",
			m:    MarketplaceOzon,
			in:   "https://www.ozon.ru/product/2274265393/",
			want: "https://www.ozon.ru/product/2274265393/",
		},
		{
			name: "Ali: .com и .ru сходятся в один канон",
			m:    MarketplaceAliexpress,
			in:   "https://aliexpress.com/item/1005006.html?spm=a2g0o.detail",
			want: "https://aliexpress.ru/item/1005006.html",
		},
		{
			// Слаг у YM гуляет (у одного товара нашлось 12 копий) — он декоративный,
			// товар резолвится по id. Проверено живым скрейпером: cmd/ym-canon-probe.
			name: "Я.Маркет: слаг и query-хвост сворачиваются",
			m:    MarketplaceYandexMarket,
			in:   "https://market.yandex.ru/product--smartfon-xiaomi/123456789?sku=101&do-waremd5=z",
			want: "https://market.yandex.ru/product/123456789",
		},
		{
			name: "Я.Маркет: та же карточка уже без слага — тот же канон",
			m:    MarketplaceYandexMarket,
			in:   "https://market.yandex.ru/product/123456789",
			want: "https://market.yandex.ru/product/123456789",
		},
		{
			// /card/ id НЕ сводится к /product/<id> (проверено 2026-07-19: не резолвится),
			// но query-хвост (cpc/OOS) протухает и рушит повторный скрейп — срезаем его,
			// путь со слагом оставляем.
			name: "Я.Маркет /card/: query-хвост срезается, путь остаётся",
			m:    MarketplaceYandexMarket,
			in:   "https://market.yandex.ru/card/kofemashina-jura-e8-15584/5193397317?showOriginalKmEmptyOffer=1&ogV=-12",
			want: "https://market.yandex.ru/card/kofemashina-jura-e8-15584/5193397317",
		},
		{
			name: "id не достаётся → отдаём как есть, дубль лучше потери ссылки",
			m:    MarketplaceOzon,
			in:   "https://www.ozon.ru/product/",
			want: "https://www.ozon.ru/product/",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CanonicalProductURL(c.m, c.in)
			if got != c.want {
				t.Errorf("канон(%q)\n = %q\nждали %q", c.in, got, c.want)
			}
			if again := CanonicalProductURL(c.m, got); again != got {
				t.Errorf("канон не идемпотентен: канон(%q) = %q", got, again)
			}
		})
	}
}

// Формы URL Я.Маркета, для которых канон НЕ переписывает URL, обязаны оставаться
// нетронутыми. Это не придирка: канон переписывает URL, а YM по нему РЕАЛЬНО ходит
// — свернём не туда, и товар молча перестанет скрейпиться либо начнёт отдавать
// чужую цену.
//
// /card/ сюда БОЛЬШЕ НЕ входит: с 2026-07-19 канон срезает у него query-хвост
// (см. TestCanonicalProductURL). Его id по-прежнему НЕ сводится к /product/<id>
// (проверено тёплым ботом — не резолвится), но сам путь /card/<slug>/<id>
// сохраняется, поэтому «нетронутость пути» проверяется там же.
func TestCanonicalYMLeavesUnverifiedForms(t *testing.T) {
	cases := map[string]string{
		// Неразвёрнутая короткая ссылка: id в ней нет вовсе, разворачивать должен резолвер.
		"/cc/ — короткая ссылка, id нет": "https://market.yandex.ru/cc/9w7AHT",
		// Витрина продавца — вообще не карточка, хотя число в пути есть.
		"/business-- — витрина, а не товар": "https://market.yandex.ru/business--shop/123456",
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			if got := CanonicalProductURL(MarketplaceYandexMarket, u); got != u {
				t.Errorf("канон тронул непроверенную форму:\n было  %q\n стало %q", u, got)
			}
		})
	}
}

// Канон обязан оставаться распознаваемым: и Matches (иначе Registry.FindByURL не
// найдёт скрейпер), и экстрактором id (иначе Scrape не сможет скрейпить то, что
// мы сами же сохранили). Регресс здесь = молчаливая потеря товара.
func TestCanonicalProductURLStaysScrapeable(t *testing.T) {
	wb := NewWildberriesScraper(1)
	oz := NewOzonScraper(OzonOptions{})
	ali := NewAliexpressScraper(AliexpressOptions{})

	t.Run("wildberries", func(t *testing.T) {
		u := CanonicalProductURL(MarketplaceWildberries,
			"https://www.wildberries.ru/catalog/221501024/detail.aspx?x=1")
		if !wb.Matches(u) {
			t.Fatalf("Matches отверг собственный канон %q", u)
		}
		if id, err := ExtractArticleID(u); err != nil || id != "221501024" {
			t.Fatalf("ExtractArticleID(%q) = %q, %v", u, id, err)
		}
	})

	t.Run("ozon", func(t *testing.T) {
		u := CanonicalProductURL(MarketplaceOzon,
			"https://www.ozon.ru/product/tovar-2274265393/?asb=1")
		if !oz.Matches(u) {
			t.Fatalf("Matches отверг собственный канон %q", u)
		}
		if id, err := extractOzonID(u); err != nil || id != "2274265393" {
			t.Fatalf("extractOzonID(%q) = %q, %v", u, id, err)
		}
	})

	t.Run("aliexpress", func(t *testing.T) {
		u := CanonicalProductURL(MarketplaceAliexpress,
			"https://aliexpress.com/item/1005006.html?spm=x")
		if !ali.Matches(u) {
			t.Fatalf("Matches отверг собственный канон %q", u)
		}
		if id, err := ExtractAliexpressID(u); err != nil || id != "1005006" {
			t.Fatalf("ExtractAliexpressID(%q) = %q, %v", u, id, err)
		}
	})

	// У Я.Маркета ставка выше остальных: он ЕДИНСТВЕННЫЙ реально загружает
	// сохранённый URL и берёт из его пути sku, которым ищет цену в HTML
	// (ymStatePrice). Разъедься канон с ymExtractSKU — парсер искал бы в странице
	// чужой id и молча брал не ту цену. Поэтому проверяем оба соответствия.
	t.Run("yandex_market", func(t *testing.T) {
		ym := NewYandexMarketScraper(YandexMarketOptions{})
		raw := "https://market.yandex.ru/product--kofevarka-kitfort/1339990358?sku=103961394773"
		u := CanonicalProductURL(MarketplaceYandexMarket, raw)
		if !ym.Matches(u) {
			t.Fatalf("Matches отверг собственный канон %q", u)
		}
		if id, err := ExtractYandexMarketID(u); err != nil || id != "1339990358" {
			t.Fatalf("ExtractYandexMarketID(%q) = %q, %v", u, id, err)
		}
		if got, want := ymExtractSKU(u), ymExtractSKU(raw); got != want {
			t.Fatalf("канон увёл sku: было %q, стало %q — парсер искал бы чужой id", want, got)
		}

		// /card/ канонизируется срезанием query (id в /product/ не сводится). После
		// канона он обязан остаться и распознаваемым (Matches), и парсибельным
		// (ymExtractSKU достаёт тот же id из пути) — иначе товар молча выпадет.
		cardRaw := "https://market.yandex.ru/card/kofemashina-jura-e8-15584/5193397317?showOriginalKmEmptyOffer=1&cpc=xxx"
		cardCanon := CanonicalProductURL(MarketplaceYandexMarket, cardRaw)
		if !ym.Matches(cardCanon) {
			t.Fatalf("Matches отверг канон /card/ %q", cardCanon)
		}
		if got, want := ymExtractSKU(cardCanon), ymExtractSKU(cardRaw); got != want || got != "5193397317" {
			t.Fatalf("канон /card/ увёл sku: было %q, стало %q", want, got)
		}
	})
}
