package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
)

// TestBundlingEndToEnd — настоящий end-to-end бандлинга против ЖИВОГО Postgres:
// кладём несколько алертов в outbox через реальный репозиторий и гоняем реальный
// проход флашера (flushDue) с фейковым отправителем. Проверяем то, что фейк-стор
// в юнит-тестах не покрывает: SQL FetchDue (окно deliver_after) и группировку.
//
// Скипается без TEST_DATABASE_URL — `go test ./...` не ломается. Запускать на
// ОДНОРАЗОВОЙ базе (тест пишет/чистит pending_alerts). Пример:
//
//	docker run --rm -d --name pg-test -e POSTGRES_PASSWORD=test \
//	  -e POSTGRES_DB=test -p 5544:5432 postgres:16
//	TEST_DATABASE_URL="postgres://postgres:test@localhost:5544/test?sslmode=disable" \
//	  go test ./cmd/notifier/ -run TestBundlingEndToEnd -v
//	docker rm -f pg-test
func TestBundlingEndToEnd(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run (одноразовый Postgres)")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	ensurePendingAlertsSchema(t, ctx, pool)

	repo := postgres.NewPendingAlertRepo(pool)

	// Уникальный префикс ключей прогона → точечная чистка (не TRUNCATE — безопаснее).
	run := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM pending_alerts WHERE idem_key LIKE $1`, run+"%")
	})

	past := time.Now().Add(-time.Minute) // созрело
	future := time.Now().Add(time.Hour)  // окно ещё не вышло

	// user 100: три созревших падения цены → должны схлопнуться в ОДИН бандл.
	enqueue(t, ctx, repo, run+"-a1", 100, 100, "Кроссовки", 1990, 1490, past)
	enqueue(t, ctx, repo, run+"-a2", 100, 100, "Чайник", 3200, 2800, past)
	enqueue(t, ctx, repo, run+"-a3", 100, 100, "Наушники", 8990, 6990, past)
	// user 100: ещё не созревший алерт — НЕ должен уйти в этом проходе.
	enqueue(t, ctx, repo, run+"-afut", 100, 100, "Позже", 500, 400, future)
	// user 200: один созревший → одиночный богатый алерт (не бандл).
	enqueue(t, ctx, repo, run+"-b1", 200, 200, "Книга", 700, 500, past)

	sender := &fakeSender{}
	limiter := rate.NewLimiter(rate.Inf, 1) // без троттла в тесте
	if err := flushDue(ctx, discardLog(), repo, sender, limiter, 500, 10); err != nil {
		t.Fatalf("flushDue: %v", err)
	}

	// user 100 → один бандл ровно из 3 (future не вошёл — окно соблюдено).
	if len(sender.bundles) != 1 {
		t.Fatalf("ждём 1 бандл, получили %d: %+v", len(sender.bundles), sender.bundles)
	}
	if got := len(sender.bundles[0].Items); got != 3 {
		t.Fatalf("ждём бандл из 3 позиций (future исключён), получили %d", got)
	}
	if sender.bundles[0].ChatID != 100 {
		t.Fatalf("бандл не тому юзеру: ChatID=%d", sender.bundles[0].ChatID)
	}

	// user 200 → один богатый одиночный алерт.
	if len(sender.single) != 1 || sender.single[0].ChatID != 200 {
		t.Fatalf("ждём 1 одиночный алерт юзеру 200, получили %+v", sender.single)
	}

	// Доставленные помечены sent_at; недозревший — всё ещё в очереди.
	if n := countUnsentLike(t, ctx, pool, run+"%"); n != 1 {
		t.Fatalf("ждём 1 недоставленную строку (future), в очереди %d", n)
	}
	if n := countSentLike(t, ctx, pool, run+"%"); n != 4 {
		t.Fatalf("ждём 4 помеченных sent (3 бандл + 1 одиночный), помечено %d", n)
	}

	// Повторный проход не должен ничего отправить (всё созревшее уже ушло).
	sender2 := &fakeSender{}
	if err := flushDue(ctx, discardLog(), repo, sender2, limiter, 500, 10); err != nil {
		t.Fatalf("flushDue#2: %v", err)
	}
	if len(sender2.bundles) != 0 || len(sender2.single) != 0 {
		t.Fatalf("повторный проход не должен слать (идемпотентность): bundles=%d single=%d",
			len(sender2.bundles), len(sender2.single))
	}
}

func enqueue(t *testing.T, ctx context.Context, repo *postgres.PendingAlertRepo, key string, user, chat int64, name string, old, newp float64, deliverAfter time.Time) {
	t.Helper()
	payload, err := json.Marshal(telegram.PriceAlert{
		ChatID: chat, UserID: user, ProductName: name, OldPrice: old, NewPrice: newp,
	})
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := repo.Insert(ctx, &domain.PendingAlert{
		UserID: user, IdemKey: key, Payload: payload, DeliverAfter: deliverAfter,
	})
	if err != nil {
		t.Fatalf("insert %s: %v", key, err)
	}
	if !inserted {
		t.Fatalf("insert %s: ON CONFLICT — ключ не уникален?", key)
	}
}

// ensurePendingAlertsSchema создаёт таблицу из Up-блока миграции 021 (идемпотентно),
// чтобы тест работал на пустой базе без отдельного прогона goose.
func ensurePendingAlertsSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	raw, err := os.ReadFile("../../migrations/021_pending_alerts.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	s := string(raw)
	up := s[strings.Index(s, "+goose Up"):strings.Index(s, "+goose Down")]
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
}

func countUnsentLike(t *testing.T, ctx context.Context, pool *pgxpool.Pool, like string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pending_alerts WHERE idem_key LIKE $1 AND sent_at IS NULL`, like).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countSentLike(t *testing.T, ctx context.Context, pool *pgxpool.Pool, like string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pending_alerts WHERE idem_key LIKE $1 AND sent_at IS NOT NULL`, like).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
