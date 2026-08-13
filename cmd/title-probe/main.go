// title-probe — снимает названия товаров из выдач WB по списку категорий и
// пишет их в TSV. Нужна, чтобы измерить, насколько названия информативны для
// СОПОСТАВЛЕНИЯ товаров между площадками: как часто в заголовке вообще есть
// извлекаемый код модели/артикул, а как часто остаётся только текст.
//
// Замер решает вопрос из docs/PRODUCT-MATCH-JVM.md §11: с какой категории
// начинать. Брать надо ту, где текст реально решает, а не ту, где всё
// сводится к сравнению кодов регэкспом.
//
// Ходит тем же транспортом, что боевой поиск-скрейпер (tls-client): обычный
// http.Client и curl получают от u-search 403 — причина транспорт, а не IP
// (docs/WB-SEARCH-STATUS.md).
//
//	go run ./cmd/title-probe -out titles.tsv
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

// Категории подобраны по ожидаемой доле кодов в названии: от техники, где
// артикул производителя обычно есть, до одежды и расходников, где его нет.
var queries = []string{
	"кофемашина",
	"смартфон",
	"наушники беспроводные",
	"робот пылесос",
	"фен для волос",
	"кроссовки мужские",
	"коляска детская",
	"корм для кошек",
}

func main() {
	out := flag.String("out", "titles.tsv", "куда писать TSV (категория, бренд, название)")
	pages := flag.Int("pages", 1, "страниц выдачи на категорию")
	// Токены wbaas лежат в Redis и наполняются майнером — вне прод-контура их
	// нет, и direct-путь падает с «пустой wbaas-токен». Браузерный сайдкар
	// токенов не требует и подхватывается как штатный фолбэк на ошибку.
	sidecar := flag.String("sidecar", "", "URL wb-search-miner, напр. http://wb-search-miner:8081")
	flag.Parse()

	s := scraper.NewWildberriesSearchScraper(nil, nil, nil, *pages, 700*time.Millisecond)
	if *sidecar != "" {
		s.SetBrowserSidecar(*sidecar, *pages)
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

	ctx := context.Background()
	for _, q := range queries {
		u := "https://www.wildberries.ru/catalog/0/search.aspx?search=" + url.QueryEscape(q)
		rs, err := s.ScrapeSearch(ctx, u)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%-24s ОШИБКА: %v\n", q, err)
			continue
		}
		for _, it := range rs.Items {
			name := strings.ReplaceAll(it.Name, "\t", " ")
			_ = w.Write([]string{q, it.Brand, name})
		}
		fmt.Fprintf(os.Stderr, "%-24s позиций: %d\n", q, len(rs.Items))
		w.Flush()
		time.Sleep(1200 * time.Millisecond)
	}
}
