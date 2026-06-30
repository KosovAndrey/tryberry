package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"gitlab.com/KosovAndrey/tryberrybot/internal/config"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/tracing"

	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/max"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/searchsub"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
	"gitlab.com/KosovAndrey/tryberrybot/internal/vk"
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
	databaseURL := config.MustEnv("DATABASE_URL")
	redisURL := config.MustEnv("REDIS_URL")
	kafkaBrokers := strings.Split(config.MustEnv("KAFKA_BROKERS"), ",")
	kafkaGroupID := config.MustEnv("KAFKA_GROUP_ID")
	botToken := config.MustEnv("TELEGRAM_BOT_TOKEN")
	otlpEndpoint := config.MustEnv("OTLP_ENDPOINT")

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

	go health.RunServer(ctx, log, pool, redisClient, "8091")

	// ── Репозитории ──────────────────────────────────────────────────────────
	subRepo := postgres.NewSubscriptionRepo(pool)
	notifRepo := postgres.NewNotificationRepo(pool)
	pendingRepo := postgres.NewPendingAlertRepo(pool)
	priceHistoryRepo := postgres.NewPriceHistoryRepo(pool)
	searchNotifRepo := postgres.NewSearchNotificationRepo(pool)
	searchSubRepo := postgres.NewSearchSubscriptionRepo(pool)
	userRepo := postgres.NewUserRepo(pool)
	referralRepo := postgres.NewReferralRepo(pool)

	var priceCache *redisrepo.PriceCache
	if redisClient != nil {
		priceCache = redisrepo.NewPriceCache(redisClient)
	}

	// ── Telegram ─────────────────────────────────────────────────────────────
	// Исходящие в Telegram идут через HTTPS_PROXY (см. compose) — поэтому и
	// товарные, и поиск-уведомления пробиваются с RU-хостинга.
	publicBaseURL := os.Getenv("PUBLIC_BASE_URL")
	if publicBaseURL == "" {
		publicBaseURL = "https://tryberry.ru"
	}
	tgNotifier := telegram.NewNotifier(botToken, publicBaseURL)

	// Маршрутизация по каналам: без VK_GROUP_TOKEN ведёт себя ровно как раньше
	// (всё в Telegram). С токеном — смотрит на users.notify_channel.
	sender := &deliverer{log: log, tg: tgNotifier, users: userRepo, chartBaseURL: publicBaseURL}
	if vkToken := os.Getenv("VK_GROUP_TOKEN"); vkToken != "" {
		sender.vk = vk.NewClient(vkToken)
		log.Info("vk delivery enabled")
	}
	if maxToken := os.Getenv("MAX_BOT_TOKEN"); maxToken != "" {
		if mc, err := max.NewClient(maxToken); err != nil {
			log.Error("max delivery init failed", "err", err)
		} else {
			sender.mx = mc
			log.Info("max delivery enabled")
		}
	}

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
		if err := searchConsumer.Run(ctx, makeSearchHandler(log, searchNotifRepo, sender, topN)); err != nil {
			log.Error("search-events consumer stopped", "err", err)
		}
	}()

	// Reconciler grace-периода: гасит/восстанавливает/чистит поиск-подписки по
	// действующему плану. Живёт здесь, т.к. notifier — единственный синглтон с
	// БД И egress в Telegram (scheduler без HTTPS_PROXY юзеру написать не может).
	reconcileInterval := time.Duration(getEnvInt("PLAN_RECONCILE_INTERVAL_MINUTES", 15)) * time.Minute
	go runPlanReconciler(ctx, log, searchSubRepo, subRepo, userRepo, referralRepo, sender, reconcileInterval)

	// Персональный дайджест «твои товары сейчас» (еженедельно). Гейт DIGEST_ENABLED —
	// фича шлёт сообщения ВСЕМ юзерам, поэтому включается осознанно (по умолчанию выкл).
	if os.Getenv("DIGEST_ENABLED") == "true" {
		// Тест: снизить возрастной гейт (свежие товары попадут в дайджест). В проде не задавать.
		if v := getEnvInt("DIGEST_MIN_AGE_DAYS", -1); v >= 0 {
			domain.SetHonestMinAge(time.Duration(v) * 24 * time.Hour)
			log.Info("digest: honest min age overridden", "days", v)
		}
		onlyTG := parseTGIDs(os.Getenv("DIGEST_TEST_TG_IDS"))
		if len(onlyTG) > 0 {
			log.Info("digest: canary mode — only listed telegram ids", "count", len(onlyTG))
		}
		go runDigest(ctx, log, userRepo, subRepo, priceHistoryRepo, sender, digestConfig{
			intervalDays: getEnvInt("DIGEST_INTERVAL_DAYS", 7),
			batch:        getEnvInt("DIGEST_BATCH", 100),
			startHour:    getEnvInt("DIGEST_HOUR_START", 6),    // UTC; 06–18 = 09–21 МСК
			endHour:      getEnvInt("DIGEST_HOUR_END", 18),     // для теста: 0..24 = без окна
			tickMinutes:  getEnvInt("DIGEST_TICK_MINUTES", 60), // для теста можно 1
			onlyTG:       onlyTG,                               // DIGEST_TEST_TG_IDS — рассылать только себе
		})
	} else {
		log.Info("digest disabled (set DIGEST_ENABLED=true to enable)")
	}

	// Дефолт-фолбэк интервала проверки для тарифов без своего Interval.
	defaultInterval := time.Duration(getEnvInt("SCRAPE_INTERVAL_MINUTES", 15)) * time.Minute
	// Singleton-доставщик: разгребает durable-outbox (pending_alerts) с глобальным
	// rate-limit и ретраями. Расцепляет консьюмер price-events от медленной
	// Telegram-отправки. См. docs/SCALING-NOTIFIER-DELIVERY.md.
	go runFlusher(ctx, log, pendingRepo, sender, defaultFlusherConfig())

	handler := makeHandler(log, subRepo, notifRepo, pendingRepo, priceHistoryRepo, priceCache, sender, defaultInterval)

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
	referralRepo *postgres.ReferralRepo,
	sender *deliverer,
	interval time.Duration,
) {
	tick := func() {
		now := time.Now()
		cutoff := now.Add(-domain.PlanGracePeriod)

		// 1. Пауза сверхлимитных подписок истёкших юзеров (поиск + товары).
		//    Затронутых уведомляем ОДИН раз, объединяя оба типа.
		notify := make(map[int64]struct{})
		for _, uid := range pauseExpiredSearch(ctx, log, searchRepo) {
			notify[uid] = struct{}{}
		}
		for _, uid := range pauseExpiredProducts(ctx, log, subRepo, now) {
			notify[uid] = struct{}{}
		}
		if len(notify) > 0 {
			log.Info("reconcile: notifying paused users", "users", len(notify))
			for uid := range notify {
				if err := sender.SendPlanPausedNotice(ctx, uid); err != nil {
					log.Error("reconcile: notify paused", "user_id", uid, "err", err)
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
		remindExpiring(ctx, log, userRepo, sender, now.Add(24*time.Hour))

		// 5. Реферальные награды за «активных» друзей (48ч + активная подписка).
		rewardReferralActivations(ctx, log, referralRepo, sender, now)

		// 6. Сигнал «пора бандлинг»: макс. активных подписок у одного юзера. Дёшево
		//    (1 запрос раз в reconcile-интервал), не на горячем пути.
		if n, err := subRepo.MaxActivePerUser(ctx); err != nil {
			log.Warn("reconcile: max active subs per user", "err", err)
		} else {
			metrics.MaxActiveSubsPerUser.Set(float64(n))
		}
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
// (free → MaxSearch=0). Возвращает users.id затронутых.
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
// лимита плана (самые старые остаются). Возвращает users.id затронутых.
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
func remindExpiring(ctx context.Context, log *slog.Logger, repo *postgres.UserRepo, sender *deliverer, until time.Time) {
	ids, err := repo.ListExpiringUnreminded(ctx, until)
	if err != nil {
		log.Error("reconcile: list expiring", "err", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	var sent []int64
	for _, uid := range ids {
		if err := sender.SendPlanExpiringReminder(ctx, uid); err != nil {
			log.Error("reconcile: send expiry reminder", "user_id", uid, "err", err)
			continue
		}
		sent = append(sent, uid)
	}
	if len(sent) == 0 {
		return
	}
	if err := repo.MarkReminded(ctx, sent); err != nil {
		log.Error("reconcile: mark reminded", "err", err)
	}
	log.Info("reconcile: sent expiry reminders", "count", len(sent))
}

// rewardReferralActivations начисляет рефереру дни за «активных» друзей:
// приглашённый прожил ReferralActivationAge и держит активную подписку.
// Идемпотентно: UNIQUE (referee, event) в referral_rewards + потолок за год.
func rewardReferralActivations(ctx context.Context, log *slog.Logger, repo *postgres.ReferralRepo, sender *deliverer, now time.Time) {
	pending, err := repo.ListPendingActivations(ctx, domain.ReferralActivationAge, 100)
	if err != nil {
		log.Error("reconcile: list pending referral activations", "err", err)
		return
	}
	for _, p := range pending {
		referrer := domain.User{Plan: p.ReferrerPlan, PlanExpiresAt: p.ReferrerExpAt}
		plan, expiresAt, setPlan := domain.ApplyReferralReward(&referrer, domain.ReferralActivatedRewardDays, now)

		granted, err := repo.GrantReward(ctx,
			p.ReferrerID, p.RefereeID,
			domain.ReferralEventActivated, domain.ReferralActivatedRewardDays, domain.ReferralYearlyCapDays,
			setPlan, plan, expiresAt)
		if err != nil {
			log.Error("reconcile: grant referral reward", "referrer", p.ReferrerID, "referee", p.RefereeID, "err", err)
			continue
		}
		if !granted {
			continue // потолок за год или уже начислено — без уведомления
		}
		log.Info("reconcile: referral reward granted",
			"referrer", p.ReferrerID, "referee", p.RefereeID, "days", domain.ReferralActivatedRewardDays, "set_plan", setPlan)
		if err := sender.SendReferralRewardNotice(ctx, p.ReferrerID, p.RefereeName, domain.ReferralActivatedRewardDays, setPlan); err != nil {
			log.Error("reconcile: notify referral reward", "referrer_id", p.ReferrerID, "err", err)
		}
	}
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
// самые старые остаются) и users.id затронутых юзеров.
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
			affected = append(affected, group[0].UserID)
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

// alertSender — доставка алертов (deliverer; в тестах можно мокать).
type alertSender interface {
	SendPriceAlert(ctx context.Context, a telegram.PriceAlert) error
	SendSearchAlert(ctx context.Context, a telegram.SearchAlert) error
	SendDigest(ctx context.Context, userID, telegramID int64, text string) error
}

func makeHandler(
	log *slog.Logger,
	subRepo *postgres.SubscriptionRepo,
	notifRepo *postgres.NotificationRepo,
	pendingRepo *postgres.PendingAlertRepo,
	priceHistoryRepo *postgres.PriceHistoryRepo,
	priceCache *redisrepo.PriceCache,
	sender alertSender,
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

		// Наличие: дополнительно страхуемся ценой (>0 ⇒ в наличии) на случай старых
		// событий без полей InStock/WasInStock во время rolling-деплоя scraper.
		inStock := event.InStock || event.NewPrice > 0
		wasInStock := event.WasInStock || event.OldPrice > 0

		now := time.Now()

		// «Честная цена»: вердикт по истории считаем ЛЕНИВО — только когда реально
		// собираемся отправить алерт (а не на каждое событие: уведомления редки,
		// событий — на каждом скрейпе). Мемоизация на событие: первый отправляемый
		// алерт по товару считает Stats, остальные подписчики переиспользуют. Цикл
		// последователен — без локов. Ошибки/мало данных → пустая строка.
		honestLine := ""
		honestDone := false
		honest := func() string {
			if honestDone {
				return honestLine
			}
			honestDone = true
			if inStock && currentPrice > 0 {
				if stats, err := priceHistoryRepo.Stats(ctx, event.ProductID, now); err != nil {
					log.Warn("honest price stats", "err", err)
				} else {
					honestLine = domain.AssessHonestPrice(currentPrice, stats, now).Line()
				}
			}
			return honestLine
		}

		for _, sub := range subs {
			// Throttle: оцениваем подписку не чаще интервала её тарифа. PriceEvent
			// шлётся на каждом скрейпе (= MIN-интервал по подписчикам товара), но
			// доставку каждому держим строго по его плану.
			plan := domain.EffectivePlanFor(sub.OwnerPlan, sub.OwnerPlanExpiresAt, now)
			iv := plan.EffectiveInterval(defaultInterval)
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

			// back_in_stock: срабатываем при переходе «нет в наличии»→«появилось».
			// Ценовой движок тут неприменим (подписка заведена без цены).
			backInStock := sub.TriggerType == domain.TriggerBackInStock
			if backInStock {
				if wasInStock || !inStock {
					markEval() // ещё не появился (или уже был в наличии) — чек-поинт пройден
					continue
				}
			} else {
				// Ценовые триггеры оцениваем ТОЛЬКО когда товар в наличии: при OOS
				// currentPrice=0 дал бы ложное срабатывание below_target (0 <= target).
				if !inStock {
					markEval()
					continue
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

			// Вердикт «честной цены» — только для ценового алерта (для back_in_stock
			// не показываем и Stats не дёргаем). Ленивый расчёт здесь = «когда реально шлём».
			hl := ""
			if !backInStock {
				hl = honest()
			}

			// Кладём в durable-outbox (pending_alerts) вместо синхронной отправки:
			// горячий путь консьюмера не блокируется медленным Telegram-egress,
			// доставку гарантирует флашер ретраями (at-least-once). Стейт подписки
			// (baseline/notified) двигаем вперёд здесь, на решении — доставка
			// случится позже, но решение уже принято и зафиксировано.
			// См. docs/SCALING-NOTIFIER-DELIVERY.md.
			payload, err := json.Marshal(telegram.PriceAlert{
				ChatID:         sub.TelegramID,
				UserID:         sub.UserID,
				SubscriptionID: sub.ID,
				ProductName:    sub.ProductName,
				ProductURL:     domain.CleanProductURL(sub.ProductURL),
				PublicID:       sub.ProductPublicID,
				OldPrice:       sub.BaselinePrice,
				NewPrice:       event.NewPrice,
				ImageURL:       sub.ProductImageURL,
				BackInStock:    backInStock,
				HonestLine:     hl,
			})
			if err != nil {
				return fmt.Errorf("marshal alert payload: %w", err)
			}
			// Окно бандлинга по тарифу: до deliver_after флашер копит алерты юзера
			// и отправит их одним сообщением (≥2) либо богатым алертом (1).
			if _, err := pendingRepo.Insert(ctx, &domain.PendingAlert{
				UserID:         sub.UserID,
				SubscriptionID: sub.ID,
				ProductID:      sub.ProductID,
				IdemKey:        iKey,
				Payload:        payload,
				DeliverAfter:   now.Add(plan.BundleWindow()),
			}); err != nil {
				return fmt.Errorf("enqueue pending alert: %w", err)
			}

			// Фиксируем цену последнего уведомления (+ notified=TRUE)
			if err := subRepo.UpdateBaseline(ctx, sub.ID, event.NewPrice); err != nil {
				return fmt.Errorf("update baseline: %w", err)
			}

			// Товар вернулся в наличии: дальше следим за ЦЕНОЙ — переключаем триггер на
			// any_drop (baseline уже = цена возврата). Иначе подписка осталась бы «жду
			// наличия» и ничего не делала, пока товар в продаже, а /list врал бы.
			// SetTrigger не трогает baseline/notified — they уже выставлены выше.
			if backInStock {
				if err := subRepo.SetTrigger(ctx, sub.ID, sub.UserID, string(domain.TriggerAnyDrop), nil, nil); err != nil {
					log.Warn("back_in_stock: switch to any_drop", "sub_id", sub.ID, "err", err)
				} else {
					log.Info("back_in_stock: switched to any_drop", "sub_id", sub.ID)
				}
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
			log.Info("notification enqueued",
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
	sender alertSender,
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
			UserID:    ev.UserID,
			QueryText: ev.QueryText,
			SearchURL: ev.SearchURL,
			TotalHits: total,
		}
		for _, it := range shown {
			alert.Items = append(alert.Items, telegram.SearchAlertItem{
				Name:         it.Name,
				URL:          domain.CleanProductURL(it.URL),
				EffectiveRub: searchsub.Rubles(it.EffectiveKopecks),
				PrevRub:      searchsub.Rubles(it.PrevPriceKopecks),
				PointsRub:    searchsub.Rubles(it.FeedbackPointsKopecks),
			})
		}

		// Сначала отправляем; запись — только при успехе.
		if err := sender.SendSearchAlert(ctx, alert); err != nil {
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

// digestConfig — параметры дайджеста (все из env, с дефолтами).
type digestConfig struct {
	intervalDays int            // каданс на юзера
	batch        int            // юзеров на тик
	startHour    int            // начало дневного окна, UTC (вкл.)
	endHour      int            // конец дневного окна, UTC (искл.); startHour..endHour
	tickMinutes  int            // период проверки
	onlyTG       map[int64]bool // непусто → шлём ТОЛЬКО этим telegram_id (канареечный тест)
}

// runDigest — персональный дайджест «хорошие цены сейчас». Тик каждые tickMinutes;
// шлём только в дневном окне [startHour, endHour) UTC, пачками (batch), и только тем,
// у кого есть что показать. Каданс держим last_digest_at (двигаем для каждого
// обработанного, даже если слать было нечего — иначе сканировали бы каждый тик).
func runDigest(ctx context.Context, log *slog.Logger, userRepo *postgres.UserRepo,
	subRepo *postgres.SubscriptionRepo, priceRepo *postgres.PriceHistoryRepo,
	sender alertSender, cfg digestConfig) {
	log.Info("digest started", "interval_days", cfg.intervalDays, "batch", cfg.batch,
		"window_utc", fmt.Sprintf("%d-%d", cfg.startHour, cfg.endHour), "tick_min", cfg.tickMinutes)
	ticker := time.NewTicker(time.Duration(cfg.tickMinutes) * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h := time.Now().UTC().Hour(); h < cfg.startHour || h >= cfg.endHour {
				continue // вне дневного окна — не беспокоим
			}
			digestSweep(ctx, log, userRepo, subRepo, priceRepo, sender, cfg)
		}
	}
}

func digestSweep(ctx context.Context, log *slog.Logger, userRepo *postgres.UserRepo,
	subRepo *postgres.SubscriptionRepo, priceRepo *postgres.PriceHistoryRepo,
	sender alertSender, cfg digestConfig) {
	before := time.Now().Add(-time.Duration(cfg.intervalDays) * 24 * time.Hour)
	recipients, err := userRepo.UsersDueForDigest(ctx, before, cfg.batch)
	if err != nil {
		log.Error("digest: list due", "err", err)
		return
	}
	if len(recipients) == 0 {
		return
	}
	sent := 0
	for _, r := range recipients {
		if ctx.Err() != nil {
			return
		}
		// Канареечный тест: шлём только указанным TG, остальных НЕ трогаем (их
		// last_digest_at не двигаем → останутся due для боевой рассылки).
		if len(cfg.onlyTG) > 0 && !cfg.onlyTG[r.TelegramID] {
			continue
		}
		if text, ok := buildDigest(ctx, log, subRepo, priceRepo, r.UserID); ok {
			if err := sender.SendDigest(ctx, r.UserID, r.TelegramID, text); err != nil {
				log.Warn("digest: send", "user_id", r.UserID, "err", err)
			} else {
				sent++
			}
		}
		// Чек-поинт двигаем независимо от факта отправки — каданс недельный, без
		// ретрай-шторма (неотправленный дайджест не критичен, дождётся следующего).
		if err := userRepo.MarkDigestSent(ctx, r.UserID, time.Now()); err != nil {
			log.Warn("digest: mark", "user_id", r.UserID, "err", err)
		}
	}
	log.Info("digest sweep done", "due", len(recipients), "sent", sent)
}

// buildDigest собирает текст дайджеста: товары пользователя, которые СЕЙЧАС по
// честно хорошей цене (вердикт 🟢 минимум за 30/90д/всё время). Пусто (ok=false),
// если показывать нечего — тогда дайджест не шлём (без спама «ничего нет»).
func buildDigest(ctx context.Context, log *slog.Logger, subRepo *postgres.SubscriptionRepo,
	priceRepo *postgres.PriceHistoryRepo, userID int64) (string, bool) {
	subs, err := subRepo.GetActiveByUserID(ctx, userID)
	if err != nil {
		log.Warn("digest: load subs", "user_id", userID, "err", err)
		return "", false
	}
	if len(subs) == 0 {
		return "", false
	}
	now := time.Now()
	type deal struct {
		name, url, verdict string
		price              float64
	}
	var deals []deal
	for _, s := range subs {
		if s.CurrentPrice <= 0 {
			continue
		}
		stats, err := priceRepo.Stats(ctx, s.ProductID, now)
		if err != nil {
			continue
		}
		hp := domain.AssessHonestPrice(s.CurrentPrice, stats, now)
		switch hp.Verdict {
		case domain.VerdictLowestEver, domain.VerdictLowest90, domain.VerdictLowest30:
			deals = append(deals, deal{name: s.ProductName, url: s.ProductURL, verdict: hp.Line(), price: s.CurrentPrice})
		}
	}
	if len(deals) == 0 {
		return "", false
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🟢 Хорошие цены сейчас — %d из %d твоих товаров:\n\n", len(deals), len(subs))
	const showN = 10
	for i, d := range deals {
		if i >= showN {
			fmt.Fprintf(&sb, "…и ещё %d\n", len(deals)-showN)
			break
		}
		fmt.Fprintf(&sb, "%s — %.0f ₽\n%s\n%s\n\n", clipRunes(d.name, 60), d.price, d.verdict, domain.CleanProductURL(d.url))
	}
	sb.WriteString("Тип уведомлений — в /list · больше слотов — /plans")
	return sb.String(), true
}

func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// parseTGIDs парсит "111,222" в множество telegram_id (для DIGEST_TEST_TG_IDS).
// Пусто → nil (фильтра нет, шлём всем).
func parseTGIDs(s string) map[int64]bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := make(map[int64]bool)
	for _, p := range strings.Split(s, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
			out[id] = true
		}
	}
	return out
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
