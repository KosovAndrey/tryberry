package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/partition"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
	"gitlab.com/KosovAndrey/tryberrybot/internal/tracing"
	"golang.org/x/sync/errgroup"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(log); err != nil {
		log.Error("scraper failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	_ = godotenv.Load()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// ── Config из env ────────────────────────────────────────────────────────
	databaseURL := config.MustEnv("DATABASE_URL")
	redisURL := config.MustEnv("REDIS_URL")
	kafkaBrokers := strings.Split(config.MustEnv("KAFKA_BROKERS"), ",")
	kafkaGroupID := config.MustEnv("KAFKA_GROUP_ID")
	otlpEndpoint := getEnv("OTLP_ENDPOINT", "jaeger:4317")
	rpsStrWB := getEnv("SCRAPER_RATE_LIMIT_RPS_WB", "5")
	// Я.Маркет теперь direct (без прокси, см. docs/YANDEX-WARMED-COOKIES.md) —
	// probe держал сотни запросов без капчи, прежний потолок 2 был из-за одного
	// proxy-IP. Поднимаем до 8; сторож — метрика yandex_price_source_total{proxy}.
	rpsStrYandex := getEnv("SCRAPER_RATE_LIMIT_RPS_YANDEX", "8")
	rpsStrOzon := getEnv("SCRAPER_RATE_LIMIT_RPS_OZON", "1")
	rpsStrAli := getEnv("SCRAPER_RATE_LIMIT_RPS_ALI", "1")
	rpsWB, err := strconv.ParseFloat(rpsStrWB, 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS_WB: %w", err)
	}
	rpsYandex, err := strconv.ParseFloat(rpsStrYandex, 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS_YANDEX: %w", err)
	}
	rpsOzon, err := strconv.ParseFloat(rpsStrOzon, 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS_OZON: %w", err)
	}
	rpsAli, err := strconv.ParseFloat(rpsStrAli, 64)
	if err != nil {
		return fmt.Errorf("SCRAPER_RATE_LIMIT_RPS_ALI: %w", err)
	}
	// CONSUMER_CONCURRENCY — сколько scrape-задач обрабатывать параллельно в одном
	// инстансе. Скрейп I/O-bound (нагрузочный тест 2026-07-05: CPU ~15% при
	// последовательном консьюмере), поэтому пул горутин снимает главный потолок
	// пропускной без доп. реплик. Per-marketplace RPS всё равно капится rate.Limiter
	// в скрейперах — параллелизм лишь заполняет время ожидания ответа. 1 = прежний
	// последовательный режим. docs/THROUGHPUT-ROADMAP.md №1.
	concurrency, err := strconv.Atoi(getEnv("CONSUMER_CONCURRENCY", "8"))
	if err != nil || concurrency < 1 {
		return fmt.Errorf("CONSUMER_CONCURRENCY: must be a positive integer, got %q", getEnv("CONSUMER_CONCURRENCY", "8"))
	}
	// OZON_CONSUMER_CONCURRENCY — параллелизм ОТДЕЛЬНОГО консьюмера ozon-scrape-tasks
	// (docs/THROUGHPUT-ROADMAP.md №2). Ozon вынесен в свой топик, чтобы его медленные
	// (~3-5с) браузерные скрейпы не занимали слоты пула быстрых WB/YM. Держим скромнее
	// основного: rpsOzon=1/с на реплику всё равно потолок, пул лишь заполняет ожидание
	// сайдкара. Реальный рычаг Ozon-пропускной — rpsOzon + дорожки ozon-miner.
	ozonConcurrency, err := strconv.Atoi(getEnv("OZON_CONSUMER_CONCURRENCY", "6"))
	if err != nil || ozonConcurrency < 1 {
		return fmt.Errorf("OZON_CONSUMER_CONCURRENCY: must be a positive integer, got %q", getEnv("OZON_CONSUMER_CONCURRENCY", "6"))
	}

	// ── Подключения ──────────────────────────────────────────────────────────

	shutdownTracing, err := tracing.Init(ctx, "scraper", otlpEndpoint)
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
		// Redis недоступен — работаем без кэша, только логируем
		log.Warn("redis unavailable, running without cache", "err", err)
		redisClient = nil
	}

	// ── Автосоздание партиций ────────────────────────────────────────────────
	pm := partition.NewManager(pool)
	if err := pm.EnsurePartitions(ctx, 2); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}
	log.Info("partitions ok")

	// Периодически до-создаём партиции: EnsurePartitions на старте покрывает +2 мес,
	// но при длинном аптайме (>2 мес без рестарта) партиция нового месяца не появится
	// и INSERT в price_history упадёт. Суточный тик держит окно открытым.
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := pm.EnsurePartitions(ctx, 2); err != nil {
					log.Error("ensure partitions (periodic)", "err", err)
				} else {
					log.Info("partitions ensured (periodic)")
				}
			}
		}
	}()

	go health.RunServer(ctx, log, pool, redisClient, "8090")

	// ── Репозитории ──────────────────────────────────────────────────────────
	productRepo := postgres.NewProductRepo(pool)
	priceHistoryRepo := postgres.NewPriceHistoryRepo(pool)

	var priceCache *redisrepo.PriceCache
	if redisClient != nil {
		priceCache = redisrepo.NewPriceCache(redisClient)
	}

	// ── Kafka ────────────────────────────────────────────────────────────────
	// Товарный путь: scrape-tasks → (этот consumer) → price-events.
	// Поисковый путь вынесен в отдельный бинарь cmd/search-worker.
	consumer := kafka.NewConsumer(kafkaBrokers, "scrape-tasks", kafkaGroupID)
	defer consumer.Close()

	// Ozon вынесен в отдельный топик + СВОЮ consumer-group (изоляция rebalance и
	// отдельный lag в мониторинге, как reseller-tasks). Тот же handler — registry
	// сам роутит по URL; сюда просто приходят только Ozon-задачи от планировщика.
	ozonConsumer := kafka.NewConsumer(kafkaBrokers, "ozon-scrape-tasks", kafkaGroupID+"-ozon")
	defer ozonConsumer.Close()

	producer := kafka.NewProducer(kafkaBrokers, "price-events")
	defer producer.Close()

	// ── Скрейперы (товарные карточки) ──────────────────────────────────────────
	wbCard := scraper.NewWildberriesScraper(rpsWB)
	if redisClient != nil {
		bc := redisrepo.NewBasketCache(redisClient)
		wbCard.SetBasketResolver(bc)
		// Снимки для conditional GET: 304 вместо полного скрейпа на стабильной
		// цене (метрика wb_cond_get_total). Без Redis — полный скрейп, как раньше.
		wbCard.SetCondCache(bc)
	}
	// u-card-fallback (трансграничные товары) через прокси: с прямого RU-IP воркера
	// u-card отдаёт 403, зарубежный/чистый выход (xray) — принимает.
	if err := wbCard.SetUCardProxy(getEnv("WB_UCARD_PROXY_URL", "")); err != nil {
		log.Warn("bad WB_UCARD_PROXY_URL, u-card fallback uses direct egress", "err", err)
	}
	registry := scraper.NewRegistry(
		wbCard,
		scraper.NewOzonScraper(scraper.OzonOptions{
			ProxyURL:     getEnv("OZON_PROXY_URL", ""),
			RPS:          rpsOzon,
			Mode:         getEnv("OZON_API_MODE", "mobile"),
			AccessToken:  getEnv("OZON_ACCESS_TOKEN", ""),
			RefreshToken: getEnv("OZON_REFRESH_TOKEN", ""),
			Cookie:       getEnv("OZON_COOKIE", ""),
			BrowserURL:   getEnv("OZON_BROWSER_URL", ""),
			Logger:       log,
		}),
		scraper.NewYandexMarketScraper(scraper.YandexMarketOptions{
			// Без аккаунта: хороший TLS + RU-прокси. По умолчанию переиспользуем
			// мобильный прокси Ozon (один IP). ВНИМАНИЕ: дележ IP ускоряет его
			// выгорание (см. OZON-STATUS) — при росте нагрузки задать отдельный.
			ProxyURL: getEnv("YANDEX_PROXY_URL", getEnv("OZON_PROXY_URL", "")),
			RPS:      rpsYandex,
			Logger:   log,
		}),
		scraper.NewAliexpressScraper(scraper.AliexpressOptions{
			// aliexpress.ru: внутренний JSON-API (aer-jsonapi productData), без
			// браузера. Ходит direct с датацентр-IP; ALI_PROXY_URL — опциональный
			// RU-прокси-фолбэк на рефреш cookie, задать если X5SEC начнёт рубить
			// cold-сессии (сигнал: рост ErrMarketplaceBlocked по aliexpress).
			ProxyURL: getEnv("ALI_PROXY_URL", getEnv("OZON_PROXY_URL", "")),
			RPS:      rpsAli,
			Logger:   log,
		}),
	)

	// ── Обработчик сообщений (цены) — блокирующий основной цикл ────────────────
	handler := makeHandler(log, registry, productRepo, priceHistoryRepo, priceCache, producer, pm)

	log.Info("scraper started, waiting for tasks...",
		"concurrency", concurrency, "ozon_concurrency", ozonConcurrency)

	// Два независимых консюмера в одном инстансе: быстрый WB/YM/Ali (scrape-tasks)
	// и медленный Ozon (ozon-scrape-tasks). errgroup: падение/останов любого гасит
	// оба (общий egCtx), ctx-cancel завершает штатно (RunConcurrent → nil).
	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(func() error { return consumer.RunConcurrent(egCtx, handler, concurrency) })
	eg.Go(func() error { return ozonConsumer.RunConcurrent(egCtx, handler, ozonConcurrency) })
	return eg.Wait()
}

