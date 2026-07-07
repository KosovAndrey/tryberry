// cmd/search-worker — воркер поисковой выдачи WB.
//
// Consumer группы KAFKA_GROUP_ID на топике SEARCH_TASKS_TOPIC; уведомления уходят
// событием в notifier через топик search-events. Реплицируется свободно (как
// consumer group). Один бинарь обслуживает обе дорожки — конфигурируется env:
//
//	обычная:  SEARCH_TASKS_TOPIC=search-tasks   KAFKA_GROUP_ID=search-workers
//	          WB_TOKEN_POOL_PREFIX=wb:search:
//	перекупы: SEARCH_TASKS_TOPIC=reseller-tasks  KAFKA_GROUP_ID=reseller-workers
//	          WB_TOKEN_POOL_PREFIX=wb:reseller:   (масштабируется --scale)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"gitlab.com/KosovAndrey/tryberrybot/internal/config"
	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
	"gitlab.com/KosovAndrey/tryberrybot/internal/tracing"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("search-worker failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	_ = godotenv.Load()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	databaseURL := config.MustEnv("DATABASE_URL")
	redisURL := config.MustEnv("REDIS_URL")
	kafkaBrokers := strings.Split(config.MustEnv("KAFKA_BROKERS"), ",")
	kafkaGroupID := config.MustEnv("KAFKA_GROUP_ID")
	tasksTopic := getEnv("SEARCH_TASKS_TOPIC", "search-tasks")
	poolPrefix := getEnv("WB_TOKEN_POOL_PREFIX", "wb:search:")
	otlpEndpoint := getEnv("OTLP_ENDPOINT", "jaeger:4317")
	healthPort := getEnv("SEARCH_WORKER_HEALTH_PORT", "8094")
	defaultInterval := time.Duration(getEnvInt("SEARCH_SCRAPE_INTERVAL_MINUTES", 20)) * time.Minute

	rpsWB, err := strconv.ParseFloat(getEnv("SCRAPER_RATE_LIMIT_RPS_WB", "5"), 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS_WB: %w", err)
	}

	shutdownTracing, err := tracing.Init(ctx, "search-worker", otlpEndpoint)
	if err != nil {
		log.Warn("tracing init failed, continuing without", "err", err)
	} else {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdownTracing(shutdownCtx)
		}()
		log.Info("tracing initialized", "endpoint", otlpEndpoint)
	}

	pool, err := db.NewPostgresPool(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	redisClient, err := db.NewRedisClient(ctx, redisURL)
	if err != nil {
		log.Warn("redis unavailable — token pool will be empty", "err", err)
		redisClient = nil
	}

	go health.RunServer(ctx, log, pool, redisClient, healthPort)

	productRepo := postgres.NewProductRepo(pool)
	searchQueryRepo := postgres.NewSearchQueryRepo(pool)
	searchSubRepo := postgres.NewSearchSubscriptionRepo(pool)
	searchResultRepo := postgres.NewSearchResultRepo(pool)
	searchNotifRepo := postgres.NewSearchNotificationRepo(pool)

	// ── Скрейпер WB-поиска ────────────────────────────────────────────────────
	poolSize := getEnvInt("WB_TOKEN_POOL_SIZE", 5)
	tokenProvider := scraper.NewRedisSearchTokenPool(redisClient, poolSize, log, poolPrefix)

	proxyPool, proxyErrs := scraper.NewProxyPool(splitCSV(getEnv("SEARCH_PROXY_URLS", "")), 12*time.Second)
	for _, e := range proxyErrs {
		log.Warn("bad search proxy, skipped", "err", e)
	}

	wbCard := scraper.NewWildberriesScraper(rpsWB)
	if redisClient != nil {
		wbCard.SetBasketResolver(redisrepo.NewBasketCache(redisClient))
	}
	wbSearch := scraper.NewWildberriesSearchScraper(
		wbCard,
		proxyPool,
		tokenProvider,
		getEnvInt("SEARCH_MAX_PAGES", 5),
		time.Duration(getEnvInt("SEARCH_PAGE_DELAY_MS", 700))*time.Millisecond,
	)
	// 403-фолбэк WB-поиска в браузер-сайдкар wb-search-miner: wbaas режет горячие
	// запросы direct (различие в транспорте, не в токене). Пусто → фолбэка нет.
	// Браузер-страницы дороги (навигация) → по умолчанию 1 (топ-100).
	wbSearch.SetBrowserSidecar(getEnv("WB_SEARCH_BROWSER_URL", ""), getEnvInt("WB_SEARCH_BROWSER_MAX_PAGES", 1))
	// WB-витрина продавца (/seller/{id}): открытый каталог-API, без токена/прокси.
	// Тот же товарный базовый скрейпер, что у поиска. SELLER_MAX_PAGES = CAP×100.
	wbSeller := scraper.NewWildberriesSellerScraper(
		wbCard,
		getEnvInt("SELLER_MAX_PAGES", 5),
		time.Duration(getEnvInt("SEARCH_PAGE_DELAY_MS", 700))*time.Millisecond,
	)
	// Я.Маркет-поиск: тот же транспорт, что у карточки (tls-client + RU-прокси).
	yandexSearch := scraper.NewYandexMarketSearchScraper(
		scraper.NewYandexMarketScraper(scraper.YandexMarketOptions{
			ProxyURL: getEnv("YANDEX_PROXY_URL", getEnv("OZON_PROXY_URL", "")),
			RPS:      2,
			Logger:   log,
		}),
		getEnvInt("SEARCH_MAX_ITEMS_YANDEX", 100),
		getEnvInt("YANDEX_MAX_PAGES", 12),
	)
	// Ozon-поиск: только через сайдкар ozon-miner (browser-пул) — прямой API за
	// FAB. Маршрут /search в сайдкаре есть; парсер searchResultsV2 best-effort,
	// доводим по прод-логам. Без OZON_BROWSER_URL ScrapeSearch вернёт blocked.
	ozonBrowser := scraper.NewOzonScraper(scraper.OzonOptions{
		Mode:       "browser", // поиск/витрина Ozon доступны только через сайдкар-пул
		BrowserURL: getEnv("OZON_BROWSER_URL", ""),
		Logger:     log,
	})
	ozonSearch := scraper.NewOzonSearchScraper(ozonBrowser, getEnvInt("SEARCH_MAX_ITEMS_OZON", 60))
	// Витрина продавца Ozon (/seller/<slug-id>/) — тот же сайдкар (/seller), парсер
	// выдачи переиспользуется.
	ozonSeller := scraper.NewOzonSellerScraper(ozonBrowser, getEnvInt("SEARCH_MAX_ITEMS_OZON", 60))
	// AliExpress-поиск: JSON-API /aer-webapi/v1/search через тот же транспорт, что
	// у карточки (direct; ALI_PROXY_URL — опциональный фолбэк на рефреш cookie).
	aliSearch := scraper.NewAliexpressSearchScraper(
		scraper.NewAliexpressScraper(scraper.AliexpressOptions{
			ProxyURL: getEnv("ALI_PROXY_URL", getEnv("OZON_PROXY_URL", "")),
			RPS:      2,
			Logger:   log,
		}),
		getEnvInt("SEARCH_MAX_ITEMS_ALI", 80),
	)
	// X5SEC-фолбэк выдачи в браузер-сайдкар ali-miner (как WB выше): direct-POST
	// выдачи X5SEC режет с датацентр-IP, хотя карточный productData проходит.
	aliSearch.SetBrowserSidecar(getEnv("ALI_BROWSER_URL", ""), getEnvInt("ALI_SEARCH_BROWSER_MAX_PAGES", 1))
	registry := scraper.NewRegistry(wbSearch, wbSeller, yandexSearch, ozonSearch, ozonSeller, aliSearch)

	// ── Kafka ─────────────────────────────────────────────────────────────────
	consumer := kafka.NewConsumer(kafkaBrokers, tasksTopic, kafkaGroupID)
	defer consumer.Close()
	searchEvents := kafka.NewProducer(kafkaBrokers, "search-events")
	defer searchEvents.Close()

	sw := &searchWorker{
		log:             log.With("component", "search_worker"),
		registry:        registry,
		queries:         searchQueryRepo,
		subs:            searchSubRepo,
		results:         searchResultRepo,
		notifs:          searchNotifRepo,
		products:        productRepo,
		events:          searchEvents,
		defaultInterval: defaultInterval,
		// Анти-спам below_target на широких/ротирующихся выдачах (Ozon отдаёт
		// ~8 ротирующихся позиций → каждый скрейп новые дешёвые SKU). 0 — выкл.
		belowTargetCooldown: time.Duration(getEnvInt("SEARCH_BELOW_TARGET_COOLDOWN_MINUTES", 360)) * time.Minute,
	}

	log.Info("search-worker started",
		"topic", tasksTopic, "group", kafkaGroupID,
		"pool_prefix", poolPrefix, "pool_size", poolSize,
		"default_interval", defaultInterval.String())
	return consumer.Run(ctx, sw.makeHandler())
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}
