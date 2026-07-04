package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const namespace = "tryberrybot"

// ── Бизнес-метрики ───────────────────────────────────────────────────────────

var (
	// Counters — растут только вверх

	TrackCommands = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "track_commands_total",
			Help:      "Total /track commands processed",
		},
		[]string{"result"}, // success | duplicate | reactivated | error
	)

	NotificationsSent = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "notifications_sent_total",
			Help:      "Total notifications sent to users",
		},
		[]string{"marketplace"},
	)

	PriceDrops = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "price_drops_total",
			Help:      "Total price drop events detected by scraper",
		},
		[]string{"marketplace"},
	)

	// Gauges — могут расти и падать

	ActiveSubscriptions = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "active_subscriptions",
			Help:      "Current number of active subscriptions",
		},
	)

	TotalUsers = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "users_total",
			Help:      "Current number of registered users",
		},
	)

	SubscriptionsByMarketplace = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "subscriptions_by_marketplace",
			Help:      "Active subscriptions grouped by marketplace",
		},
		[]string{"marketplace"},
	)

	// NotificationsDelivered — попытки доставки уведомлений по каналам
	// (notifier, deliverer). channel=none/status=skipped — доставлять
	// было некуда (например, VK-only юзер при выключенном VK).
	NotificationsDelivered = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "notifications_delivered_total",
			Help:      "Notification delivery attempts by channel",
		},
		[]string{"channel", "status"}, // tg|vk|none × ok|error|skipped
	)

	// VKMessages — входящие личные сообщения VK-бота по распознанному
	// типу: имя команды (profile, link, plans…) либо текстовый флоу
	// (link_code, promo_code, ref_code, track_url, search_url,
	// track_threshold, search_threshold, other).
	VKMessages = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vk_messages_total",
			Help:      "Incoming VK messages by resolved command/flow",
		},
		[]string{"command"},
	)

	// MaxMessages — входящие личные сообщения MAX-бота по распознанному
	// типу (зеркало VKMessages).
	MaxMessages = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "max_messages_total",
			Help:      "Incoming MAX messages by resolved command/flow",
		},
		[]string{"command"},
	)

	// PromoRedeems — успешные погашения промокодов (единая точка —
	// PromoRepo.RedeemGrant, канал TG/VK тут не различим).
	PromoRedeems = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "promo_redeems_total",
			Help:      "Successful promo code redemptions",
		},
	)

	// AccountMerges — успешные слияния TG/VK-аккаунтов
	// (UserRepo.MergeAccounts).
	AccountMerges = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "account_merges_total",
			Help:      "Successful TG/VK account merges",
		},
	)

	// PaymentsCreated — созданные платежи ЮKassa (нажата «Оплатить»).
	PaymentsCreated = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "payments_created_total",
			Help:      "YooKassa payments created (checkout started)",
		},
	)

	// PaymentsSucceeded — успешно оплаченные платежи (применён план).
	PaymentsSucceeded = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "payments_succeeded_total",
			Help:      "YooKassa payments succeeded and applied",
		},
	)

	// PaymentRevenueKopecks — суммарная выручка по успешным платежам, копейки.
	PaymentRevenueKopecks = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "payment_revenue_kopecks_total",
			Help:      "Total revenue from succeeded payments, in kopecks",
		},
	)
)

// ── Поиск-подписки ───────────────────────────────────────────────────────────

var (
	// SearchScrapes — циклы скрейпа поисковой выдачи по статусу.
	SearchScrapes = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "search_scrapes_total",
			Help:      "Total search-result scrape cycles by status",
		},
		[]string{"status"}, // success | empty | error
	)

	// SearchNotificationsSent — отправленные батч-уведомления по поиск-подпискам,
	// сгруппированные по типу триггера.
	SearchNotificationsSent = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "search_notifications_sent_total",
			Help:      "Total search-subscription notifications sent, by trigger type",
		},
		[]string{"trigger"}, // below_target | any_drop | discount_pct
	)

	// ActiveSearchSubscriptions — текущее число активных поиск-подписок.
	ActiveSearchSubscriptions = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "active_search_subscriptions",
			Help:      "Current number of active search subscriptions",
		},
	)
)

// ── Тарифы ───────────────────────────────────────────────────────────────────

var (
	// UsersByPlan — распределение пользователей по тарифу (значение столбца plan
	// в БД; истёкшие триалы здесь считаются как 'trial', эффективный план
	// вычисляется в рантайме отдельно).
	UsersByPlan = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "users_by_plan",
			Help:      "Registered users grouped by stored plan name",
		},
		[]string{"plan"}, // free | trial | basic | pro | unlimited
	)
)

// ── Scraper ──────────────────────────────────────────────────────────────────

