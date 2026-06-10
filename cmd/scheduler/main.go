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
	// Дефолт-фолбэк интервала для тарифов без своего Interval (на практике все
	// планы его задают; фолбэк — страховка).
	defaultInterval := time.Duration(getEnvInt("SCRAPE_INTERVAL_MINUTES", 15)) * time.Minute
	// Шаг тикера: часто опрашиваем БД, но эмитим только «созревшие» товары/запросы
	// (по last_enqueued_at + их интервал). Должен быть заметно меньше самого
	// короткого тарифного интервала (reseller = 1 мин). Один шаг на оба пути.
	tick := time.Duration(getEnvInt("SCHEDULER_TICK_SECONDS", 20)) * time.Second

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
	// Быстрая дорожка перекупов — отдельный топик (отдельная consumer group и
	// пул токенов на стороне reseller-worker).
	resellerProducer := kafka.NewProducer(brokers, "reseller-tasks")
	defer resellerProducer.Close()

	log.Info("scheduler started",
		"default_interval", defaultInterval.String(),
		"tick", tick.String())

	go runProductScheduler(ctx, log, productRepo, productProducer, tick, defaultInterval)
	go runSearchScheduler(ctx, log, searchQueryRepo, searchProducer, resellerProducer, tick, defaultInterval)

	<-ctx.Done()
	return nil
}

// ── Товарный планировщик (due-based per-plan, зеркало поискового) ────────────
//
// Товар скрейпится не чаще MIN-интервала своих подписчиков: free-only товар —
// раз в 60 мин, а если его же отслеживает Pro — раз в 15 (Pro и платит за
// свежесть). Строгую доставку каждому тарифу обеспечивает throttle в notifier.
// Отдельной быстрой дорожки для товаров нет: reseller-товаров мало, они едут по
// общему scrape-tasks на своём 1-мин кадансе.

