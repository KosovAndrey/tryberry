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
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
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

	databaseURL := mustEnv("DATABASE_URL")
	redisURL := mustEnv("REDIS_URL")
	kafkaBrokers := strings.Split(mustEnv("KAFKA_BROKERS"), ",")
	kafkaGroupID := mustEnv("KAFKA_GROUP_ID")
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

	go runHealthServer(ctx, log, pool, redisClient, healthPort)

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

	wbSearch := scraper.NewWildberriesSearchScraper(
		scraper.NewWildberriesScraper(rpsWB),
		proxyPool,
		tokenProvider,
		getEnvInt("SEARCH_MAX_PAGES", 5),
		time.Duration(getEnvInt("SEARCH_PAGE_DELAY_MS", 700))*time.Millisecond,
	)
	// Я.Маркет-поиск: тот же транспорт, что у карточки (tls-client + RU-прокси).
	yandexSearch := scraper.NewYandexMarketSearchScraper(
		scraper.NewYandexMarketScraper(scraper.YandexMarketOptions{
			ProxyURL: getEnv("YANDEX_PROXY_URL", getEnv("OZON_PROXY_URL", "")),
			RPS:      2,
			Logger:   log,
		}),
		getEnvInt("SEARCH_MAX_ITEMS_YANDEX", 60),
	)
	// Ozon-поиск: пока за FAB-блоком + у сайдкара нет search-маршрута
	// (ScrapeSearch → ErrMarketplaceBlocked). Регистрируем для распознавания URL.
	ozonSearch := scraper.NewOzonSearchScraper(scraper.NewOzonScraper(scraper.OzonOptions{
		ProxyURL:   getEnv("OZON_PROXY_URL", ""),
		Mode:       getEnv("OZON_API_MODE", "mobile"),
		BrowserURL: getEnv("OZON_BROWSER_URL", ""),
		Logger:     log,
	}))
	registry := scraper.NewRegistry(wbSearch, yandexSearch, ozonSearch)

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
	}

	log.Info("search-worker started",
		"topic", tasksTopic, "group", kafkaGroupID,
		"pool_prefix", poolPrefix, "pool_size", poolSize,
		"default_interval", defaultInterval.String())
	return consumer.Run(ctx, sw.makeHandler())
}

func runHealthServer(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool, redisClient *redis.Client, port string) {
	healthChecker := health.New(pool, redisClient)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthChecker.Handler())
	mux.HandleFunc("/live", health.LivenessHandler())
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{Addr: ":" + port, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	log.Info("health server started", "port", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("health server", "err", err)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("env %s is required", key))
	}
	return v
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