var (
	ScrapeDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "scrape_duration_seconds",
			Help:      "Duration of marketplace scraping in seconds",
			Buckets:   []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		},
		[]string{"marketplace"},
	)

	ScrapeRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "scrape_requests_total",
			Help:      "Total scrape requests by marketplace and status",
		},
		[]string{"marketplace", "status"}, // success | not_found | blocked | proxy | auth | disabled | parse_error | error
	)

	// OzonAgeGateRetries считает ретраи age-gate (anon→authed) у Ozon:
	// outcome = success | failed. Сумма по outcome = объём 18+ товаров.
	OzonAgeGateRetries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "ozon_age_gate_retries_total",
			Help:      "Ozon age-gate retries via authed lane by outcome (success|failed)",
		},
		[]string{"outcome"},
	)

	// WBBasketResolve — как резолвился basket-шард WB для товара. Формула
	// wbBasketNumber мажет для новых vol, поэтому пробуем соседние шарды и кэшируем.
	// outcome: cache (из кэша) | formula (кандидат точен) | probe (сосед ±4) |
	// probe_far (далёкий сосед 5..12 — формула сильно уехала, обновить маппинг) |
	// not_found (не нашли нигде в окне — удалён/трансгран/баскет за окном).
	WBBasketResolve = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "wb_basket_resolve_total",
			Help:      "How WB basket shard was resolved for a product",
		},
		[]string{"outcome"},
	)

	// WBPriceSource — каким источником взята цена WB-карточки. ucard — основной
	// (u-card.wb.ru, real-time, без перебора баскетов, видит трансграничные);
	// basket — fallback на basket-CDN price-history (u-card не ответил).
	WBPriceSource = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "wb_price_source_total",
			Help:      "Which source served the WB product price",
		},
		[]string{"source"},
	)

	// AliPriceSource — каким путём взята цена AliExpress. direct — основной (запрос
	// к aer-jsonapi напрямую с датацентр-IP по сессионной cookie, без прокси); proxy
	// — fallback-рефреш (cookie протухла → один запрос через RU-прокси, он же
	// обновляет jar). Рост доли proxy = чаще платим за прокси / короче TTL cookie.
	AliPriceSource = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "ali_price_source_total",
			Help:      "Which path served the AliExpress product price (direct vs proxy-refresh)",
		},
		[]string{"source"},
	)

	// YandexPriceSource — каким путём взята цена Я.Маркета. direct — основной
	// (карточка напрямую с датацентр-IP, без прокси: проверено probe'ом — IP держит
	// поток без капчи); proxy — fallback, когда direct упёрся в SmartCaptcha (один
	// запрос через RU-прокси). Рост доли proxy = датацентр-IP начал ловить капчу.
	YandexPriceSource = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "yandex_price_source_total",
			Help:      "Which path served the Yandex Market product price (direct vs proxy-fallback)",
		},
		[]string{"source"},
	)

	// MaxActiveSubsPerUser — максимум активных товарных подписок у одного юзера.
	// Сигнал «пора делать бандлинг алертов»: пока мало (единицы) — пачек уведомлений
	// за цикл почти нет; когда вырастет (~15+) или появятся reseller-юзеры — за цикл
	// у юзера может падать много товаров → нужен коалесцер (см. задачу «Бандлинг»).
	MaxActiveSubsPerUser = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "max_active_subs_per_user",
			Help:      "Max active product subscriptions held by a single user (bundling-need signal)",
		},
	)

	// PendingAlertsDepth — глубина outbox-очереди доставки (недоставленные строки
	// pending_alerts). Растёт = флашер не успевает слать (упёрлись в Telegram-лимит
	// / egress лёг). См. docs/SCALING-NOTIFIER-DELIVERY.md.
	PendingAlertsDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "pending_alerts_depth",
			Help:      "Undelivered rows in the pending_alerts delivery outbox",
		},
	)

	// PendingAlertsDelivered — исход доставки строки из outbox (флашер).
	PendingAlertsDelivered = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "pending_alerts_delivered_total",
			Help:      "Outbox delivery attempts by outcome",
		},
		[]string{"outcome"}, // sent | failed
	)
)

// ── Kafka ────────────────────────────────────────────────────────────────────

var (
	KafkaMessagesProduced = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "kafka_messages_produced_total",
			Help:      "Total Kafka messages produced",
		},
		[]string{"topic", "status"}, // success | error
	)

	KafkaMessagesConsumed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "kafka_messages_consumed_total",
			Help:      "Total Kafka messages consumed",
		},
		[]string{"topic", "status"}, // success | error
	)

	KafkaProcessingDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "kafka_processing_duration_seconds",
			Help:      "Time to process one Kafka message",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 30},
		},
		[]string{"topic"},
	)
)

// ── HTTP (для middleware) ────────────────────────────────────────────────────

var (
	HTTPRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_requests_total",
			Help:      "Total HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	HTTPDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request duration in seconds",
			Buckets:   []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
		},
		[]string{"method", "path"},
	)
)

// ── Database ─────────────────────────────────────────────────────────────────

var (
	DBQueries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "db_queries_total",
			Help:      "Total database queries",
		},
		[]string{"operation", "status"}, // operation: upsert_user, get_active_subs, etc
	)

	DBQueryDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "db_query_duration_seconds",
			Help:      "Database query duration in seconds",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1},
		},
		[]string{"operation"},
	)
)

// ── Redis ────────────────────────────────────────────────────────────────────

var (
	RedisOperations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "redis_operations_total",
			Help:      "Total Redis operations",
		},
		[]string{"op", "status"}, // op: get/set/del; status: hit/miss/error
	)
)

// ── Telegram ─────────────────────────────────────────────────────────────────

var (
	TelegramAPICallsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "telegram_api_calls_total",
			Help:      "Total calls to Telegram Bot API",
		},
		[]string{"method", "status"},
	)
)
