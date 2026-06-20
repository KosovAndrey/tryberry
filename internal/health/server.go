package health

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

// RunServer поднимает общий health/metrics-сервер сервиса на :port и блокируется
// до отмены ctx (затем gracefully гасит). Эндпоинты: /health (БД+Redis), /live
// (liveness), /metrics (Prometheus). Раньше эта функция копипастилась в каждом
// cmd/* (scraper/bot-worker/search-worker/notifier/scheduler) — единый источник.
func RunServer(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool, redisClient *redis.Client, port string) {
	checker := New(pool, redisClient)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/live", LivenessHandler())
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{Addr: ":" + port, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background()) //nolint:errcheck // best-effort shutdown
	}()

	log.Info("health server started", "port", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("health server", "err", err)
	}
}
