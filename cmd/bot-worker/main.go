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

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
	"gitlab.com/KosovAndrey/tryberrybot/internal/tracing"
	"gitlab.com/KosovAndrey/tryberrybot/internal/vk"
)

// bot-worker (Шаг 2a): потребляет telegram-updates и обрабатывает апдейты
// (HandleUpdate), отвечая в Telegram через HTTPS_PROXY. Реплицируемый: FSM
// диалогов лежит в Redis (общий для всех воркеров), а ingestor публикует апдейты
// с ключом = user ID, поэтому апдейты одного юзера попадают в одну партицию и
// обрабатываются по порядку. Чтобы поднять >1 реплики (Шаг 2b) — убрать
// container_name и `docker compose up --scale bot-worker=N`.

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	if err := run(log); err != nil {
		log.Error("bot-worker failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	_ = godotenv.Load()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	databaseURL := mustEnv("DATABASE_URL")
	redisURL := mustEnv("REDIS_URL")
	botToken := mustEnv("TELEGRAM_BOT_TOKEN")
	kafkaBrokers := strings.Split(mustEnv("KAFKA_BROKERS"), ",")
	groupID := getEnv("KAFKA_GROUP_ID", "bot-workers")
	healthPort := getEnv("BOT_WORKER_HEALTH_PORT", "8093")
	otlpEndpoint := getEnv("OTLP_ENDPOINT", "jaeger:4317")

	shutdownTracing, err := tracing.Init(ctx, "bot-worker", otlpEndpoint)
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
		// Redis нужен для FSM диалогов — без него ввод порога не работает, но
		// команды/ссылки обрабатываются. Логируем и продолжаем (как в api).
		log.Warn("redis unavailable", "err", err)
		redisClient = nil
	}

	go runHealthServer(ctx, log, pool, redisClient, healthPort)

	userRepo := postgres.NewUserRepo(pool)
	subRepo := postgres.NewSubscriptionRepo(pool)
	prodRepo := postgres.NewProductRepo(pool)
	searchQueryRepo := postgres.NewSearchQueryRepo(pool)
	searchSubRepo := postgres.NewSearchSubscriptionRepo(pool)
	promoRepo := postgres.NewPromoRepo(pool)
	referralRepo := postgres.NewReferralRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	billingRepo := postgres.NewBillingSubscriptionRepo(pool)

	var discounts *redisrepo.DiscountStore
	if redisClient != nil {
		discounts = redisrepo.NewDiscountStore(redisClient)
	}

	// Боту токен скрейпа не нужен — он зовёт только разбор URL
	// (FindByURL/NormalizeSearchURL), не ScrapeSearch. Идентично api.
	wbSearch := scraper.NewWildberriesSearchScraper(scraper.NewWildberriesScraper(5), nil, nil, 5, 0)
	registry := scraper.NewRegistry(
		wbSearch,
		// Бот при /track скрейпит сразу (показать товар) → Ozon нужен рабочим:
		// аккаунт-cookie (OZON_COOKIE) + тот же мобильный прокси, что у scraper.
		scraper.NewOzonScraper(scraper.OzonOptions{
			ProxyURL:     getEnv("OZON_PROXY_URL", ""),
			Mode:         getEnv("OZON_API_MODE", "mobile"),
			AccessToken:  getEnv("OZON_ACCESS_TOKEN", ""),
			RefreshToken: getEnv("OZON_REFRESH_TOKEN", ""),
			Cookie:       getEnv("OZON_COOKIE", ""),
			Logger:       log,
		}),
		scraper.NewYandexMarketScraper(2),
	)

	// getMe ходит наружу (через HTTPS_PROXY). Ретраим старт.
	bot, err := initWithRetry(ctx, log, func() (*telegram.Bot, error) {
		return telegram.NewBot(
			botToken, log, userRepo, subRepo, prodRepo, registry,
			searchQueryRepo, searchSubRepo, promoRepo, referralRepo, redisClient, parseAdminIDs(getEnv("ADMIN_IDS", "")),
			getEnv("VK_BOT_URL", ""),
		)
	})
	if err != nil {
		return fmt.Errorf("init bot: %w", err)
	}

	// ── VK-консьюмер (фаза 1: привязка аккаунтов + ответы в ЛС) ─────────────
	// Включается только при заданном VK_GROUP_TOKEN. vk.com доступен напрямую,
	// прокси не нужен.
	var vkBot *vk.Bot
	if vkToken := getEnv("VK_GROUP_TOKEN", ""); vkToken != "" {
		var linkCodes *redisrepo.LinkCodeStore
		if redisClient != nil {
			linkCodes = redisrepo.NewLinkCodeStore(redisClient)
		}
		vkBot = vk.NewBot(vk.NewClient(vkToken), log, userRepo, subRepo, prodRepo,
			searchQueryRepo, searchSubRepo, promoRepo, referralRepo, registry, linkCodes, redisClient,
			getEnv("VK_BOT_URL", ""))
		vkConsumer := kafka.NewConsumer(kafkaBrokers, "vk-updates", "vk-workers")
		defer vkConsumer.Close()
		go func() {
			log.Info("vk-updates consumer started")
			err := vkConsumer.Run(ctx, func(ctx context.Context, msg kafka.Message) error {
				ev, err := kafka.Decode[vk.CallbackEvent](msg)
				if err != nil {
					log.Error("decode vk update", "err", err)
					return nil // poison pill — пропускаем
				}
				vkBot.HandleEvent(ctx, ev)
				return nil
			})
			if err != nil {
				log.Error("vk-updates consumer stopped", "err", err)
			}
		}()
	}

	// Оплата ЮKassa: подключает платёжный сервис к витринам и запускает
	// консьюмер применения (no-op без конфигурации — витрина покажет заглушку).
	if payConsumer := setupPayments(ctx, log, kafkaBrokers, bot, vkBot,
		paymentRepo, promoRepo, referralRepo, userRepo, billingRepo, discounts); payConsumer != nil {
		defer payConsumer.Close()
	}

	consumer := kafka.NewConsumer(kafkaBrokers, "telegram-updates", groupID)
	defer consumer.Close()

	handler := func(ctx context.Context, msg kafka.Message) error {
		update, err := kafka.Decode[tgbotapi.Update](msg)
		if err != nil {
			log.Error("decode telegram update", "err", err)
			return nil // poison pill — пропускаем
		}
		bot.HandleUpdate(ctx, update)
		return nil
	}

	log.Info("bot-worker started, consuming telegram-updates...", "group", groupID)
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

func initWithRetry[T any](ctx context.Context, log *slog.Logger, build func() (T, error)) (T, error) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		v, err := build()
		if err == nil {
			if attempt > 1 {
				log.Info("telegram init ok after retries", "attempts", attempt)
			}
			return v, nil
		}
		log.Warn("telegram init failed (getMe), retrying",
			"attempt", attempt, "backoff", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			var zero T
			return zero, fmt.Errorf("cancelled after %d attempts: %w", attempt, err)
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
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

// parseAdminIDs — "123,456" → множество telegram_id админов.
func parseAdminIDs(s string) map[int64]bool {
	out := map[int64]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if id, err := strconv.ParseInt(part, 10, 64); err == nil {
			out[id] = true
		}
	}
	return out
}
