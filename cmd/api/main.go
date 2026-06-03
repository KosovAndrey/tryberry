package main

import (
	"context"
	"encoding/json"
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
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/tracing"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

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

	databaseURL := mustEnv("DATABASE_URL")
	redisURL := mustEnv("REDIS_URL")
	botToken := mustEnv("TELEGRAM_BOT_TOKEN")
	// В polling-режиме webhook URL не нужен, поэтому больше не mustEnv.
	webhookURL := getEnv("TELEGRAM_WEBHOOK_URL", "")
	port := getEnv("PORT", "8081")
	otlpEndpoint := getEnv("OTLP_ENDPOINT", "jaeger:4317")

	shutdownTracing, err := tracing.Init(ctx, "api", otlpEndpoint)
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
		log.Warn("redis unavailable", "err", err)
		redisClient = nil
	}

	pm := partition.NewManager(pool)
	if err := pm.EnsurePartitions(ctx, 2); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}

	userRepo := postgres.NewUserRepo(pool)
	subRepo := postgres.NewSubscriptionRepo(pool)
	prodRepo := postgres.NewProductRepo(pool)
	searchQueryRepo := postgres.NewSearchQueryRepo(pool)
	searchSubRepo := postgres.NewSearchSubscriptionRepo(pool)

	// Search-скрейпер встраивает товарный, поэтому служит и FindByURL (товар),
	// и FindSearchByURL (выдача). Боту токен не нужен — он зовёт только разбор
	// URL (NormalizeSearchURL/MatchesSearch), не ScrapeSearch.
	wbSearch := scraper.NewWildberriesSearchScraper(scraper.NewWildberriesScraper(5), nil, nil, 5, 0)
	registry := scraper.NewRegistry(
		wbSearch,
		scraper.NewOzonScraper(),
		scraper.NewYandexMarketScraper(2),
	)

	// getMe внутри NewBot ходит наружу к Telegram. На RU-хостинге канал флапает
	// (РКН-троттлинг), поэтому единичный таймаут НЕ должен ронять сервис в петлю
	// рестартов — ретраим с backoff'ом до успеха или отмены ctx.
	bot, err := newBotWithRetry(ctx, log, func() (*telegram.Bot, error) {
		return telegram.NewBot(
			botToken, log, userRepo, subRepo, prodRepo, registry,
			searchQueryRepo, searchSubRepo, redisClient, parseAdminIDs(getEnv("ADMIN_IDS", "")),
		)
	})
	if err != nil {
		return fmt.Errorf("init bot: %w", err)
	}
	if err := bot.SetCommands(); err != nil {
		log.Warn("set commands failed", "err", err)
	}

	// WEBHOOK_ENABLED=true  → webhook (Telegram стучится к нам; нужен доступный
	//                          входящий путь Telegram→RU-IP).
	// WEBHOOK_ENABLED=false → long-polling (бот сам ходит наружу через прокси).
	//                          Используем, пока РКН режет входящие webhook'и.
	if getEnv("WEBHOOK_ENABLED", "true") == "true" {
		if webhookURL == "" {
			return fmt.Errorf("WEBHOOK_ENABLED=true requires TELEGRAM_WEBHOOK_URL")
		}
		if err := bot.SetWebhook(webhookURL); err != nil {
			return fmt.Errorf("set webhook: %w", err)
		}
		log.Info("webhook set", "url", webhookURL)
	} else {
		// Polling крутится в фоне; HTTP-сервер ниже продолжает отдавать
		// /health, /metrics, /live. Маршрут /webhook остаётся, но не задействован.
		go func() {
			if err := bot.RunPolling(ctx); err != nil {
				log.Error("polling stopped with error", "err", err)
			}
		}()
		log.Info("polling mode enabled (getUpdates via proxy)")
	}

	mux := http.NewServeMux()
	healthChecker := health.New(pool, redisClient)

	webhookHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.Handle("/webhook", metrics.HTTPMiddleware("webhook")(
		otelhttp.NewHandler(webhookHandler, "webhook"),
	))
	mux.HandleFunc("/health", healthChecker.Handler())
	mux.HandleFunc("/live", health.LivenessHandler())
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{Addr: ":" + port, Handler: mux}

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

// newBotWithRetry повторяет инициализацию бота (getMe ходит наружу к Telegram)
// с экспоненциальным backoff'ом. Возвращает ошибку только при отмене ctx —
// иначе временная недоступность Telegram (флап РКН/прокси) не валит api, а ждёт
// восстановления канала. Это устраняет петлю рестартов на старте.
func newBotWithRetry(ctx context.Context, log *slog.Logger, build func() (*telegram.Bot, error)) (*telegram.Bot, error) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		bot, err := build()
		if err == nil {
			if attempt > 1 {
				log.Info("telegram init ok after retries", "attempts", attempt)
			}
			return bot, nil
		}
		log.Warn("telegram init failed (getMe), retrying",
			"attempt", attempt, "backoff", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("cancelled after %d attempts: %w", attempt, err)
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

func runMetricsUpdater(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool) {
	tick := func() {
		ctxQ, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

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

		rows, err := pool.Query(ctxQ, `
        SELECT p.marketplace, COUNT(*)
        FROM subscriptions s
        JOIN products p ON p.id = s.product_id
        WHERE s.active = TRUE
        GROUP BY p.marketplace`)
		if err == nil {
			defer rows.Close()
			metrics.SubscriptionsByMarketplace.Reset()
			for rows.Next() {
				var mp string
				var count int
				if err := rows.Scan(&mp, &count); err == nil {
					metrics.SubscriptionsByMarketplace.WithLabelValues(mp).Set(float64(count))
				}
			}
		}

		var searchSubs int
		if err := pool.QueryRow(ctxQ,
			`SELECT COUNT(*) FROM search_subscriptions WHERE active = TRUE`).Scan(&searchSubs); err == nil {
			metrics.ActiveSearchSubscriptions.Set(float64(searchSubs))
		}

		planRows, err := pool.Query(ctxQ,
			`SELECT plan, COUNT(*) FROM users GROUP BY plan`)
		if err == nil {
			defer planRows.Close()
			metrics.UsersByPlan.Reset()
			for planRows.Next() {
				var plan string
				var count int
				if err := planRows.Scan(&plan, &count); err == nil {
					metrics.UsersByPlan.WithLabelValues(plan).Set(float64(count))
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
