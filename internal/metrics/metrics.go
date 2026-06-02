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
		[]string{"marketplace", "status"}, // success | not_found | blocked | error
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