func runProductScheduler(
	ctx context.Context,
	log *slog.Logger,
	productRepo *postgres.ProductRepo,
	producer *kafka.Producer,
	tickInterval, defaultInterval time.Duration,
) {
	tick := func() {
		if err := productSchedulerTick(ctx, log, productRepo, producer, defaultInterval); err != nil {
			log.Error("product scheduler tick failed", "err", err)
		}
	}
	tick()
	timer := time.NewTicker(tickInterval)
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
	defaultInterval time.Duration,
) error {
	rows, err := productRepo.GetSchedulableProducts(ctx)
	if err != nil {
		return fmt.Errorf("get schedulable products: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	now := time.Now()

	type agg struct {
		url    string
		eff    time.Duration
		lastEn *time.Time
	}
	byProduct := make(map[int64]*agg)
	for _, r := range rows {
		iv := domain.EffectivePlanFor(r.OwnerPlan, r.PlanExpiresAt, now).EffectiveInterval(defaultInterval)
		a, ok := byProduct[r.ProductID]
		if !ok {
			byProduct[r.ProductID] = &agg{url: r.URL, eff: iv, lastEn: r.LastEnqueuedAt}
			continue
		}
		if iv < a.eff {
			a.eff = iv
		}
	}

	type due struct {
		id  int64
		url string
	}
	var dueList []due
	for id, a := range byProduct {
		if a.lastEn != nil && now.Sub(*a.lastEn) < a.eff {
			continue
		}
		dueList = append(dueList, due{id: id, url: a.url})
	}
	if len(dueList) == 0 {
		return nil
	}

	ids := make([]int64, len(dueList))
	for i, d := range dueList {
		ids[i] = d.id
	}
	if err := productRepo.ClaimEnqueued(ctx, ids); err != nil {
		return fmt.Errorf("claim products enqueued: %w", err)
	}

	sent := 0
	for _, d := range dueList {
		task := domain.ScrapeTask{ProductID: d.id, URL: d.url}
		key := strconv.FormatInt(d.id, 10)
		if err := producer.Send(ctx, key, task); err != nil {
			log.Error("send scrape task", "product_id", d.id, "err", err)
			continue
		}
		sent++
	}
	log.Info("product scheduler tick done", "due", len(dueList), "sent", sent)
	return nil
}

// ── Поисковый планировщик (due-based, две дорожки) ───────────────────────────
//
// Один частый тикер обслуживает обе дорожки. На каждом тике:
//   1. читает (запрос × подписчик) с тарифами;
//   2. для каждого запроса считает эффективный интервал = MIN по подписчикам
//      (источник истины — domain.Plans);
//   3. эмитит только «созревшие» (last_enqueued_at + интервал ≤ now);
//   4. бакетирует: интервал < дефолта → reseller-tasks (перекупы), иначе
//      search-tasks (обычные);
//   5. claim'ит last_enqueued_at перед эмиссией — синглтон, гонок нет.
//
// Дорожки не пересекаются: запрос с перекуп-подписчиком целиком уходит в
// быструю; обычные подписчики того же запроса оцениваются по своему интервалу
// уже в воркере (throttle), а не плодят уведомления каждую минуту.

func runSearchScheduler(
	ctx context.Context,
	log *slog.Logger,
	queryRepo *postgres.SearchQueryRepo,
	searchProducer, resellerProducer *kafka.Producer,
	tickInterval, defaultInterval time.Duration,
) {
	tick := func() {
		if err := searchSchedulerTick(ctx, log, queryRepo, searchProducer, resellerProducer, defaultInterval); err != nil {
			log.Error("search scheduler tick failed", "err", err)
		}
	}
	tick()
	timer := time.NewTicker(tickInterval)
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

// resellerLaneCutoff — запрос с эффективным интервалом ≤ этого порога уходит в
// быструю дорожку (reseller-tasks, отдельный пул токенов). Reseller-планы (1 мин)
// проходят, обычные (15/30/60) — нет.
const resellerLaneCutoff = 2 * time.Minute

// dueQuery — созревший запрос, готовый к эмиссии.
type dueQuery struct {
	id   int64
	url  string
	text string
	fast bool // эффективный интервал ≤ resellerLaneCutoff → дорожка перекупов
}

func searchSchedulerTick(
	ctx context.Context,
	log *slog.Logger,
	queryRepo *postgres.SearchQueryRepo,
	searchProducer, resellerProducer *kafka.Producer,
	defaultInterval time.Duration,
) error {
	rows, err := queryRepo.GetSchedulable(ctx)
	if err != nil {
		return fmt.Errorf("get schedulable: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	now := time.Now()

	// Группируем по запросу: эффективный интервал = MIN по подписчикам.
	type agg struct {
		url    string
		text   string
		eff    time.Duration
		lastEn *time.Time
	}
	byQuery := make(map[int64]*agg)
	for _, r := range rows {
		iv := domain.EffectivePlanFor(r.OwnerPlan, r.PlanExpiresAt, now).EffectiveInterval(defaultInterval)
		a, ok := byQuery[r.QueryID]
		if !ok {
			byQuery[r.QueryID] = &agg{url: r.NormalizedURL, text: r.QueryText, eff: iv, lastEn: r.LastEnqueuedAt}
			continue
		}
		if iv < a.eff {
			a.eff = iv
		}
	}

	// Отбираем созревшие.
	var due []dueQuery
	for id, a := range byQuery {
		if a.lastEn != nil && now.Sub(*a.lastEn) < a.eff {
			continue // ещё не пора
		}
		due = append(due, dueQuery{id: id, url: a.url, text: a.text, fast: a.eff <= resellerLaneCutoff})
	}
	if len(due) == 0 {
		return nil
	}

	// Claim до эмиссии: помечаем все созревшие как поставленные в очередь.
	ids := make([]int64, len(due))
	for i, d := range due {
		ids[i] = d.id
	}
	if err := queryRepo.ClaimEnqueued(ctx, ids); err != nil {
		return fmt.Errorf("claim enqueued: %w", err)
	}

	var sentFast, sentNormal int
	for _, d := range due {
		task := domain.SearchTask{QueryID: d.id, URL: d.url, QueryText: d.text}
		key := strconv.FormatInt(d.id, 10)
		producer := searchProducer
		if d.fast {
			producer = resellerProducer
		}
		if err := producer.Send(ctx, key, task); err != nil {
			log.Error("send search task", "query_id", d.id, "fast", d.fast, "err", err)
			continue
		}
		if d.fast {
			sentFast++
		} else {
			sentNormal++
		}
	}
	log.Info("search scheduler tick done", "due", len(due), "reseller", sentFast, "normal", sentNormal)
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
