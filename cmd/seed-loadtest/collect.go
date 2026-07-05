package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

// defaultQueries — вшитый список запросов: в прод-образ попадает только бинарь,
// файл рядом не лежит. Флаг -queries переопределяет.
//
//go:embed queries.txt
var defaultQueries string

// collectedProduct — строка products.jsonl: реальный товар из поисковой выдачи.
type collectedProduct struct {
	Marketplace  string `json:"marketplace"`
	URL          string `json:"url"`
	Name         string `json:"name"`
	ImageURL     string `json:"image_url,omitempty"`
	PriceKopecks int64  `json:"price_kopecks"`
}

// searchURL — поисковая ссылка маркетплейса для текстового запроса (формат тот
// же, что принимают MatchesSearch соответствующих SearchScraper'ов).
func searchURL(marketplace, query string) string {
	q := url.QueryEscape(query)
	switch marketplace {
	case string(scraper.MarketplaceWildberries):
		return "https://www.wildberries.ru/catalog/0/search.aspx?search=" + q
	case string(scraper.MarketplaceYandexMarket):
		return "https://market.yandex.ru/search?text=" + q
	case string(scraper.MarketplaceOzon):
		return "https://www.ozon.ru/search/?text=" + q
	case string(scraper.MarketplaceAliexpress):
		return "https://aliexpress.ru/wholesale?SearchText=" + q
	}
	return ""
}

// runCollect — обход выдачи. Обвязка скрейперов повторяет cmd/search-worker
// (те же env: WB-токены из Redis, OZON_BROWSER_URL для сайдкара и т.д.).
// Результат дописывается в -out; уже собранные URL пропускаются (резюмируемо).
func runCollect(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	queriesPath := fs.String("queries", "", "файл запросов (по одному в строке); пусто — вшитый список")
	outPath := fs.String("out", "products.jsonl", "куда писать JSONL")
	mps := fs.String("mp", "wildberries,yandex_market,aliexpress", "маркетплейсы через запятую (ozon — при наличии OZON_BROWSER_URL)")
	perQuery := fs.Int("per-query", 60, "макс. товаров с одного запроса на маркетплейс")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	queries := parseLines(defaultQueries)
	if *queriesPath != "" {
		var err error
		queries, err = readLines(*queriesPath)
		if err != nil {
			return fmt.Errorf("queries: %w", err)
		}
	}

	registry, err := buildSearchRegistry(ctx, log)
	if err != nil {
		return err
	}

	seen, err := loadSeenURLs(*outPath)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(*outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	enc := json.NewEncoder(out)

	total := 0
	for _, mp := range splitCSV(*mps) {
		for i, q := range queries {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			su := searchURL(mp, q)
			if su == "" {
				log.Warn("unknown marketplace, skipped", "mp", mp)
				break
			}
			ss, err := registry.FindSearchByURL(su)
			if err != nil {
				log.Warn("no search scraper", "mp", mp, "err", err)
				break
			}
			res, err := ss.ScrapeSearch(ctx, su)
			if err != nil {
				// Частичный сбой одного запроса не валит обход: собираем что можем.
				log.Warn("scrape search failed", "mp", mp, "query", q, "err", err)
				continue
			}
			added := 0
			for _, it := range res.Items {
				if added >= *perQuery {
					break
				}
				if it.URL == "" || it.PriceKopecks <= 0 || seen[it.URL] {
					continue
				}
				seen[it.URL] = true
				if err := enc.Encode(collectedProduct{
					Marketplace:  mp,
					URL:          it.URL,
					Name:         strings.TrimSpace(it.Name),
					ImageURL:     it.ImageURL,
					PriceKopecks: it.PriceKopecks,
				}); err != nil {
					return err
				}
				added++
				total++
			}
			log.Info("query done", "mp", mp, "query", q, "added", added,
				"progress", fmt.Sprintf("%d/%d", i+1, len(queries)))
		}
	}
	log.Info("collect finished", "new_products", total, "out", *outPath)
	return nil
}

// buildSearchRegistry — реестр search-скрейперов, обвязка как в cmd/search-worker.
func buildSearchRegistry(ctx context.Context, log *slog.Logger) (*scraper.Registry, error) {
	var scrapers []scraper.MarketplaceScraper

	wbCard := scraper.NewWildberriesScraper(5)
	redisURL := os.Getenv("REDIS_URL")
	var tokens scraper.TokenProvider = scraper.StaticTokenProvider{}
	if redisURL != "" {
		if rc, err := db.NewRedisClient(ctx, redisURL); err != nil {
			log.Warn("redis unavailable — WB search token pool empty", "err", err)
		} else {
			wbCard.SetBasketResolver(redisrepo.NewBasketCache(rc))
			tokens = scraper.NewRedisSearchTokenPool(rc, getEnvInt("WB_TOKEN_POOL_SIZE", 5), log,
				getEnv("WB_TOKEN_POOL_PREFIX", "wb:search:"))
		}
	}
	wbSearch := scraper.NewWildberriesSearchScraper(
		wbCard, nil, tokens, getEnvInt("SEARCH_MAX_PAGES", 3), 700*time.Millisecond)
	// 403-фолбэк горячих запросов в браузер-сайдкар (как в search-worker) — иначе
	// сбор WB по популярным запросам упрётся в wbaas 403.
	wbSearch.SetBrowserSidecar(getEnv("WB_SEARCH_BROWSER_URL", ""), getEnvInt("WB_SEARCH_BROWSER_MAX_PAGES", 1))
	scrapers = append(scrapers, wbSearch)

	scrapers = append(scrapers, scraper.NewYandexMarketSearchScraper(
		scraper.NewYandexMarketScraper(scraper.YandexMarketOptions{
			ProxyURL: getEnv("YANDEX_PROXY_URL", ""),
			RPS:      2,
			Logger:   log,
		}), getEnvInt("SEARCH_MAX_ITEMS_YANDEX", 100), getEnvInt("YANDEX_MAX_PAGES", 12)))

	scrapers = append(scrapers, scraper.NewAliexpressSearchScraper(
		scraper.NewAliexpressScraper(scraper.AliexpressOptions{
			ProxyURL: getEnv("ALI_PROXY_URL", ""),
			RPS:      2,
			Logger:   log,
		}), 80))

	// Ozon — только через сайдкар ozon-miner; без OZON_BROWSER_URL выдача
	// вернёт blocked, поэтому регистрируем лишь при заданном URL.
	if bu := os.Getenv("OZON_BROWSER_URL"); bu != "" {
		scrapers = append(scrapers, scraper.NewOzonSearchScraper(
			scraper.NewOzonScraper(scraper.OzonOptions{Mode: "browser", BrowserURL: bu, Logger: log}),
			getEnvInt("SEARCH_MAX_ITEMS_OZON", 60)))
	}

	return scraper.NewRegistry(scrapers...), nil
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseLines(string(data)), nil
}

func parseLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// loadSeenURLs — URL из уже существующего out-файла (резюмирование обхода).
func loadSeenURLs(path string) (map[string]bool, error) {
	seen := map[string]bool{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return seen, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for sc.Scan() {
		var p collectedProduct
		if json.Unmarshal(sc.Bytes(), &p) == nil && p.URL != "" {
			seen[p.URL] = true
		}
	}
	return seen, sc.Err()
}