func makeHandler(
	log *slog.Logger,
	registry *scraper.Registry,
	productRepo *postgres.ProductRepo,
	priceHistoryRepo *postgres.PriceHistoryRepo,
	priceCache *redisrepo.PriceCache,
	producer *kafka.Producer,
	pm *partition.Manager,
) kafka.HandlerFunc {
	return func(ctx context.Context, msg kafka.Message) error {
		task, err := kafka.Decode[domain.ScrapeTask](msg)
		if err != nil {
			log.Error("decode scrape task", "err", err)
			return nil
		}

		log := log.With("product_id", task.ProductID, "url", task.URL)

		// Скрейпим через registry — он сам выбирает нужный маркетплейс
		result, marketplace, err := registry.Scrape(ctx, task.URL)
		if err != nil {
			// Перманентные ошибки (товар не найден / битый URL) не ретраим: иначе
			// один нескрейпящийся товар (удалённый / трансгран без цены на этом
			// egress) застревает в петле и лагает весь консьюмер. Пропускаем —
			// следующая плановая задача по этому товару попробует снова.
			if errors.Is(err, scraper.ErrProductNotFound) || errors.Is(err, scraper.ErrInvalidURL) {
				log.Warn("scrape skipped (permanent)", "err", err)
				return nil
			}
			log.Error("scrape failed", "err", err)
			return err
		}
		log.Info("scraped", "marketplace", marketplace, "name", result.Name, "price", result.Price)

		// Обновляем product (имя/картинка/наличие); получаем ПРЕДЫДУЩЕЕ наличие для
		// детекта перехода «нет в наличии»→«появилось» (триггер back_in_stock).
		wasInStock, err := productRepo.UpdateScrapedData(ctx, task.ProductID, result.Name, result.ImageURL, result.InStock)
		if err != nil {
			return fmt.Errorf("update product: %w", err)
		}

		// Получаем предыдущую цену (из Redis или PostgreSQL)
		prevPrice, err := getPrevPrice(ctx, log, task.ProductID, priceCache, priceHistoryRepo)
		if err != nil {
			log.Warn("could not get prev price, skipping event", "err", err)
		}

		// price_history/кэш обновляем ТОЛЬКО когда товар в наличии: запись нулевой
		// цены для OOS засорила бы аналитику и дала ложный price drop. Событие шлём
		// всегда — notifier обрабатывает и появление в наличии, и снижение цены.
		if result.InStock {
			// CHANGE-ONLY: пишем в историю лишь при СМЕНЕ цены. При 1-мин кадансе
			// reseller хранить идентичные точки расточительно (~99% дублей); сегмент
			// «цена X действует с t0» восстанавливаем на чтении (PriceHistoryRepo.Stats
			// взвешивает по длительности). prevPrice<=0 → первая точка по товару.
			// Кэш последней цены обновляем ВСЕГДА — на нём держится детект снижения.
			// Дозаливаем недостающую СТАРУЮ историю от маркетплейса (WB
			// price-history.json): покрывает и новые товары, и добавленные до
			// появления бэкфилла (у них своя история начинается с момента трекинга).
			// Самоограничивается дешёвым пред-чеком внутри (EarliestRecordedAt) —
			// в установившемся режиме это один индексный запрос. Best-effort:
			// ошибка бэкфилла не валит обычную запись цены.
			if len(result.History) > 0 {
				backfillHistory(ctx, log, pm, priceHistoryRepo, task.ProductID, result.History)
			}
			if prevPrice <= 0 || !pricesEqual(result.Price, prevPrice) {
				if err := priceHistoryRepo.Insert(ctx, task.ProductID, result.Price); err != nil {
					return fmt.Errorf("insert price history: %w", err)
				}
				// Смена цены сбрасывает волатильностный бэкофф планировщика
				// (docs/TARIFF-FREE-SEARCH-LINK.md §2). Best-effort: не сорвал
				// запись истории — не валим и скрейп.
				if err := productRepo.TouchPriceChanged(ctx, task.ProductID); err != nil {
					log.Warn("touch price changed", "product_id", task.ProductID, "err", err)
				}
			}
			if priceCache != nil {
				if err := priceCache.Set(ctx, task.ProductID, result.Price); err != nil {
					log.Warn("redis set failed", "err", err)
				}
			}
		}

		// Публикуем событие на КАЖДОМ скрейпе (не только при изменении цены):
		// строгая per-plan модель требует, чтобы notifier мог оценить подписку на
		// её чек-поинте по текущей цене. Иначе free-подписчик (60 мин) пропустит
		// устойчивое падение, случившееся между событиями «по изменению». Частоту
		// доставки режет throttle (last_evaluated_at) в notifier.
		// При OOS result.Price может быть «последней» ценой из стейта — в событие её
		// НЕ кладём (NewPrice=0): notifier выводит наличие в т.ч. из NewPrice>0
		// (страховка для старых событий), и ненулевая last-цена ложно пометила бы
		// товар «в наличии», сломав триггер back_in_stock.
		newPrice := result.Price
		if !result.InStock {
			newPrice = 0
		}
		event := domain.PriceEvent{
			ProductID:   task.ProductID,
			Marketplace: string(marketplace),
			OldPrice:    prevPrice,
			NewPrice:    newPrice,
			RecordedAt:  time.Now(),
			InStock:     result.InStock,
			WasInStock:  wasInStock,
		}
		key := strconv.FormatInt(task.ProductID, 10)
		if err := producer.Send(ctx, key, event); err != nil {
			return fmt.Errorf("send price event: %w", err)
		}
		if result.InStock && prevPrice > 0 && result.Price < prevPrice {
			metrics.PriceDrops.WithLabelValues(string(marketplace)).Inc()
			log.Info("price dropped",
				"marketplace", marketplace, "old_price", prevPrice, "new_price", result.Price)
		}

		return nil
	}
}

