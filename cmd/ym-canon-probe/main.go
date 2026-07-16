// Command ym-canon-probe — разовый эксперимент: можно ли канонизировать URL
// карточки Я.Маркета до `/product/<id>` (без слага). НЕ часть прод-флоу.
//
// Зачем. products.url UNIQUE = ключ товара, поэтому для WB/Ozon/Ali мы сводим
// ссылку к канону по id (internal/scraper/canonical_url.go) — иначе одна карточка
// заводит несколько products со своей историей и public_id. Я.Маркет там
// НАМЕРЕННО пропущен, и он же самый крупный в базе (14344 товара на 2026-07-16).
//
// Почему пропущен: YM — единственный, кто РЕАЛЬНО загружает сохранённый URL
// (остальные достают из него id и работают по id). Больше того, парсер цены берёт
// sku из ПУТИ (ymExtractSKU → ymStatePrice). Значит канон обязан не просто
// открываться, а открываться И парситься — иначе мы молча потеряем цену у всего
// Я.Маркета. Это и проверяем: гоняем обе формы через НАСТОЯЩИЙ скрейпер и сверяем.
//
// Формы:
//
//	A slug — как сейчас в базе: /product--<slug>/<id>[?...]
//	B canon — кандидат в канон:  /product/<id>
//
// Вердикт: канон годится, только если B даёт ту же цену и то же имя, что A, на
// ВСЕХ пробах. Одно расхождение — идея отвергается.
//
// ВАЖНО про пустые пробы: если форма A не дала ЖИВОЙ цены (товар OOS/удалён/
// парсер промахнулся), проба НЕПРИГОДНА и в вердикт не идёт. Иначе «обе формы
// вернули 0» читалось бы как «совпало» — сравнение пустого с пустым. Ровно на
// этом я обжёгся 2026-07-16, подставив выдуманный id.
//
// Запуск (YM ходит direct с датацентр-IP; локально тоже работает):
//
//	go run ./cmd/ym-canon-probe -query 'кофеварка'      # реальные товары из выдачи
//	go run ./cmd/ym-canon-probe -url '<карточка>'       # конкретная карточка
package main

import (
	"context"
	"flag"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

type urlList []string

func (u *urlList) String() string     { return fmt.Sprint(*u) }
func (u *urlList) Set(v string) error { *u = append(*u, v); return nil }

func main() {
	var urls urlList
	flag.Var(&urls, "url", "URL карточки Я.Маркета (можно повторять)")
	query := flag.String("query", "", "взять реальные товары из выдачи по запросу")
	n := flag.Int("n", 4, "сколько товаров брать из выдачи")
	proxy := flag.String("proxy", "", "RU-прокси на случай капчи (опционально)")
	flag.Parse()

	if len(urls) == 0 && *query == "" {
		fmt.Fprintln(os.Stderr, "usage: ym-canon-probe -query 'кофеварка' | -url '<карточка>' [-proxy http://...]")
		os.Exit(2)
	}

	base := scraper.NewYandexMarketScraper(scraper.YandexMarketOptions{ProxyURL: *proxy, RPS: 0.5})
	ctx := context.Background()

	if *query != "" {
		got, err := realURLsFromSearch(ctx, base, *query, *n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "выдача не отдалась: %v\n", err)
			os.Exit(1)
		}
		urls = append(urls, got...)
		fmt.Printf("взято из выдачи %q: %d карточек\n\n", *query, len(got))
	}

	fmt.Printf("%-8s %-12s %-10s %s\n", "форма", "цена", "в наличии", "имя / ошибка")
	fmt.Println("─────────────────────────────────────────────────────────────────────")

	var usable, matched int
	rejected := false
	for _, raw := range urls {
		// Канон строим ровно тем же кодом, что и прод, — иначе пробник проверял бы
		// не то, что поедет в базу.
		canon := scraper.CanonicalProductURL(scraper.MarketplaceYandexMarket, raw)
		if canon == raw && !strings.Contains(raw, "/product/") {
			fmt.Printf("!! id не достаётся из %s — пропуск\n\n", raw)
			continue
		}

		a, aErr := base.Scrape(ctx, raw)
		report("A slug", a, aErr)
		time.Sleep(2 * time.Second) // не долбим: у YM один IP и SmartCaptcha
		b, bErr := base.Scrape(ctx, canon)
		report("B canon", b, bErr)

		// Проба годится в вердикт, только если контрольная форма дала ЖИВУЮ цену:
		// «обе вернули 0» — это не совпадение, а отсутствие данных.
		if aErr != nil || a.Price <= 0 || !a.InStock {
			fmt.Printf("   → проба НЕПРИГОДНА (у формы A нет живой цены — OOS/ошибка), в вердикт не идёт\n\n")
			continue
		}
		usable++
		switch {
		case bErr != nil:
			fmt.Printf("   → канон НЕ ОТКРЫЛСЯ: %v\n", bErr)
			rejected = true
		case b.Price != a.Price || b.Name != a.Name:
			fmt.Printf("   → РАСХОЖДЕНИЕ (A: %.2f/%q против B: %.2f/%q)\n", a.Price, a.Name, b.Price, b.Name)
			rejected = true
		default:
			matched++
			fmt.Printf("   → совпало (%s)\n", canon)
		}
		fmt.Println()
	}

	fmt.Printf("пригодных проб: %d, совпало: %d\n", usable, matched)
	switch {
	case usable == 0:
		fmt.Println("ВЕРДИКТ: НЕТ ДАННЫХ — ни одной пригодной пробы (все товары без живой цены).")
		fmt.Println("Это НЕ подтверждение канона. Повторить на товарах в наличии.")
		os.Exit(2)
	case rejected:
		fmt.Println("ВЕРДИКТ: канон НЕ годится. Я.Маркет оставляем как есть.")
		os.Exit(1)
	default:
		fmt.Println("ВЕРДИКТ: канон /product/<id> годится — все пригодные пробы совпали.")
	}
}

// realURLsFromSearch — настоящие карточки из выдачи (со слагом и живой ценой),
// чтобы не проверять канон на выдуманных id.
func realURLsFromSearch(ctx context.Context, base *scraper.YandexMarketScraper, query string, n int) ([]string, error) {
	ss := scraper.NewYandexMarketSearchScraper(base, n, 1)
	norm, err := ss.NormalizeSearchURL("https://market.yandex.ru/search?text=" + neturl.QueryEscape(query))
	if err != nil {
		return nil, err
	}
	set, err := ss.ScrapeSearch(ctx, norm)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for _, it := range set.Items {
		if len(out) >= n {
			break
		}
		if it.URL != "" {
			out = append(out, it.URL)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("выдача пуста")
	}
	return out, nil
}

func report(form string, r *scraper.Result, err error) {
	if err != nil {
		fmt.Printf("%-6s %-12s %-10s %v\n", form, "—", "—", err)
		return
	}
	// Режем по рунам, а не байтам: кириллица иначе рвётся посередине символа.
	name := []rune(r.Name)
	if len(name) > 40 {
		name = name[:40]
	}
	fmt.Printf("%-6s %-12.2f %-10v %s\n", form, r.Price, r.InStock, string(name))
}
