// cmd/scheduler/main.go
//
// scheduler — СИНГЛТОН-планировщик задач скрейпинга.
//
// Должен работать ровно в ОДНОМ процессе (replicas=1) — иначе пойдут дубли
// задач в Kafka. Благодаря этому api/scraper/notifier реплицируются свободно.
//
// Два независимых тикера:
//   - товарный  → топик scrape-tasks  (интервал SCRAPE_INTERVAL_MINUTES)
//   - поисковый → топик search-tasks  (интервал SEARCH_SCRAPE_INTERVAL_MINUTES)
//
// Поисковый тикер (M1b) заменил in-process runSearchLoop, который раньше жил в
// scraper и мешал его репликации.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"gitlab.com/KosovAndrey/tryberrybot/internal/config"
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

	databaseURL := config.MustEnv("DATABASE_URL")
	brokers := strings.Split(config.MustEnv("KAFKA_BROKERS"), ",")
	otlpEndpoint := getEnv("OTLP_ENDPOINT", "jaeger:4317")
	healthPort := getEnv("SCHEDULER_HEALTH_PORT", "8092")
	// Дефолт-фолбэк интервала для тарифов без своего Interval (на практике все
	// планы его задают; фолбэк — страховка).
	defaultInterval := time.Duration(getEnvInt("SCRAPE_INTERVAL_MINUTES", 15)) * time.Minute
	// ОПЦИОНАЛЬНЫЙ аварийный троттл Ozon-ТОВАРОВ (пол + множитель к тарифному
	// интервалу). Исторически Ozon душили, т.к. mobile-API ходил за одним
	// аккаунтом/IP и не терпел частоты. С переходом на браузерный сайдкар
	// ozon-miner (антибот-безопасный транспорт) троттл больше не нужен и в проде
	// ВЫКЛЮЧЕН (.env: OZON_MIN_INTERVAL_MINUTES=0, OZON_INTERVAL_MULTIPLIER=1) →
	// Ozon-товары идут ПОЛНЫМ тарифным кадансом как WB/YM (reseller = 1 мин).
	// Оставлено рычагом на случай, если сайдкар начнёт захлёбываться. Пол=0 и
	// множитель=1 = троттл выкл. Канон: docs/SCRAPE-CADENCE.md.
	ozonMinInterval := time.Duration(getEnvInt("OZON_MIN_INTERVAL_MINUTES", 0)) * time.Minute
	ozonMult := getEnvInt("OZON_INTERVAL_MULTIPLIER", 1)
	// Пол интервала для Ozon-ПОИСКА (отдельно от товарного): выдача ротируется
	// и тянется через одну прогретую дорожку сайдкара — частить нельзя. 0 — выкл.
	ozonSearchMin := time.Duration(getEnvInt("OZON_SEARCH_MIN_INTERVAL_MINUTES", 30)) * time.Minute
	// Джиттер пола Ozon-поиска: доля от ozonSearchMin, на которую интервал гуляет
	// вверх/вниз (0.4 → ±40%). 0 — ровная сетка, как было.
	ozonSearchJitter := getEnvFloat("OZON_SEARCH_JITTER", 0)
	if ozonMult < 1 {
		ozonMult = 1
	}
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

	go health.RunServer(ctx, log, pool, nil, healthPort)

	productRepo := postgres.NewProductRepo(pool)
	searchQueryRepo := postgres.NewSearchQueryRepo(pool)

	productProducer := kafka.NewProducer(brokers, "scrape-tasks")
	defer productProducer.Close()
	// Ozon — самый медленный (браузерный сайдкар ~3-5с) и туже всех зажат
	// rate-лимитом — в отдельный топик, чтобы его backlog не занимал слоты пула
	// быстрых WB/YM в scraper. Своя consumer-group + свой параллелизм на стороне
	// scraper (docs/THROUGHPUT-ROADMAP.md №2). Топик авто-создаётся (10 партиций).
	ozonProducer := kafka.NewProducer(brokers, "ozon-scrape-tasks")
	defer ozonProducer.Close()
	searchProducer := kafka.NewProducer(brokers, "search-tasks")
	defer searchProducer.Close()
	// Быстрая дорожка перекупов — отдельный топик (отдельная consumer group и
	// пул токенов на стороне reseller-worker).
	resellerProducer := kafka.NewProducer(brokers, "reseller-tasks")
	defer resellerProducer.Close()

	log.Info("scheduler started",
		"default_interval", defaultInterval.String(),
		"ozon_min_interval", ozonMinInterval.String(),
		"ozon_mult", ozonMult,
		"tick", tick.String())

	go runProductScheduler(ctx, log, productRepo, productProducer, ozonProducer, tick, defaultInterval, ozonMinInterval, ozonMult)
	go runSearchScheduler(ctx, log, searchQueryRepo, searchProducer, resellerProducer, tick, defaultInterval, ozonSearchMin, ozonSearchJitter)

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
	producer, ozonProducer *kafka.Producer,
	tickInterval, defaultInterval, ozonMinInterval time.Duration,
	ozonMult int,
) {
	tick := func() {
		if err := productSchedulerTick(ctx, log, productRepo, producer, ozonProducer, defaultInterval, ozonMinInterval, ozonMult); err != nil {
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
	producer, ozonProducer *kafka.Producer,
	defaultInterval, ozonMinInterval time.Duration,
	ozonMult int,
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
		url        string
		eff        time.Duration
		lastEn     *time.Time
		lastChange *time.Time // последняя смена цены — волатильностный бэкофф
		subs       int        // активных подписчиков — кап популярности
		fastSub    bool       // есть подписчик минутного (reseller) тарифа — бэкофф не применяем
		has        bool       // есть хоть один подписчик, по которому товар скрейпится
	}
	byProduct := make(map[int64]*agg)
	for _, r := range rows {
		a, ok := byProduct[r.ProductID]
		if !ok {
			a = &agg{url: r.URL, lastEn: r.LastEnqueuedAt, lastChange: r.LastPriceChangeAt}
			byProduct[r.ProductID] = a
		}
		a.subs++
		lurl := strings.ToLower(r.URL)
		// Ozon — единственный антибот, требующий троттла: FAB + один аккаунт/IP за
		// общим мобильным прокси не терпят частого опроса. Я.Маркет СЮДА БОЛЬШЕ НЕ
		// ВХОДИТ: probe доказал, что он ходит direct с датацентр-IP без прокси и
		// держит поток без капчи (docs/YANDEX-WARMED-COOKIES.md) — троттлить его как
		// антибот незачем, скрейпим обычным WB-кадансом (вкл. reseller-планы).
		isOzon := strings.Contains(lurl, "ozon.ru")
		plan := domain.EffectivePlanFor(r.OwnerPlan, r.PlanExpiresAt, now)
		iv := plan.EffectiveInterval(defaultInterval)
		// Reseller-подписчик (минутный тариф) выключает волатильностный бэкофф
		// для товара целиком — скорость есть ядро ценности reseller, замедлять
		// его нельзя нигде (в т.ч. на Ozon). Определяем по ТАРИФНОМУ интервалу.
		if iv <= resellerLaneCutoff {
			a.fastSub = true
		}
		if isOzon {
			// Опциональный троттл Ozon (× ozonMult, пол ozonMinInterval). В проде
			// ВЫКЛЮЧЕН (mult=1, пол=0) — Ozon идёт полным тарифным кадансом как WB
			// (reseller = 1 мин); сайдкар ozon-miner держит нагрузку. Оба параметра
			// >дефолта включают троттл обратно. docs/SCRAPE-CADENCE.md.
			iv *= time.Duration(ozonMult)
			if ozonMinInterval > 0 && iv < ozonMinInterval {
				iv = ozonMinInterval
			}
		}
		if !a.has || iv < a.eff {
			a.eff = iv
			a.has = true
		}
	}

	type due struct {
		id  int64
		url string
	}
	var dueList []due
	for id, a := range byProduct {
		// Страховка: товар без единого вкладывающегося подписчика не скрейпим
		// (при выключенном Ozon-троттле не срабатывает, но оставлено на случай
		// его включения — тогда Ozon-товар только с reseller-подписчиками мог бы
		// не набрать вклад).
		if !a.has {
			continue
		}
		// Волатильностный бэкофф: давно не менявшаяся цена → реже опрос
		// (×1..×3, сброс первым изменением). Товары с reseller-подписчиком не
		// трогаем совсем (fastSub) — частота и есть продукт reseller-тарифов.
		// Потолка нет: худший товарный случай 60м×3 = 3ч.
		eff := a.eff
		if !a.fastSub && eff > resellerLaneCutoff {
			eff = domain.ApplyVolatility(eff, a.lastChange, a.subs, now, 0)
		}
		if a.lastEn != nil && now.Sub(*a.lastEn) < eff {
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

	sent, ozonSent := 0, 0
	for _, d := range dueList {
		task := domain.ScrapeTask{ProductID: d.id, URL: d.url}
		key := strconv.FormatInt(d.id, 10)
		// Ozon → отдельный топик (детект тот же, что при расчёте каданса выше).
		// Прочие МП (WB/YM/Ali) — общий scrape-tasks.
		p := producer
		isOzon := strings.Contains(strings.ToLower(d.url), "ozon.ru")
		if isOzon {
			p = ozonProducer
		}
		if err := p.Send(ctx, key, task); err != nil {
			log.Error("send scrape task", "product_id", d.id, "ozon", isOzon, "err", err)
			continue
		}
		if isOzon {
			ozonSent++
		} else {
			sent++
		}
	}
	log.Info("product scheduler tick done", "due", len(dueList), "sent", sent, "ozon_sent", ozonSent)
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
	tickInterval, defaultInterval, ozonSearchMin time.Duration,
	ozonSearchJitter float64,
) {
	tick := func() {
		if err := searchSchedulerTick(ctx, log, queryRepo, searchProducer, resellerProducer, defaultInterval, ozonSearchMin, ozonSearchJitter); err != nil {
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

// jitterFactor — множитель в [1-j, 1+j], ДЕТЕРМИНИРОВАННЫЙ для пары
// (запрос, начало цикла). Стабильность внутри цикла обязательна: планировщик
// проверяет «пора?» каждый тик, и если разыгрывать порог заново на каждом
// тике, сработает первый же тик, которому выпало маленькое значение — всё
// распределение съедет к нижней границе (вместо 3.6–8.4 мин получим ~3.6).
// Привязка к lastEnqueued даёт новый множитель на каждый следующий цикл.
func jitterFactor(id int64, lastEnqueued *time.Time, j float64) float64 {
	if j <= 0 {
		return 1
	}
	if j > 0.9 {
		j = 0.9 // ниже 10% от пола не опускаемся ни при какой конфигурации
	}
	seed := uint64(id) * 2654435761
	if lastEnqueued != nil {
		seed ^= uint64(lastEnqueued.UnixNano())
	}
	// xorshift64 — перемешать биты: без него соседние id/времена дают соседние
	// множители, и запросы синхронизируются вместо того, чтобы размазаться.
	seed ^= seed << 13
	seed ^= seed >> 7
	seed ^= seed << 17
	u := float64(seed%10000) / 10000.0 // [0,1)
	return 1 + j*(2*u-1)
}

// scaleDuration — d × factor с округлением до секунды (для читаемых логов).
func scaleDuration(d time.Duration, factor float64) time.Duration {
	return time.Duration(float64(d) * factor).Round(time.Second)
}

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
	defaultInterval, ozonSearchMin time.Duration,
	ozonSearchJitter float64,
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
	// Интервал ПОИСКА — отдельный от товарного (EffectiveSearchInterval):
	// поисковый скрейп дороже, free задаёт свой редкий каданс (6ч).
	type agg struct {
		mp         string
		url        string
		text       string
		eff        time.Duration
		lastEn     *time.Time
		lastChange *time.Time // последняя смена мин. цены топ-N — бэкофф
		subs       int        // активных подписчиков — кап популярности
		fastSub    bool       // есть подписчик минутного (reseller) тарифа — бэкофф не применяем
	}
	byQuery := make(map[int64]*agg)
	for _, r := range rows {
		iv := domain.EffectivePlanFor(r.OwnerPlan, r.PlanExpiresAt, now).EffectiveSearchInterval(defaultInterval)
		a, ok := byQuery[r.QueryID]
		if !ok {
			a = &agg{mp: r.Marketplace, url: r.NormalizedURL, text: r.QueryText, eff: iv, lastEn: r.LastEnqueuedAt, lastChange: r.LastChangeAt}
			byQuery[r.QueryID] = a
		} else if iv < a.eff {
			a.eff = iv
		}
		a.subs++
		// По тарифному интервалу (до Ozon-пола): Ozon-запрос перекупа флорится до
		// 30м и уходит с fast-дорожки, но волатильностный бэкофф на него всё
		// равно не распространяется — reseller не замедляем нигде.
		if iv <= resellerLaneCutoff {
			a.fastSub = true
		}
	}

	// Пол интервала для Ozon-поиска: живая выдача достаётся через одну прогретую
	// дорожку сайдкара и сильно ротируется — частый скрейп жжёт сессию и спамит
	// below_target новыми позициями. Поэтому Ozon не чаще ozonSearchMin даже на
	// быстрых тарифах (заодно уводит Ozon с reseller-дорожки на нормальную).
	// Джиттер размазывает пол: без него запрос, упёршийся в ozonSearchMin, ходит
	// по ровной сетке (видно по батчам уведомлений — 11:05/11:15/11:35). Тот же
	// приём, что у спейсинга дорожек внутри сайдкара (OZON_LANE_JITTER).
	// Применяем ТОЛЬКО к запросам, реально прижатым полом: у медленных тарифов
	// (free-поиск 6ч) ±40% — это ±2.4ч, чего нам не надо.
	if ozonSearchMin > 0 {
		for id, a := range byQuery {
			if a.mp == "ozon" && a.eff < ozonSearchMin {
				a.eff = scaleDuration(ozonSearchMin,
					jitterFactor(id, a.lastEn, ozonSearchJitter))
			}
		}
	}

	// Отбираем созревшие.
	var due []dueQuery
	for id, a := range byQuery {
		// Дорожку выбираем ДО волатильности: бэкофф не должен выкидывать
		// перекупа из fast-lane. Запросы с reseller-подписчиком (fastSub) не
		// замедляем совсем — в т.ч. Ozon, ушедший с fast-дорожки по полу 30м.
		fast := a.eff <= resellerLaneCutoff
		eff := a.eff
		if !a.fastSub {
			// Волатильностный бэкофф + потолок: любая выдача проверяется
			// минимум дважды в сутки (free-поиск 6ч не уезжает дальше 12ч).
			eff = domain.ApplyVolatility(eff, a.lastChange, a.subs, now, domain.SearchIntervalCeil)
		}
		if a.lastEn != nil && now.Sub(*a.lastEn) < eff {
			continue // ещё не пора
		}
		due = append(due, dueQuery{id: id, url: a.url, text: a.text, fast: fast})
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

func getEnvFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}
