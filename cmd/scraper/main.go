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
	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/partition"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
	"gitlab.com/KosovAndrey/tryberrybot/internal/searchsub"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
	"gitlab.com/KosovAndrey/tryberrybot/internal/tracing"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(log); err != nil {
		log.Error("scraper failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	_ = godotenv.Load()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// ── Config из env ────────────────────────────────────────────────────────
	databaseURL := mustEnv("DATABASE_URL")
	redisURL := mustEnv("REDIS_URL")
	kafkaBrokers := strings.Split(mustEnv("KAFKA_BROKERS"), ",")
	kafkaGroupID := mustEnv("KAFKA_GROUP_ID")
	otlpEndpoint := getEnv("OTLP_ENDPOINT", "jaeger:4317")
	rpsStrWB := getEnv("SCRAPER_RATE_LIMIT_RPS_WB", "5")
	rpsStrYandex := getEnv("SCRAPER_RATE_LIMIT_RPS_YANDEX", "2")
	rpsWB, err := strconv.ParseFloat(rpsStrWB, 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS_WB: %w", err)
	}
	rpsYandex, err := strconv.ParseFloat(rpsStrYandex, 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS_YANDEX: %w", err)
	}

	// ── Подключения ──────────────────────────────────────────────────────────

	shutdownTracing, err := tracing.Init(ctx, "scraper", otlpEndpoint)
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
		// Redis недоступен — работаем без кэша, только логируем
		log.Warn("redis unavailable, running without cache", "err", err)
		redisClient = nil
	}

	// ── Автосоздание партиций ────────────────────────────────────────────────
	pm := partition.NewManager(pool)
	if err := pm.EnsurePartitions(ctx, 2); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}
	log.Info("partitions ok")

	go runHealthServer(ctx, log, pool, redisClient, "8090")

	// ── Репозитории ──────────────────────────────────────────────────────────
	productRepo := postgres.NewProductRepo(pool)
	priceHistoryRepo := postgres.NewPriceHistoryRepo(pool)

	searchQueryRepo := postgres.NewSearchQueryRepo(pool)
	searchSubRepo := postgres.NewSearchSubscriptionRepo(pool)
	searchResultRepo := postgres.NewSearchResultRepo(pool)
	searchNotifRepo := postgres.NewSearchNotificationRepo(pool)

	var priceCache *redisrepo.PriceCache
	if redisClient != nil {
		priceCache = redisrepo.NewPriceCache(redisClient)
	}

	// ── Kafka ────────────────────────────────────────────────────────────────
	consumer := kafka.NewConsumer(kafkaBrokers, "scrape-tasks", kafkaGroupID)
	defer consumer.Close()

	producer := kafka.NewProducer(kafkaBrokers, "price-events")
	defer producer.Close()

	// ── Скрейперы ─────────────────────────────────────────────────────────────
	// WB-поиск аутентифицируется cookie-токеном wbaas из Redis (обновляется
	// вручную через scripts/wb-token-update.sh). Search-скрейпер встраивает
	// товарный, поэтому обслуживает и карточки, и выдачи.
	tokenProvider := scraper.TokenProviderFunc(func(ctx context.Context) (scraper.SearchToken, error) {
		if redisClient == nil {
			return scraper.SearchToken{}, fmt.Errorf("redis unavailable")
		}
		cookie, err := redisClient.Get(ctx, "wb:search:cookie").Result()
		if err != nil {
			return scraper.SearchToken{}, err
		}
		ua, _ := redisClient.Get(ctx, "wb:search:ua").Result()
		return scraper.SearchToken{Cookie: cookie, UserAgent: ua}, nil
	})

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

	registry := scraper.NewRegistry(
		wbSearch,
		scraper.NewOzonScraper(),
		scraper.NewYandexMarketScraper(rpsYandex),
	)

	// ── Поиск-планировщик (тикер рядом с consumer'ом цен) ─────────────────────
	var searchNotifier searchsub.SearchNotifier
	if tok := os.Getenv("TELEGRAM_BOT_TOKEN"); tok != "" {
		searchNotifier = tgSearchNotifier{
			n:    telegram.NewNotifier(tok),
			topN: getEnvInt("SEARCH_NOTIFY_TOP_N", 10),
		}
		log.Info("search notifications via Telegram")
	} else {
		searchNotifier = logNotifier{log: log.With("component", "search_notify")}
		log.Warn("TELEGRAM_BOT_TOKEN not set — search notifications go to log only")
	}

	sl := &searchLoop{
		log:      log.With("component", "search_loop"),
		registry: registry,
		queries:  searchQueryRepo,
		subs:     searchSubRepo,
		results:  searchResultRepo,
		notifs:   searchNotifRepo,
		products: productRepo,
		notifier: searchNotifier,
	}
	searchInterval := time.Duration(getEnvInt("SEARCH_SCRAPE_INTERVAL_MINUTES", 30)) * time.Minute
	go runSearchLoop(ctx, sl, searchInterval)

	// ── Обработчик сообщений (цены) ──────────────────────────────────────────
	handler := makeHandler(log, registry, productRepo, priceHistoryRepo, priceCache, producer)

	log.Info("scraper started, waiting for tasks...")
	return consumer.Run(ctx, handler)
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

