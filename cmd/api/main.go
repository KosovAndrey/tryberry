package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"

	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/partition"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(log); err != nil {
		log.Error("api failed", "err", err)
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
	botToken := mustEnv("TELEGRAM_BOT_TOKEN")
	webhookURL := mustEnv("TELEGRAM_WEBHOOK_URL")
	port := getEnv("PORT", "8081")

	// ── Подключения ──────────────────────────────────────────────────────────
	pool, err := db.NewPostgresPool(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	redisClient, err := db.NewRedisClient(ctx, redisURL)
	if err != nil {
		log.Warn("redis unavailable", "err", err)
	}

	// ── Партиции ─────────────────────────────────────────────────────────────
	pm := partition.NewManager(pool)
	if err := pm.EnsurePartitions(ctx, 2); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}

	// ── Репозитории ──────────────────────────────────────────────────────────
	userRepo := postgres.NewUserRepo(pool)
	subRepo := postgres.NewSubscriptionRepo(pool)
	prodRepo := postgres.NewProductRepo(pool)

	_ = redisURL
	_ = redisClient

	// ── WB клиент ────────────────────────────────────────────────────────────
	registry := scraper.NewRegistry(
		scraper.NewWildberriesScraper(5),
		scraper.NewOzonScraper(), // заглушка
		scraper.NewYandexMarketScraper(2),
	)

	// ── Telegram бот ─────────────────────────────────────────────────────────
	bot, err := telegram.NewBot(botToken, log, userRepo, subRepo, prodRepo, registry)
	if err != nil {
		return fmt.Errorf("init bot: %w", err)
	}
	if err := bot.SetCommands(); err != nil {
		log.Warn("set commands failed", "err", err)
	}

	if getEnv("WEBHOOK_ENABLED", "true") == "true" {
		if err := bot.SetWebhook(webhookURL); err != nil {
			return fmt.Errorf("set webhook: %w", err)
		}
		log.Info("webhook set", "url", webhookURL)
	} else {
		log.Info("webhook disabled, skipping registration")
	}

	// ── HTTP сервер ───────────────────────────────────────────────────────────
	mux := http.NewServeMux()

	healthChecker := health.New(pool, redisClient)

	mux.Handle("/webhook", metrics.HTTPMiddleware("webhook")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var update tgbotapi.Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			log.Error("decode update", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bot.HandleUpdate(r.Context(), update)
		w.WriteHeader(http.StatusOK)
	})))

	mux.HandleFunc("/health", healthChecker.Handler())
	mux.HandleFunc("/live", health.LivenessHandler())
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	// Запускаем планировщик
	go runScheduler(ctx, log, pool, botToken)

	log.Info("api started", "port", port)

	go runMetricsUpdater(ctx, log, pool)

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func runMetricsUpdater(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool) {
	tick := func() {
		ctxQ, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		// Общие счётчики
		var total int
		if err := pool.QueryRow(ctxQ,
			`SELECT COUNT(*) FROM subscriptions WHERE active = TRUE`).Scan(&total); err == nil {
			metrics.ActiveSubscriptions.Set(float64(total))
		}

		var users int
		if err := pool.QueryRow(ctxQ,
			`SELECT COUNT(*) FROM users`).Scan(&users); err == nil {
			metrics.TotalUsers.Set(float64(users))
		}

		// Подписки по маркетплейсам
		rows, err := pool.Query(ctxQ, `
        SELECT p.marketplace, COUNT(*)
        FROM subscriptions s
        JOIN products p ON p.id = s.product_id
        WHERE s.active = TRUE
        GROUP BY p.marketplace`)
		if err == nil {
			defer rows.Close()
			// Сбрасываем чтобы убрать маркетплейсы у которых стало 0
			metrics.SubscriptionsByMarketplace.Reset()
			for rows.Next() {
				var mp string
				var count int
				if err := rows.Scan(&mp, &count); err == nil {
					metrics.SubscriptionsByMarketplace.WithLabelValues(mp).Set(float64(count))
				}
			}
		}
	}

	tick()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
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
