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
	// status=rejected — получатель недоставляем (заблокировал бота, удалил
	// аккаунт): канал жив, алерт NotificationChannelFailing это не считает.
	NotificationsDelivered = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "notifications_delivered_total",
			Help:      "Notification delivery attempts by channel",
		},
		[]string{"channel", "status"}, // tg|vk|max|none|synth × ok|error|rejected|skipped
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

	// ── Воронка конверсии (фри-поиск → триал → платный) ──────────────────────
	// Ставим ДО запуска: конверсию задним числом не измеришь. Считаем три стадии
	// воронки, чтобы после старта видеть free→lite/pro.

	// TariffUpgrades — смена тарифа по УСПЕШНОМУ платежу: from = план до покупки,
	// to = купленный. from==to = продление, from=free/trial → to=lite/pro =
	// целевая конверсия воронки. Инкрементит payment.MarkSucceeded.
	TariffUpgrades = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "tariff_upgrades_total",
			Help:      "Paid tariff changes by from/to plan (from==to = renewal)",
		},
		[]string{"from", "to"},
	)

	// TrialActivations — активации бесплатного триала по каналу входа (tg|vk|max):
	// вход в воронку после фри-поиска. Инкрементит handleTrial при успехе.
	TrialActivations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "trial_activations_total",
			Help:      "Free trial activations by channel",
		},
		[]string{"channel"},
	)

	// SearchUpsellShown — показ апселла фри-юзеру, упёршемуся в лимит поиска
	// (каналы tg|vk|max): спрос на «больше поисков» = потенциал апгрейда.
	// Инкрементит free-ветка searchLimitText.
	SearchUpsellShown = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "search_upsell_shown_total",
			Help:      "Free-tier search-limit upsell shown, by channel",
		},
		[]string{"channel"},
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

	// SearchTasksStale — задачи поиска, отброшенные как устаревшие (продюсер
	// быстрее консьюмера). Ноль = дорожка успевает. Устойчивый рост = каданс
	// обещает больше, чем дорожка отдаёт: разбирать хвост бессмысленно (задача
	// «сходи за выдачей» переотправляется каждый каданс), но и молчать нельзя —
	// подписчик получает свежесть реже обещанной тарифом. Инцидент 02-09-2026:
	// reseller-tasks копил 4 задачи/мин при пропускной способности ~1/мин, лаг
	// рос сутки и упёрся в алерт, хотя ни одна задача не «застряла».
	SearchTasksStale = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "search_tasks_stale_total",
			Help:      "Search tasks skipped because they were older than the freshness window",
		},
		[]string{"topic"},
	)

	// WBSearchFetch — исходы запросов страниц WB-поиска по транспорту и результату.
	// transport: direct | browser; result: ok | forbidden | 429 | other | error.
	// direct+error = запрос не дошёл вовсе (мёртвый egress/прокси), а не ответ WB.
	// browser+ok = горячий запрос, спасённый браузер-сайдкаром wb-search-miner
	// (см. wildberries_search.go). browser+forbidden = челлендж не пройден и в
	// браузере. direct+forbidden = обычный 403 на горячем (уходит в сайдкар).
	WBSearchFetch = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "wb_search_fetch_total",
			Help:      "WB search page fetches by transport and result",
		},
		[]string{"transport", "result"},
	)

	// AliSearchFetch — исходы запросов страниц Ali-поиска по транспорту и
	// результату. transport: direct | proxy | browser; result: ok | blocked |
	// empty | other | error. blocked = X5SEC-стена (выдачу X5SEC проверяет
	// строже карточного productData); browser+ok = запрос, спасённый сайдкаром
	// ali-miner (см. aliexpress_search.go). Рост direct+blocked при нулевом
	// browser+ok = поиск Ali лежит.
	AliSearchFetch = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "ali_search_fetch_total",
			Help:      "Ali search page fetches by transport and result",
		},
		[]string{"transport", "result"},
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
		[]string{"marketplace", "status"}, // success | not_found | blocked | proxy | auth | disabled | parse_error | dead_url | error
	)

	// ScrapeCircuitOpen — 1, пока брейкер держит цепь площадки разомкнутой (мы
	// намеренно не стучимся: площадка режет по IP). Основной сигнал «скрейпинг
	// площадки стоит»: сам MarketplaceBlocked после размыкания затихает, потому
	// что настоящих запросов больше нет.
	ScrapeCircuitOpen = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "scrape_circuit_open",
			Help:      "1 while the blocked-breaker keeps the marketplace circuit open (requests suppressed)",
		},
		[]string{"marketplace"},
	)

	// ScrapeSuppressed — сколько запросов брейкер задавил, не сходив в сеть.
	// Намеренно НЕ идёт в scrape_requests_total: тот счётчик означает «сходили к
	// площадке», и подмешивание в него давленых запросов испортило бы и success
	// rate, и алерт по блокировкам.
	ScrapeSuppressed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "scrape_suppressed_total",
			Help:      "Scrape requests suppressed by the blocked-breaker (no network call made)",
		},
		[]string{"marketplace"},
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

	// WBUCardFetch — исход запроса живой цены к u-card.wb.ru (через xray).
	// Без него wb_price_source{basket} говорит «u-card не смог», но молчит ПОЧЕМУ:
	// forbidden — WB режет наш exit (по частоте или целиком); timeout — не тянет
	// прокси; empty — карточки нет; error — сеть/парсинг. Заведена 2026-07-16,
	// когда basket держал 77% и диагностировать было нечем.
	WBUCardFetch = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "wb_ucard_fetch_total",
			Help:      "Outcome of live WB u-card fetch (ok|forbidden|timeout|empty|error)",
		},
		[]string{"outcome"},
	)

	// WBCondGet — исход conditional GET price-history.json на basket-CDN
	// (дешёвый change-detection перед полным скрейпом). not_modified — 304,
	// цена не менялась, card.json не запрашивался (сэкономленный полный скрейп);
	// modified — 200, файл перегенерирован (обычно смена цены, реже — тот же
	// контент с новым mtime); miss — снимка в кэше нет/битый → полный скрейп.
	// Доля not_modified = КПД фичи; ожидаемо высока из-за недельного каданса
	// файла у WB.
	WBCondGet = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "wb_cond_get_total",
			Help:      "Outcome of conditional GET on WB basket price-history (not_modified|modified|miss)",
		},
		[]string{"outcome"},
	)

	// WBPriceSource — каким источником взята цена WB-карточки:
	//   ucard  — ЖИВАЯ цена с u-card.wb.ru через xray (основной с 2026-07-16);
	//   basket — архив basket-CDN price-history, отстаёт на ДНИ (u-card не ответил:
	//            лёг прокси / 403). Рост доли = деградация ПРАВДИВОСТИ цен, при
	//            этом success rate остаётся 100% — следить надо именно здесь, глазами
	//            такое ловилось только сверкой с живой карточкой.
	WBPriceSource = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "wb_price_source_total",
			Help:      "Which source served the WB product price (ucard=live via xray|basket=stale archive)",
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

	// YandexEmptySKUPrice — счётчик скрейпов Я.Маркета, где sku из URL пустой,
	// а цену всё равно достали из стейта ЖАДНЫМ фолбэком (ymStatePrice берёт
	// первое вхождение locs[0]). Это тот самый класс бага, что у ozon-oos: при
	// пустом sku «первая цена на странице» может принадлежать чужому товару из
	// рекомендаций. Пустой sku = URL без числового сегмента, типично неразвёрнутый
	// шорт /cc/<код> (SmartCaptcha не дал резолву). Метрика ставится ПЕРЕД сменой
	// поведения: сперва измеряем объём (сколько таких скрейпов реально идёт),
	// потом решаем, отдавать ли 0 вместо чужой цены. Рост = пора чинить фолбэк.
	YandexEmptySKUPrice = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "yandex_empty_sku_price_total",
			Help:      "YM scrapes where sku was empty but a price was still taken via greedy state fallback (possible foreign price, ozon-oos class)",
		},
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

	// TelegramConnected — 1 после успешного getMe, 0 пока InitWithRetry ретраится.
	// Урок инцидента 2026-07-08: битый токен в .env → бот 2 часа крутил getMe
	// Unauthorized, а ServiceDown молчал (контейнер жив, /metrics отвечает).
	// Экспортируется всеми бинарями (promauto), поэтому алерт фильтрует по job
	// api/bot-worker — только они инициализируют Telegram.
	TelegramConnected = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "telegram_connected",
			Help:      "1 when Telegram getMe succeeded, 0 while init is retrying",
		},
	)

	// TelegramPollLastSuccess — unix-время последнего успешного getUpdates в api.
	// Long-poll возвращается каждые ~poll-timeout секунд даже без апдейтов, поэтому
	// застывшее значение = поллинг мёртв (токен отозван на лету / egress лёг),
	// независимо от того, пишут ли юзеры. 0 до первого успеха (webhook-режим —
	// всегда 0, алерт это учитывает).
	TelegramPollLastSuccess = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "telegram_poll_last_success_timestamp_seconds",
			Help:      "Unix time of the last successful getUpdates long-poll",
		},
	)

	// TelegramPollErrors — ошибки getUpdates (диагностика к TelegramPollingStale).
	TelegramPollErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "telegram_poll_errors_total",
			Help:      "Failed getUpdates long-poll requests",
		},
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

	// KafkaInflight — число сообщений в обработке ПРЯМО СЕЙЧАС в конкурентном
	// консьюмере (RunConcurrent). Приближается к CONSUMER_CONCURRENCY, когда пул
	// насыщен (упёрлись в потолок параллелизма), около нуля — простаиваем.
	KafkaInflight = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "kafka_inflight_messages",
			Help:      "Messages currently being processed by the concurrent consumer",
		},
		[]string{"topic"},
	)

	// KafkaPartitionStuck — сколько секунд худшая партиция инстанса не может
	// сдвинуть вотермарк коммита, накапливая при этом завершённые оффсеты. В норме
	// секунды. Минуты = партиция заморожена при живом консьюмере (зависший
	// обработчик или разъехавшийся после ребаланса трекер) — lag по ней растёт, а
	// ошибок в логах нет. Ловит аварию за минуты, а не за часы роста lag'а.
	KafkaPartitionStuck = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "kafka_partition_stuck_seconds",
			Help:      "Seconds the worst partition has been unable to advance its commit watermark",
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
