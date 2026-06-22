package main

import (
	"context"
	"errors"
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
	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/partition"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
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
	databaseURL := config.MustEnv("DATABASE_URL")
	redisURL := config.MustEnv("REDIS_URL")
	kafkaBrokers := strings.Split(config.MustEnv("KAFKA_BROKERS"), ",")
	kafkaGroupID := config.MustEnv("KAFKA_GROUP_ID")
	otlpEndpoint := getEnv("OTLP_ENDPOINT", "jaeger:4317")
	rpsStrWB := getEnv("SCRAPER_RATE_LIMIT_RPS_WB", "5")
	rpsStrYandex := getEnv("SCRAPER_RATE_LIMIT_RPS_YANDEX", "2")
	rpsStrOzon := getEnv("SCRAPER_RATE_LIMIT_RPS_OZON", "1")
	rpsWB, err := strconv.ParseFloat(rpsStrWB, 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS_WB: %w", err)
	}
	rpsYandex, err := strconv.ParseFloat(rpsStrYandex, 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS_YANDEX: %w", err)
	}
	rpsOzon, err := strconv.ParseFloat(rpsStrOzon, 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS_OZON: %w", err)
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

	// Периодически до-создаём партиции: EnsurePartitions на старте покрывает +2 мес,
	// но при длинном аптайме (>2 мес без рестарта) партиция нового месяца не появится
	// и INSERT в price_history упадёт. Суточный тик держит окно открытым.
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := pm.EnsurePartitions(ctx, 2); err != nil {
					log.Error("ensure partitions (periodic)", "err", err)
				} else {
					log.Info("partitions ensured (periodic)")
				}
			}
		}
	}()

	go health.RunServer(ctx, log, pool, redisClient, "8090")

	// ── Репозитории ──────────────────────────────────────────────────────────
	productRepo := postgres.NewProductRepo(pool)
	priceHistoryRepo := postgres.NewPriceHistoryRepo(pool)

	var priceCache *redisrepo.PriceCache
	if redisClient != nil {
		priceCache = redisrepo.NewPriceCache(redisClient)
	}

	// ── Kafka ────────────────────────────────────────────────────────────────
	// Товарный путь: scrape-tasks → (этот consumer) → price-events.
	// Поисковый путь вынесен в отдельный бинарь cmd/search-worker.
	consumer := kafka.NewConsumer(kafkaBrokers, "scrape-tasks", kafkaGroupID)
	defer consumer.Close()

	producer := kafka.NewProducer(kafkaBrokers, "price-events")
	defer producer.Close()

	// ── Скрейперы (товарные карточки) ──────────────────────────────────────────
	wbCard := scraper.NewWildberriesScraper(rpsWB)
	if redisClient != nil {
		wbCard.SetBasketResolver(redisrepo.NewBasketCache(redisClient))
	}
	registry := scraper.NewRegistry(
		wbCard,
		scraper.NewOzonScraper(scraper.OzonOptions{
			ProxyURL:     getEnv("OZON_PROXY_URL", ""),
			RPS:          rpsOzon,
			Mode:         getEnv("OZON_API_MODE", "mobile"),
			AccessToken:  getEnv("OZON_ACCESS_TOKEN", ""),
			RefreshToken: getEnv("OZON_REFRESH_TOKEN", ""),
			Cookie:       getEnv("OZON_COOKIE", ""),
			BrowserURL:   getEnv("OZON_BROWSER_URL", ""),
			Logger:       log,
		}),
		scraper.NewYandexMarketScraper(scraper.YandexMarketOptions{
			// Без аккаунта: хороший TLS + RU-прокси. По умолчанию переиспользуем
			// мобильный прокси Ozon (один IP). ВНИМАНИЕ: дележ IP ускоряет его
			// выгорание (см. OZON-STATUS) — при росте нагрузки задать отдельный.
			ProxyURL: getEnv("YANDEX_PROXY_URL", getEnv("OZON_PROXY_URL", "")),
			RPS:      rpsYandex,
			Logger:   log,
		}),
	)

	// ── Обработчик сообщений (цены) — блокирующий основной цикл ────────────────
	handler := makeHandler(log, registry, productRepo, priceHistoryRepo, priceCache, producer)

	log.Info("scraper started, waiting for tasks...")
	return consumer.Run(ctx, handler)
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
			// Перманентные ошибки (товар не найден / битый URL) не ретраим: иначе
			// один нескрейпящийся товар (удалённый / трансгран без цены на этом
			// egress) застревает в петле и лагает весь консьюмер. Пропускаем —
			// следующая плановая задача по этому товару попробует снова.
			if errors.Is(err, scraper.ErrProductNotFound) || errors.Is(err, scraper.ErrInvalidURL) {
				log.Warn("scrape skipped (permanent)", "err", err)
				return nil
			}
			log.Error("scrape failed", "err", err)
			return err
		}
		log.Info("scraped", "marketplace", marketplace, "name", result.Name, "price", result.Price)

		// Обновляем product (имя/картинка/наличие); получаем ПРЕДЫДУЩЕЕ наличие для
		// детекта перехода «нет в наличии»→«появилось» (триггер back_in_stock).
		wasInStock, err := productRepo.UpdateScrapedData(ctx, task.ProductID, result.Name, result.ImageURL, result.InStock)
		if err != nil {
			return fmt.Errorf("update product: %w", err)
		}

		// Получаем предыдущую цену (из Redis или PostgreSQL)
		prevPrice, err := getPrevPrice(ctx, log, task.ProductID, priceCache, priceHistoryRepo)
		if err != nil {
			log.Warn("could not get prev price, skipping event", "err", err)
		}

		// price_history/кэш обновляем ТОЛЬКО когда товар в наличии: запись нулевой
		// цены для OOS засорила бы аналитику и дала ложный price drop. Событие шлём
		// всегда — notifier обрабатывает и появление в наличии, и снижение цены.
		if result.InStock {
			// CHANGE-ONLY: пишем в историю лишь при СМЕНЕ цены. При 1-мин кадансе
			// reseller хранить идентичные точки расточительно (~99% дублей); сегмент
			// «цена X действует с t0» восстанавливаем на чтении (PriceHistoryRepo.Stats
			// взвешивает по длительности). prevPrice<=0 → первая точка по товару.
			// Кэш последней цены обновляем ВСЕГДА — на нём держится детект снижения.
			if prevPrice <= 0 || result.Price != prevPrice {
				if err := priceHistoryRepo.Insert(ctx, task.ProductID, result.Price); err != nil {
					return fmt.Errorf("insert price history: %w", err)
				}
			}
			if priceCache != nil {
				if err := priceCache.Set(ctx, task.ProductID, result.Price); err != nil {
					log.Warn("redis set failed", "err", err)
				}
			}
		}

		// Публикуем событие на КАЖДОМ скрейпе (не только при изменении цены):
		// строгая per-plan модель требует, чтобы notifier мог оценить подписку на
		// её чек-поинте по текущей цене. Иначе free-подписчик (60 мин) пропустит
		// устойчивое падение, случившееся между событиями «по изменению». Частоту
		// доставки режет throttle (last_evaluated_at) в notifier.
		// При OOS result.Price может быть «последней» ценой из стейта — в событие её
		// НЕ кладём (NewPrice=0): notifier выводит наличие в т.ч. из NewPrice>0
		// (страховка для старых событий), и ненулевая last-цена ложно пометила бы
		// товар «в наличии», сломав триггер back_in_stock.
		newPrice := result.Price
		if !result.InStock {
			newPrice = 0
		}
		event := domain.PriceEvent{
			ProductID:   task.ProductID,
			Marketplace: string(marketplace),
			OldPrice:    prevPrice,
			NewPrice:    newPrice,
			RecordedAt:  time.Now(),
			InStock:     result.InStock,
			WasInStock:  wasInStock,
		}
		key := strconv.FormatInt(task.ProductID, 10)
		if err := producer.Send(ctx, key, event); err != nil {
			return fmt.Errorf("send price event: %w", err)
		}
		if result.InStock && prevPrice > 0 && result.Price < prevPrice {
			metrics.PriceDrops.WithLabelValues(string(marketplace)).Inc()
			log.Info("price dropped",
				"marketplace", marketplace, "old_price", prevPrice, "new_price", result.Price)
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

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