// getPrevPrice — получить предыдущую цену из Redis, fallback на PostgreSQL
func getPrevPrice(
	ctx context.Context,
	log *slog.Logger,
	productID int64,
	cache *redisrepo.PriceCache,
	histRepo *postgres.PriceHistoryRepo,
) (float64, error) {
	if cache != nil {
		price, err := cache.Get(ctx, productID)
		if err == nil {
			return price, nil
		}
		log.Debug("redis miss, fallback to postgres", "product_id", productID)
	}

	price, _, err := histRepo.GetLatest(ctx, productID)
	return price, err
}

// backfillHistory дозаливает историческую серию маркетплейса в price_history:
// точки СТАРШЕ нашей самой ранней (покрывает и новые товары, и трекаемые до
// появления бэкфилла). Best-effort: ошибки логируем и идём дальше — это бонус-
// наполнение графика, оно не должно ронять обработку задачи. Сначала дешёвый
// пред-чек (EarliestRecordedAt), затем партиции прошлых месяцев и PrependOlder
// (повторно проверяет минимум под advisory-lock).
func backfillHistory(
	ctx context.Context,
	log *slog.Logger,
	pm *partition.Manager,
	histRepo *postgres.PriceHistoryRepo,
	productID int64,
	history []scraper.PriceHistoryPoint,
) {
	// Дешёвый пред-чек: тянем только точки СТАРШЕ нашей самой ранней (или все, если
	// истории нет). В установившемся режиме older пуст → ни партиций, ни лока.
	earliest, hasAny, err := histRepo.EarliestRecordedAt(ctx, productID)
	if err != nil {
		log.Warn("backfill: earliest lookup failed", "err", err, "product_id", productID)
		return
	}
	times := make([]time.Time, 0, len(history))
	older := make([]postgres.PricePoint, 0, len(history))
	for _, h := range history {
		if hasAny && !h.At.Before(earliest) {
			continue
		}
		times = append(times, h.At)
		older = append(older, postgres.PricePoint{RecordedAt: h.At, Price: h.Price})
	}
	if len(older) == 0 {
		return // нечего дозаливать
	}
	if err := pm.EnsureForTimes(ctx, times); err != nil {
		log.Warn("backfill: ensure partitions failed", "err", err, "product_id", productID)
		return
	}
	n, err := histRepo.PrependOlder(ctx, productID, older)
	if err != nil {
		log.Warn("backfill: insert failed", "err", err, "product_id", productID)
		return
	}
	if n > 0 {
		log.Info("price history backfilled", "product_id", productID, "points", n)
	}
}

// pricesEqual — равенство ДЕНЕГ с точностью до копейки, а не строгое float-сравнение.
// Корень бага: WB отдаёт цену делением kopecks/100, а pgx конвертит NUMERIC(12,2)
// из БД в float64 умножением на 10^-2 — у дробных цен (копейки) младшие биты
// расходятся, и строгое `!=` считало цену «изменившейся» на КАЖДОМ скрейпе →
// price_history WB пухла тысячами идентичных точек (у Ozon/ЯМ цены целые, эффекта
// не было). Округляем до копеек: реальное изменение цены всегда ≥ 0.01.
func pricesEqual(a, b float64) bool {
	return math.Round(a*100) == math.Round(b*100)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
