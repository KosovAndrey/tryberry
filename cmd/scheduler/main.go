// cmd/scheduler/main.go
//
// scheduler — СИНГЛТОН-планировщик задач скрейпинга.
//
// Должен работать ровно в ОДНОМ процессе (replicas=1) — иначе пойдут дубли
// задач в Kafka. Благодаря этому api/scraper/notifier реплицируются свободно.
//
// Два независимых тикера:
//   • товарный  → топик scrape-tasks  (интервал SCRAPE_INTERVAL_MINUTES)
//   • поисковый → топик search-tasks  (интервал SEARCH_SCRAPE_INTERVAL_MINUTES)
// Поисковый тикер (M1b) заменил in-process runSearchLoop, который раньше жил в
// scraper и мешал его репликации.
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

	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	"gitlab.com/KosovAndrey/tryberrybot/internal/tracing"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(log); err != nil {
		log.Error("scheduler failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	_ = godotenv.Load()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	databaseURL := mustEnv("DATABASE_URL")
	brokers := strings.Split(mustEnv("KAFKA_BROKERS"), ",")
	otlpEndpoint := getEnv("OTLP_ENDPOINT", "jaeger:4317")
	healthPort := getEnv("SCHEDULER_HEALTH_PORT", "8092")
	productInterval := time.Duration(getEnvInt("SCRAPE_INTERVAL_MINUTES", 15)) * time.Minute
	searchInterval := time.Duration(getEnvInt("SEARCH_SCRAPE_INTERVAL_MINUTES", 20)) * time.Minute

	shutdownTracing, err := tracing.Init(ctx, "scheduler", otlpEndpoint)
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

	go runHealthServer(ctx, log, pool, healthPort)

	productRepo := postgres.NewProductRepo(pool)
	searchQueryRepo := postgres.NewSearchQueryRepo(pool)

	productProducer := kafka.NewProducer(brokers, "scrape-tasks")
	defer productProducer.Close()
	searchProducer := kafka.NewProducer(brokers, "search-tasks")
	defer searchProducer.Close()

	log.Info("scheduler started",
		"product_interval", productInterval.String(),
		"search_interval", searchInterval.String())

	go runProductScheduler(ctx, log, productRepo, productProducer, productInterval)
	go runSearchScheduler(ctx, log, searchQueryRepo, searchProducer, searchInterval)

	<-ctx.Done()
	return nil
}

// ── Товарный планировщик (перенесён из cmd/api/scheduler.go, M1a) ────────────

func runProductScheduler(
	ctx context.Context,
	log *slog.Logger,
	productRepo *postgres.ProductRepo,
	producer *kafka.Producer,
	interval time.Duration,
) {
	tick := func() {
		if err := productSchedulerTick(ctx, log, productRepo, producer); err != nil {
			log.Error("product scheduler tick failed", "err", err)
		}
	}
	tick()
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			tick()
		}
	}
}

func productSchedulerTick(
	ctx context.Context,
	log *slog.Logger,
	productRepo *postgres.ProductRepo,
	producer *kafka.Producer,
) error {
	productIDs, err := productRepo.GetActiveProductIDs(ctx)
	if err != nil {
		return fmt.Errorf("get active product ids: %w", err)
	}
	if len(productIDs) == 0 {
		log.Info("scheduler: no active products")
		return nil
	}

	sent := 0
	for _, id := range productIDs {
		product, err := productRepo.GetByID(ctx, id)
		if err != nil {
			log.Error("get product", "id", id, "err", err)
			continue
		}
		task := domain.ScrapeTask{ProductID: product.ID, URL: product.URL}
		key := strconv.FormatInt(product.ID, 10)
		if err := producer.Send(ctx, key, task); err != nil {
			log.Error("send scrape task", "product_id", id, "err", err)
			continue
		}
		sent++
	}
	log.Info("scheduler tick done", "total", len(productIDs), "sent", sent)
	return nil
}

// ── Поисковый планировщик (M1b: заменяет in-process runSearchLoop) ───────────

func runSearchScheduler(
	ctx context.Context,
	log *slog.Logger,
	queryRepo *postgres.SearchQueryRepo,
	producer *kafka.Producer,
	interval time.Duration,
) {
	tick := func() {
		if err := searchSchedulerTick(ctx, log, queryRepo, producer); err != nil {
			log.Error("search scheduler tick failed", "err", err)
		}
	}
	tick()
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			tick()
		}
	}
}

func searchSchedulerTick(
	ctx context.Context,
	log *slog.Logger,
	queryRepo *postgres.SearchQueryRepo,
	producer *kafka.Producer,
) error {
	queries, err := queryRepo.GetScrapable(ctx)
	if err != nil {
		return fmt.Errorf("get scrapable: %w", err)
	}
	if len(queries) == 0 {
		log.Info("search scheduler: no scrapable queries")
		return nil
	}

	sent := 0
	for _, q := range queries {
		task := domain.SearchTask{
			QueryID:   q.ID,
			URL:       q.NormalizedURL,
			QueryText: q.QueryText,
		}
		key := strconv.FormatInt(q.ID, 10)
		if err := producer.Send(ctx, key, task); err != nil {
			log.Error("send search task", "query_id", q.ID, "err", err)
			continue
		}
		sent++
	}
	log.Info("search scheduler tick done", "total", len(queries), "sent", sent)
	return nil
}

func runHealthServer(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool, port string) {
	healthChecker := health.New(pool, nil)
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
