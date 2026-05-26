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

	var priceCache *redisrepo.PriceCache
	if redisClient != nil {
		priceCache = redisrepo.NewPriceCache(redisClient)
	}

	// ── Kafka ────────────────────────────────────────────────────────────────
	consumer := kafka.NewConsumer(kafkaBrokers, "scrape-tasks", kafkaGroupID)
	defer consumer.Close()

	producer := kafka.NewProducer(kafkaBrokers, "price-events")
	defer producer.Close()

	// ── WB клиент ────────────────────────────────────────────────────────────
	registry := scraper.NewRegistry(
		scraper.NewWildberriesScraper(rpsWB),
		scraper.NewOzonScraper(),
		scraper.NewYandexMarketScraper(rpsYandex),
	)
	// ── Обработчик сообщений ─────────────────────────────────────────────────
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
