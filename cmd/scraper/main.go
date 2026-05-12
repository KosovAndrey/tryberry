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

	"github.com/joho/godotenv"
	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
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
	rpsStr := getEnv("SCRAPER_RATE_LIMIT_RPS", "5")
	rps, err := strconv.ParseFloat(rpsStr, 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS: %w", err)
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
	wbClient := scraper.NewClient(rps)

	// ── Обработчик сообщений ─────────────────────────────────────────────────
	handler := makeHandler(log, wbClient, productRepo, priceHistoryRepo, priceCache, producer)

	log.Info("scraper started, waiting for tasks...")
	return consumer.Run(ctx, handler)
}

func makeHandler(
	log *slog.Logger,
	wbClient *scraper.Client,
	productRepo *postgres.ProductRepo,
	priceHistoryRepo *postgres.PriceHistoryRepo,
	priceCache *redisrepo.PriceCache,
	producer *kafka.Producer,
) kafka.HandlerFunc {
	return func(ctx context.Context, msg kafka.Message) error {
		task, err := kafka.Decode[domain.ScrapeTask](msg)
		if err != nil {
			// Не можем распарсить — пропускаем (poison pill)
			log.Error("decode scrape task", "err", err)
			return nil
		}

		log := log.With("product_id", task.ProductID, "url", task.URL)

		// Извлекаем артикул из URL
		articleID, err := scraper.ExtractArticleID(task.URL)
		if err != nil {
			log.Error("extract article id", "err", err)
			return nil // не ретраить — URL невалидный
		}

		// Скрейпим товар
		result, err := wbClient.Scrape(ctx, articleID)
		if err != nil {
			log.Error("scrape failed", "err", err)
			return err // ретраить
		}

		log.Info("scraped", "name", result.Name, "price", result.Price)

		// Обновляем name и image_url в products
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
				ProductID: task.ProductID,
				OldPrice:  prevPrice,
				NewPrice:  result.Price,
			}
			key := strconv.FormatInt(task.ProductID, 10)
			if err := producer.Send(ctx, key, event); err != nil {
				return fmt.Errorf("send price event: %w", err)
			}
			log.Info("price changed, event sent",
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
