// title-probe — снимает позиции выдач с маркетплейсов в единый TSV.
//
// Две задачи, обе из docs/PRODUCT-MATCH-JVM.md:
//   - замер, насколько названия несут извлекаемый код модели (§11, стартовая
//     категория) — дальше scripts/title-code-coverage.py;
//   - сбор корпуса под золотой набор пар для сопоставления товаров (§10) —
//     дальше scripts/make-pairs.py.
//
// Переиспользует БОЕВЫЕ парсеры выдачи (`scraper.SearchScraper`), а не свои:
// разметка площадок дрейфует, и держать второй разбор ради пробы значит
// чинить его дважды.
//
// Транспорт у каждой площадки свой, поэтому запускать НА ПРОДЕ, в сети compose:
//
//   - Ozon требует сайдкар ozon-miner (антибот FAB);
//
//   - WB-поиск требует wbaas-токен из Redis либо браузерный сайдкар;
//
//   - Я.Маркет идёт direct без прокси (docs/YANDEX-WARMED-COOKIES.md).
//
//     docker run --rm --network tryberrybot_default -v ~/out:/out title-probe:latest \
//     -mp ym,ozon -ozon http://ozon-miner:8080 -out /out/items.tsv
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

// Смартфоны — стартовая категория, выбранная замером (§11 дока): сильный код
// лишь у 13% названий, товар объективно один и тот же на разных площадках, а
// числовой квалификатор («8/128 ГБ») есть у 100% — то есть самый опасный тип
// ошибки представлен в каждой строке. Запросы подобраны на разброс брендов и
// ценовых сегментов, чтобы в корпус попали и трудные негативы.
var defaultQueries = []string{
	"смартфон",
	"смартфон samsung galaxy",
	"смартфон xiaomi redmi",
	"смартфон realme",
	"смартфон honor",
	"смартфон poco",
	"iphone",
	"смартфон tecno",
}

type namedScraper struct {
	name    string
	scr     scraper.SearchScraper
	makeURL func(q string) string
}

func main() {
	out := flag.String("out", "items.tsv", "куда писать TSV")
	mps := flag.String("mp", "ym,ozon,wb", "площадки через запятую: ym, ozon, wb")
	ozonURL := flag.String("ozon", "http://ozon-miner:8080", "сайдкар ozon-miner")
	wbURL := flag.String("wb", "http://wb-search-miner:8081", "сайдкар wb-search-miner")
	queriesFlag := flag.String("queries", "", "свои запросы через ';' (пусто — смартфоны по умолчанию)")
	maxItems := flag.Int("max", 60, "позиций на запрос")
	flag.Parse()

	queries := defaultQueries
	if strings.TrimSpace(*queriesFlag) != "" {
		queries = strings.Split(*queriesFlag, ";")
	}

	var list []namedScraper
	for _, mp := range strings.Split(*mps, ",") {
		switch strings.TrimSpace(mp) {
		case "ym":
			base := scraper.NewYandexMarketScraper(scraper.YandexMarketOptions{})
			list = append(list, namedScraper{
				name: "yandex_market",
				scr:  scraper.NewYandexMarketSearchScraper(base, *maxItems, 3),
				makeURL: func(q string) string {
					return "https://market.yandex.ru/search?text=" + url.QueryEscape(q)
				},
			})
		case "ozon":
			// Только browser-режим: иначе ScrapeSearch честно вернёт blocked.
			base := scraper.NewOzonScraper(scraper.OzonOptions{Mode: "browser", BrowserURL: *ozonURL})
			list = append(list, namedScraper{
				name: "ozon",
				scr:  scraper.NewOzonSearchScraper(base, *maxItems),
				makeURL: func(q string) string {
					return "https://www.ozon.ru/search/?text=" + url.QueryEscape(q)
				},
			})
		case "wb":
			// Токены wbaas лежат в Redis и наполняются майнером — вне прод-контура
			// их нет, direct падает. Сайдкар токенов не требует и подхватывается
			// штатным фолбэком на ошибку direct-пути.
			s := scraper.NewWildberriesSearchScraper(nil, nil, nil, 2, 700*time.Millisecond)
			s.SetBrowserSidecar(*wbURL, 2)
			list = append(list, namedScraper{
				name: "wildberries",
				scr:  s,
				makeURL: func(q string) string {
					return "https://www.wildberries.ru/catalog/0/search.aspx?search=" + url.QueryEscape(q)
				},
			})
		}
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "не выбрано ни одной площадки (-mp)")
		os.Exit(2)
	}

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create:", err)
		os.Exit(1)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	w.Comma = '\t'
	defer w.Flush()
	_ = w.Write([]string{"marketplace", "article", "brand", "price", "name", "url"})

	ctx := context.Background()
	seen := make(map[string]bool) // площадка+артикул: запросы пересекаются
	for _, ns := range list {
		total := 0
		for _, q := range queries {
			q = strings.TrimSpace(q)
			rs, err := ns.scr.ScrapeSearch(ctx, ns.makeURL(q))
			if err != nil {
				fmt.Fprintf(os.Stderr, "%-14s %-26s ОШИБКА: %v\n", ns.name, q, err)
				continue
			}
			n := 0
			for _, it := range rs.Items {
				key := ns.name + "|" + it.ArticleID
				if seen[key] {
					continue
				}
				seen[key] = true
				_ = w.Write([]string{
					ns.name,
					it.ArticleID,
					it.Brand,
					strconv.FormatFloat(float64(it.PriceKopecks)/100, 'f', 2, 64),
					strings.ReplaceAll(it.Name, "\t", " "),
					it.URL,
				})
				n++
			}
			total += n
			fmt.Fprintf(os.Stderr, "%-14s %-26s новых позиций: %d\n", ns.name, q, n)
			w.Flush()
			time.Sleep(2 * time.Second)
		}
		fmt.Fprintf(os.Stderr, "%-14s ИТОГО: %d\n", ns.name, total)
	}
}
