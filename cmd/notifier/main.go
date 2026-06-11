package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
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
	searchNotifRepo := postgres.NewSearchNotificationRepo(pool)
	searchSubRepo := postgres.NewSearchSubscriptionRepo(pool)
	userRepo := postgres.NewUserRepo(pool)

	var priceCache *redisrepo.PriceCache
	if redisClient != nil {
		priceCache = redisrepo.NewPriceCache(redisClient)
	}

	// ── Telegram ─────────────────────────────────────────────────────────────
	// Исходящие в Telegram идут через HTTPS_PROXY (см. compose) — поэтому и
	// товарные, и поиск-уведомления пробиваются с RU-хостинга.
	tgNotifier := telegram.NewNotifier(botToken)

	// ── Kafka ────────────────────────────────────────────────────────────────
	// Товарный путь: price-events.
	consumer := kafka.NewConsumer(kafkaBrokers, "price-events", kafkaGroupID)
	defer consumer.Close()

	// Поисковый путь (M1b): search-events.
	searchConsumer := kafka.NewConsumer(kafkaBrokers, "search-events", "search-notifier")
	defer searchConsumer.Close()

	topN := getEnvInt("SEARCH_NOTIFY_TOP_N", 10)
	go func() {
		log.Info("search-events consumer started")
		if err := searchConsumer.Run(ctx, makeSearchHandler(log, searchNotifRepo, tgNotifier, topN)); err != nil {
			log.Error("search-events consumer stopped", "err", err)
		}
	}()

	// Reconciler grace-периода: гасит/восстанавливает/чистит поиск-подписки по
	// действующему плану. Живёт здесь, т.к. notifier — единственный синглтон с
	// БД И egress в Telegram (scheduler без HTTPS_PROXY юзеру написать не может).
	reconcileInterval := time.Duration(getEnvInt("PLAN_RECONCILE_INTERVAL_MINUTES", 15)) * time.Minute
	go runPlanReconciler(ctx, log, searchSubRepo, subRepo, userRepo, tgNotifier, reconcileInterval)

	// Дефолт-фолбэк интервала проверки для тарифов без своего Interval.
	defaultInterval := time.Duration(getEnvInt("SCRAPE_INTERVAL_MINUTES", 15)) * time.Minute
	handler := makeHandler(log, subRepo, notifRepo, priceHistoryRepo, priceCache, tgNotifier, defaultInterval)

	log.Info("notifier started, waiting for price events...")
	return consumer.Run(ctx, handler)
}

