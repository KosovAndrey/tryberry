package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/tracing"

	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/searchsub"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(log); err != nil {
		log.Error("notifier failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	_ = godotenv.Load()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// ── Config ───────────────────────────────────────────────────────────────
	databaseURL := mustEnv("DATABASE_URL")
	redisURL := mustEnv("REDIS_URL")
	kafkaBrokers := strings.Split(mustEnv("KAFKA_BROKERS"), ",")
	kafkaGroupID := mustEnv("KAFKA_GROUP_ID")
	botToken := mustEnv("TELEGRAM_BOT_TOKEN")
	otlpEndpoint := mustEnv("OTLP_ENDPOINT")

	// ── Подключения ──────────────────────────────────────────────────────────
	shutdownTracing, err := tracing.Init(ctx, "notifier", otlpEndpoint)
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
		log.Warn("redis unavailable, running without cache", "err", err)
		redisClient = nil
	}

	go runHealthServer(ctx, log, pool, redisClient, "8091")

	// ── Репозитории ──────────────────────────────────────────────────────────
	subRepo := postgres.NewSubscriptionRepo(pool)
	notifRepo := postgres.NewNotificationRepo(pool)
	priceHistoryRepo := postgres.NewPriceHistoryRepo(pool)

	var priceCache *redisrepo.PriceCache
	if redisClient != nil {
		priceCache = redisrepo.NewPriceCache(redisClient)
	}

	// ── Telegram ─────────────────────────────────────────────────────────────
	tgNotifier := telegram.NewNotifier(botToken)

	// ── Kafka ────────────────────────────────────────────────────────────────
	consumer := kafka.NewConsumer(kafkaBrokers, "price-events", kafkaGroupID)
	defer consumer.Close()

	handler := makeHandler(log, subRepo, notifRepo, priceHistoryRepo, priceCache, tgNotifier)

	log.Info("notifier started, waiting for price events...")
	return consumer.Run(ctx, handler)
}

func makeHandler(
	log *slog.Logger,
	subRepo *postgres.SubscriptionRepo,
	notifRepo *postgres.NotificationRepo,
	priceHistoryRepo *postgres.PriceHistoryRepo,
	priceCache *redisrepo.PriceCache,
	tgNotifier *telegram.Notifier,
) kafka.HandlerFunc {
	return func(ctx context.Context, msg kafka.Message) error {
		event, err := kafka.Decode[domain.PriceEvent](msg)
		if err != nil {
			log.Error("decode price event", "err", err)
			return nil // poison pill — пропускаем
		}

		log := log.With("product_id", event.ProductID, "new_price", event.NewPrice)

		// Находим все активные подписки на этот товар
		subs, err := subRepo.GetActiveByProductIDWithTelegramID(ctx, event.ProductID)
		if err != nil {
			return fmt.Errorf("get subscriptions: %w", err)
		}
		if len(subs) == 0 {
			return nil
		}

		// Получаем актуальную цену (из Redis или PostgreSQL)
		currentPrice := event.NewPrice
		if priceCache != nil {
			if cached, err := priceCache.Get(ctx, event.ProductID); err == nil {
				currentPrice = cached
			}
		}
		if currentPrice == 0 {
			if p, _, err := priceHistoryRepo.GetLatest(ctx, event.ProductID); err == nil {
				currentPrice = p
			}
		}

		for _, sub := range subs {
			// Решение о срабатывании — общий движок поиск-подписок:
			//   first_seen_price  — база первого срабатывания (any_drop/discount_pct),
			//   baseline_price    — цена последнего уведомления (повторные срабатывания),
			//   notified          — фаза (первое vs повторное).
			rule := searchsub.RuleFromProductSub(sub)
			state := searchsub.ProductState{
				ProductID:           sub.ProductID,
				CurrentKopecks:      searchsub.Kopecks(currentPrice),
				BaselineKopecks:     searchsub.Kopecks(sub.FirstSeenPrice),
				LastNotifiedKopecks: searchsub.Kopecks(sub.BaselinePrice),
				HasNotified:         sub.Notified,
			}
			if !searchsub.Decide(rule, state) {
				continue
			}

			// Проверяем idempotency
			iKey := fmt.Sprintf("%d:%.2f:%s",
				sub.ID, event.NewPrice, event.RecordedAt.Format("2006-01-02T15:04:05Z"))

			exists, err := notifRepo.ExistsByKey(ctx, iKey)
			if err != nil {
				return fmt.Errorf("check idempotency: %w", err)
			}
			if exists {
				log.Info("notification already sent, skipping", "key", iKey)
				continue
			}

			// Отправляем уведомление в Telegram
			err = tgNotifier.SendPriceAlert(ctx, telegram.PriceAlert{
				ChatID:         sub.TelegramID,
				SubscriptionID: sub.ID,
				ProductName:    sub.ProductName,
				ProductURL:     sub.ProductURL,
				OldPrice:       sub.BaselinePrice,
				NewPrice:       event.NewPrice,
				ImageURL:       sub.ProductImageURL,
			})
			if err != nil {
				return fmt.Errorf("send telegram notification: %w", err)
			}

			// Фиксируем цену последнего уведомления (+ notified=TRUE)
			if err := subRepo.UpdateBaseline(ctx, sub.ID, event.NewPrice); err != nil {
				return fmt.Errorf("update baseline: %w", err)
			}

			// Записываем в notifications (idempotency)
			if err := notifRepo.Insert(ctx, &domain.Notification{
				SubscriptionID: sub.ID,
				OldPrice:       sub.BaselinePrice,
				NewPrice:       event.NewPrice,
				IdempotencyKey: iKey,
			}); err != nil {
				return fmt.Errorf("insert notification: %w", err)
			}
			metrics.NotificationsSent.WithLabelValues(event.Marketplace).Inc()
			log.Info("notification sent",
				"subscription_id", sub.ID,
				"user_id", sub.UserID,
				"trigger", string(sub.TriggerType),
				"old_price", sub.BaselinePrice,
				"new_price", event.NewPrice,
			)
		}

		return nil
	}
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