func makeHandler(
	log *slog.Logger,
	registry *scraper.Registry,
	productRepo *postgres.ProductRepo,
	priceHistoryRepo *postgres.PriceHistoryRepo,
	priceCache *redisrepo.PriceCache,
	producer *kafka.Producer,
) kafka.HandlerFunc {
	return func(ctx context.Context, msg kafka.Message) error {
		task, err := kafka.Decode[domain.ScrapeTask](msg)
		if err != nil {
			log.Error("decode scrape task", "err", err)
			return nil
		}

		log := log.With("product_id", task.ProductID, "url", task.URL)

		// Скрейпим через registry — он сам выбирает нужный маркетплейс
		result, marketplace, err := registry.Scrape(ctx, task.URL)
		if err != nil {
			log.Error("scrape failed", "err", err)
			return err
		}
		log.Info("scraped", "marketplace", marketplace, "name", result.Name, "price", result.Price)

		// Обновляем product с маркетплейсом
		if err := productRepo.UpdateScrapedData(ctx, task.ProductID, result.Name, result.ImageURL); err != nil {
			return fmt.Errorf("update product: %w", err)
		}

		// Получаем предыдущую цену (из Redis или PostgreSQL)
		prevPrice, err := getPrevPrice(ctx, log, task.ProductID, priceCache, priceHistoryRepo)
		if err != nil {
			log.Warn("could not get prev price, skipping event", "err", err)
		}

		// Сохраняем новую цену в историю
		if err := priceHistoryRepo.Insert(ctx, task.ProductID, result.Price); err != nil {
			return fmt.Errorf("insert price history: %w", err)
		}

		// Обновляем Redis кэш
		if priceCache != nil {
			if err := priceCache.Set(ctx, task.ProductID, result.Price); err != nil {
				log.Warn("redis set failed", "err", err)
			}
		}

		// Если цена изменилась — публикуем событие
		if prevPrice > 0 && result.Price != prevPrice {
			event := domain.PriceEvent{
				ProductID:   task.ProductID,
				Marketplace: string(marketplace),
				OldPrice:    prevPrice,
				NewPrice:    result.Price,
			}
			key := strconv.FormatInt(task.ProductID, 10)
			if err := producer.Send(ctx, key, event); err != nil {
				return fmt.Errorf("send price event: %w", err)
			}
			if result.Price < prevPrice {
				metrics.PriceDrops.WithLabelValues(string(marketplace)).Inc()
			}
			log.Info("price changed, event sent",
				"marketplace", marketplace,
				"old_price", prevPrice,
				"new_price", result.Price,
			)
		}

		return nil
	}
}

// getPrevPrice — получить предыдущую цену из Redis, fallback на PostgreSQL
func getPrevPrice(
	ctx context.Context,
	log *slog.Logger,
	productID int64,
	cache *redisrepo.PriceCache,
	histRepo *postgres.PriceHistoryRepo,
) (float64, error) {
	if cache != nil {
		price, err := cache.Get(ctx, productID)
		if err == nil {
			return price, nil
		}
		log.Debug("redis miss, fallback to postgres", "product_id", productID)
	}

	price, _, err := histRepo.GetLatest(ctx, productID)
	return price, err
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