// runPlanReconciler периодически приводит подписки в соответствие с действующим
// планом пользователей (grace-период после истечения) + шлёт напоминания:
//
//  1. ПАУЗА    — сверхлимитные подписки истёкших юзеров → active=FALSE,
//     paused_at=NOW(); затронутым шлём ОДНО уведомление (поиск+товары вместе).
//  2. ВОЗВРАТ  — кто вернул план в пределах grace → реактивируем самые старые
//     паузные подписки до лимита нового плана.
//  3. ОЧИСТКА  — паузные старше grace удаляем насовсем (каскад чистит baseline).
//  4. НАПОМИНАНИЕ — за сутки до истечения тарифа шлём разовое напоминание.
//
// Лимиты (MaxSearch/MaxProduct) — в коде (domain.Plans), поэтому решения считаем
// в Go (чистые функции select*). Блокируется до отмены ctx.
func runPlanReconciler(
	ctx context.Context,
	log *slog.Logger,
	searchRepo *postgres.SearchSubscriptionRepo,
	subRepo *postgres.SubscriptionRepo,
	userRepo *postgres.UserRepo,
	tg *telegram.Notifier,
	interval time.Duration,
) {
	tick := func() {
		now := time.Now()
		cutoff := now.Add(-domain.PlanGracePeriod)

		// 1. Пауза сверхлимитных подписок истёкших юзеров (поиск + товары).
		//    Затронутых уведомляем ОДИН раз, объединяя оба типа.
		notify := make(map[int64]struct{})
		for _, tgID := range pauseExpiredSearch(ctx, log, searchRepo) {
			notify[tgID] = struct{}{}
		}
		for _, tgID := range pauseExpiredProducts(ctx, log, subRepo, now) {
			notify[tgID] = struct{}{}
		}
		if len(notify) > 0 {
			log.Info("reconcile: notifying paused users", "users", len(notify))
			for tgID := range notify {
				if err := tg.SendPlanPausedNotice(ctx, tgID); err != nil {
					log.Error("reconcile: notify paused", "telegram_id", tgID, "err", err)
				}
			}
		}

		// 2. Возврат вернувшихся (в пределах grace), до лимита нового плана.
		restoreSearch(ctx, log, searchRepo, now, cutoff)
		restoreProducts(ctx, log, subRepo, now, cutoff)

		// 3. Очистка просроченных grace.
		if n, err := searchRepo.DeleteExpiredGraceSearchSubs(ctx, cutoff); err != nil {
			log.Error("reconcile: delete search past grace", "err", err)
		} else if n > 0 {
			log.Info("reconcile: deleted search subs past grace", "count", n)
		}
		if n, err := subRepo.DeleteExpiredGraceProductSubs(ctx, cutoff); err != nil {
			log.Error("reconcile: delete product past grace", "err", err)
		} else if n > 0 {
			log.Info("reconcile: deleted product subs past grace", "count", n)
		}

		// 4. Напоминание за сутки до истечения тарифа.
		remindExpiring(ctx, log, userRepo, tg, now.Add(24*time.Hour))
	}

	log.Info("plan reconciler started", "interval", interval.String())
	tick()
	ticker := time.NewTicker(interval)
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

// pauseExpiredSearch ставит на паузу ВСЕ активные поиск-подписки истёкших юзеров
// (free → MaxSearch=0). Возвращает telegram_id затронутых.
func pauseExpiredSearch(ctx context.Context, log *slog.Logger, repo *postgres.SearchSubscriptionRepo) []int64 {
	paused, err := repo.PauseExpiredSearchSubs(ctx)
	if err != nil {
		log.Error("reconcile: pause expired search", "err", err)
		return nil
	}
	if len(paused) > 0 {
		log.Info("reconcile: paused search subs", "users", len(paused))
	}
	return paused
}

// pauseExpiredProducts гасит ИЗБЫТОК товарных подписок истёкших юзеров сверх
// лимита плана (самые старые остаются). Возвращает telegram_id затронутых.
func pauseExpiredProducts(ctx context.Context, log *slog.Logger, repo *postgres.SubscriptionRepo, now time.Time) []int64 {
	cands, err := repo.ListActiveOfExpiredUsers(ctx)
	if err != nil {
		log.Error("reconcile: list active of expired", "err", err)
		return nil
	}
	toPause, affected := selectProductPauses(cands, now)
	if len(toPause) == 0 {
		return nil
	}
	if err := repo.PauseProductSubs(ctx, toPause); err != nil {
		log.Error("reconcile: pause products", "err", err)
		return nil
	}
	log.Info("reconcile: paused product subs", "count", len(toPause), "users", len(affected))
	return affected
}

// restoreSearch возвращает паузные поиск-подписки юзеров, вернувших план с
// поиском (в пределах grace), до лимита нового плана (самые старые).
func restoreSearch(ctx context.Context, log *slog.Logger, repo *postgres.SearchSubscriptionRepo, now, cutoff time.Time) {
	cands, err := repo.ListPausedWithinGrace(ctx, cutoff)
	if err != nil {
		log.Error("reconcile: list paused search", "err", err)
		return
	}
	toRestore := selectSearchRestores(cands, now)
	if len(toRestore) == 0 {
		return
	}
	if err := repo.Reactivate(ctx, toRestore); err != nil {
		log.Error("reconcile: reactivate search", "err", err)
		return
	}
	log.Info("reconcile: restored search subs", "count", len(toRestore))
}

// restoreProducts возвращает паузные товарные подписки юзеров, вернувших план
// (в пределах grace), до лимита нового плана С УЧЁТОМ уже активных подписок.
func restoreProducts(ctx context.Context, log *slog.Logger, repo *postgres.SubscriptionRepo, now, cutoff time.Time) {
	cands, err := repo.ListPausedWithinGrace(ctx, cutoff)
	if err != nil {
		log.Error("reconcile: list paused products", "err", err)
		return
	}
	toRestore := selectProductRestores(cands, now)
	if len(toRestore) == 0 {
		return
	}
	if err := repo.Reactivate(ctx, toRestore); err != nil {
		log.Error("reconcile: reactivate products", "err", err)
		return
	}
	log.Info("reconcile: restored product subs", "count", len(toRestore))
}

// remindExpiring шлёт разовое напоминание юзерам, чей тариф истекает в окне
// (now, until], и помечает их MarkReminded — только тех, кому реально отправили
// (сбой отправки → повтор на следующем тике).
func remindExpiring(ctx context.Context, log *slog.Logger, repo *postgres.UserRepo, tg *telegram.Notifier, until time.Time) {
	ids, err := repo.ListExpiringUnreminded(ctx, until)
	if err != nil {
		log.Error("reconcile: list expiring", "err", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	var sent []int64
	for _, tgID := range ids {
		if err := tg.SendPlanExpiringReminder(ctx, tgID); err != nil {
			log.Error("reconcile: send expiry reminder", "telegram_id", tgID, "err", err)
			continue
		}
		sent = append(sent, tgID)
	}
	if len(sent) == 0 {
		return
	}
	if err := repo.MarkReminded(ctx, sent); err != nil {
		log.Error("reconcile: mark reminded", "err", err)
	}
	log.Info("reconcile: sent expiry reminders", "count", len(sent))
}

// ── Чистые функции принятия решений (без БД, покрыты тестами) ────────────────
//
// Все три принимают cands, упорядоченные по (user_id, created_at), и группируют
// подряд идущие строки одного юзера. now → действующий план через EffectivePlan.

// selectSearchRestores — id поиск-подписок к возврату: самые старые до MaxSearch.
func selectSearchRestores(cands []postgres.PausedSearchSub, now time.Time) []int64 {
	var out []int64
	for i := 0; i < len(cands); {
		j := i
		for j < len(cands) && cands[j].UserID == cands[i].UserID {
			j++
		}
		group := cands[i:j]
		u := &domain.User{Plan: group[0].Plan, PlanExpiresAt: group[0].PlanExpiresAt}
		limit := u.EffectivePlan(now).MaxSearch
		for k := 0; k < len(group) && k < limit; k++ {
			out = append(out, group[k].ID)
		}
		i = j
	}
	return out
}

// selectProductPauses — id товарных подписок к паузе (избыток сверх лимита плана,
// самые старые остаются) и telegram_id затронутых юзеров.
func selectProductPauses(cands []postgres.ProductSubForReconcile, now time.Time) (pause, affected []int64) {
	for i := 0; i < len(cands); {
		j := i
		for j < len(cands) && cands[j].UserID == cands[i].UserID {
			j++
		}
		group := cands[i:j]
		u := &domain.User{Plan: group[0].Plan, PlanExpiresAt: group[0].PlanExpiresAt}
		limit := u.EffectivePlan(now).MaxProduct
		if len(group) > limit {
			for k := limit; k < len(group); k++ {
				pause = append(pause, group[k].ID)
			}
			affected = append(affected, group[0].TelegramID)
		}
		i = j
	}
	return pause, affected
}

// selectProductRestores — id товарных подписок к возврату: самые старые до
// (MaxProduct − уже активные) на юзера.
func selectProductRestores(cands []postgres.ProductSubForReconcile, now time.Time) []int64 {
	var out []int64
	for i := 0; i < len(cands); {
		j := i
		for j < len(cands) && cands[j].UserID == cands[i].UserID {
			j++
		}
		group := cands[i:j]
		u := &domain.User{Plan: group[0].Plan, PlanExpiresAt: group[0].PlanExpiresAt}
		slots := u.EffectivePlan(now).MaxProduct - group[0].ActiveCount
		for k := 0; k < len(group) && k < slots; k++ {
			out = append(out, group[k].ID)
		}
		i = j
	}
	return out
}

// evalSlack — допуск к интервалу оценки подписки: если шаг скрейпа примерно
// совпадает с интервалом тарифа, мелкий джиттер не должен «съедать» оценку.
const evalSlack = 5 * time.Second

// shouldEvaluate — пора ли оценивать подписку (throttle строгой per-plan модели):
// ещё ни разу (lastEval==nil) или с прошлой оценки прошло не меньше интервала
// тарифа. Это обузживает частоту доставки для обычных подписчиков товара, даже
// если он делит выдачу/скрейп с более быстрым тарифом.
func shouldEvaluate(lastEval *time.Time, interval time.Duration, now time.Time) bool {
	if lastEval == nil {
		return true
	}
	return now.Sub(*lastEval) >= interval-evalSlack
}

func makeHandler(
	log *slog.Logger,
	subRepo *postgres.SubscriptionRepo,
	notifRepo *postgres.NotificationRepo,
	priceHistoryRepo *postgres.PriceHistoryRepo,
	priceCache *redisrepo.PriceCache,
	tgNotifier *telegram.Notifier,
	defaultInterval time.Duration,
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

		now := time.Now()
		for _, sub := range subs {
			// Throttle: оцениваем подписку не чаще интервала её тарифа. PriceEvent
			// шлётся на каждом скрейпе (= MIN-интервал по подписчикам товара), но
			// доставку каждому держим строго по его плану.
			iv := domain.EffectivePlanFor(sub.OwnerPlan, sub.OwnerPlanExpiresAt, now).EffectiveInterval(defaultInterval)
			if !shouldEvaluate(sub.LastEvaluatedAt, iv, now) {
				continue
			}

			// markEval двигает чек-поинт. Вызываем ТОЛЬКО на безопасных точках (нет
			// срабатывания / уже отправлено / успешно записано), но НЕ перед
			// отправкой: иначе сбой отправки + переполучение сообщения «съел» бы
			// уведомление до следующего чек-поинта вместо немедленного ретрая.
			markEval := func() {
				if err := subRepo.MarkEvaluated(ctx, sub.ID); err != nil {
					log.Warn("mark evaluated", "sub_id", sub.ID, "err", err)
				}
			}

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
				markEval() // чек-поинт пройден, триггер не сработал
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
				markEval()
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
			markEval()
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

// makeSearchHandler — обработчик событий из search-events (поиск-уведомления).
// Сортирует подешевевшие товары по размеру скидки, шлёт топ-N одним сообщением,
// и ТОЛЬКО после успешной отправки фиксирует search_notifications (last-notified).
// Если отправка не удалась — offset не коммитится, событие перечитается (retry);
// при этом запись в БД не делается, так что baseline повторных срабатываний не
// исказится.
func makeSearchHandler(
	log *slog.Logger,
	searchNotifRepo *postgres.SearchNotificationRepo,
	tgNotifier *telegram.Notifier,
	topN int,
) kafka.HandlerFunc {
	return func(ctx context.Context, msg kafka.Message) error {
		ev, err := kafka.Decode[domain.SearchHitEvent](msg)
		if err != nil {
			log.Error("decode search hit event", "err", err)
			return nil // poison pill — пропускаем
		}
		if len(ev.Items) == 0 {
			return nil
		}

		// Больше реальное снижение (prev − effective) → выше в списке.
		items := make([]domain.SearchHitItem, len(ev.Items))
		copy(items, ev.Items)
		sort.SliceStable(items, func(i, j int) bool {
			return (items[i].PrevPriceKopecks - items[i].EffectiveKopecks) >
				(items[j].PrevPriceKopecks - items[j].EffectiveKopecks)
		})

		total := len(items)
		top := topN
		if top <= 0 {
			top = 10
		}
		shown := items
		if len(shown) > top {
			shown = shown[:top]
		}

		alert := telegram.SearchAlert{
			ChatID:    ev.TelegramID,
			QueryText: ev.QueryText,
			SearchURL: ev.SearchURL,
			TotalHits: total,
		}
		for _, it := range shown {
			alert.Items = append(alert.Items, telegram.SearchAlertItem{
				Name:         it.Name,
				URL:          it.URL,
				EffectiveRub: searchsub.Rubles(it.EffectiveKopecks),
				PrevRub:      searchsub.Rubles(it.PrevPriceKopecks),
				PointsRub:    searchsub.Rubles(it.FeedbackPointsKopecks),
			})
		}

		// Сначала отправляем; запись — только при успехе.
		if err := tgNotifier.SendSearchAlert(ctx, alert); err != nil {
			return fmt.Errorf("send search alert: %w", err)
		}
		// Фиксируем last-notified по ВСЕМ сработавшим товарам (не только показанным).
		for _, it := range ev.Items {
			if err := searchNotifRepo.Insert(ctx, ev.SubID, it.ProductID, searchsub.Rubles(it.EffectiveKopecks)); err != nil {
				log.Error("insert search notification", "sub_id", ev.SubID, "product_id", it.ProductID, "err", err)
			}
		}
		metrics.SearchNotificationsSent.WithLabelValues(ev.TriggerType).Inc()
		log.Info("search notification sent",
			"sub_id", ev.SubID, "telegram_id", ev.TelegramID, "items", len(alert.Items), "total_hits", total)
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

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
