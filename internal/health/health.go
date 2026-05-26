package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Checker struct {
	db    *pgxpool.Pool
	redis *redis.Client
}

func New(db *pgxpool.Pool, redis *redis.Client) *Checker {
	return &Checker{db: db, redis: redis}
}

type Status struct {
	Status   string            `json:"status"`
	Services map[string]string `json:"services"`
}

// Handler — основной /health endpoint. Проверяет PostgreSQL и Redis.
// Возвращает 200 если всё ок, 503 если что-то отвалилось.
func (c *Checker) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status := Status{
			Status:   "ok",
			Services: make(map[string]string),
		}

		if c.db != nil {
			if err := c.db.Ping(ctx); err != nil {
				status.Status = "degraded"
				status.Services["postgres"] = "down: " + err.Error()
			} else {
				status.Services["postgres"] = "up"
			}
		}

		if c.redis != nil {
			if err := c.redis.Ping(ctx).Err(); err != nil {
				status.Status = "degraded"
				status.Services["redis"] = "down: " + err.Error()
			} else {
				status.Services["redis"] = "up"
			}
		}

		code := http.StatusOK
		if status.Status != "ok" {
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(status)
	}
}

// LivenessHandler — простой "я жив" чек без зависимостей от БД.
// Нужен для Kubernetes/Docker — отвечает 200 всегда пока процесс работает.
func LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("alive"))
	}
}
