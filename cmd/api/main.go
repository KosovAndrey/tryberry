package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment/yookassa"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
	"gitlab.com/KosovAndrey/tryberrybot/internal/tracing"
)

// api в роли INGESTOR (Шаг 2a): принимает апдейты (polling/webhook за флагом
// WEBHOOK_ENABLED) и публикует сырой Update в Kafka topic telegram-updates с
// ключом = user ID (порядок диалога одного юзера сохраняется в партиции →
// один bot-worker обрабатывает их последовательно). Обработка и ответы переехали
// в сервис bot-worker.
//
// Подключение к БД сохранено НАМЕРЕННО — ради бизнес-метрик (runMetricsUpdater)
// и health, которые Prometheus уже скрейпит с api:/metrics. Репозитории/registry
// и FSM здесь больше не нужны.

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	if err := run(log); err != nil {
		log.Error("api failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	_ = godotenv.Load()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	databaseURL := mustEnv("DATABASE_URL")
	redisURL := mustEnv("REDIS_URL")
	botToken := mustEnv("TELEGRAM_BOT_TOKEN")
	kafkaBrokers := strings.Split(mustEnv("KAFKA_BROKERS"), ",")
	webhookURL := getEnv("TELEGRAM_WEBHOOK_URL", "")
	webhookSecret := getEnv("TELEGRAM_WEBHOOK_SECRET", "")
	webhookEnabled := getEnv("WEBHOOK_ENABLED", "true") == "true"
	port := getEnv("PORT", "8081")
	otlpEndpoint := getEnv("OTLP_ENDPOINT", "jaeger:4317")

	shutdownTracing, err := tracing.Init(ctx, "api", otlpEndpoint)
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
		log.Warn("redis unavailable", "err", err)
		redisClient = nil
	}

	// getMe ходит наружу к Telegram (через HTTPS_PROXY). Канал флапает — ретраим
	// с backoff'ом, чтобы старт не падал в петлю.
	receiver, err := initWithRetry(ctx, log, func() (*telegram.Receiver, error) {
		return telegram.NewReceiver(botToken, log)
	})
	if err != nil {
		return fmt.Errorf("init receiver: %w", err)
	}
	if err := receiver.SetCommands(); err != nil {
		log.Warn("set commands failed", "err", err)
	}

	producer := kafka.NewProducer(kafkaBrokers, "telegram-updates")
	defer producer.Close()

	// publish — публикует апдейт в Kafka. Ключ = user ID, чтобы апдейты одного
	// пользователя шли в одну партицию (порядок диалога сохраняется).
	publish := func(ctx context.Context, update tgbotapi.Update) {
		key := ""
		if u := update.SentFrom(); u != nil {
			key = strconv.FormatInt(u.ID, 10)
		}
		if err := producer.Send(ctx, key, update); err != nil {
			log.Error("publish update", "err", err)
		}
	}

	if webhookEnabled {
		if webhookURL == "" {
			return fmt.Errorf("WEBHOOK_ENABLED=true requires TELEGRAM_WEBHOOK_URL")
		}
		// Без секрета /webhook аутентифицировать нечем — публичный эндпоинт принимал
		// бы поддельные апдейты. Падаем на старте, а не запускаемся «открытыми».
		if webhookSecret == "" {
			return fmt.Errorf("WEBHOOK_ENABLED=true requires TELEGRAM_WEBHOOK_SECRET")
		}
		if err := receiver.SetWebhook(webhookURL, webhookSecret); err != nil {
			return fmt.Errorf("set webhook: %w", err)
		}
		log.Info("webhook set", "url", webhookURL)
	} else {
		go func() {
			if err := receiver.RunPolling(ctx, publish); err != nil {
				log.Error("polling stopped with error", "err", err)
			}
		}()
		log.Info("polling mode enabled (getUpdates via proxy)")
	}

	mux := http.NewServeMux()
	healthChecker := health.New(pool, redisClient)

	webhookHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Аутентификация эндпоинта: апдейт принимаем, только если Telegram прислал
		// согласованный с setWebhook секрет. Сравнение постоянного времени.
		got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(webhookSecret)) != 1 {
			log.Warn("webhook secret mismatch", "remote", r.RemoteAddr)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var update tgbotapi.Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			log.Error("decode update", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		publish(r.Context(), update)
		w.WriteHeader(http.StatusOK)
	})

	// Эндпоинт монтируем ТОЛЬКО в webhook-режиме. В polling-режиме /webhook не
	// существует (404) — иначе он висел бы открытым приёмником поддельных апдейтов.
	if webhookEnabled {
		mux.Handle("/webhook", metrics.HTTPMiddleware("webhook")(
			otelhttp.NewHandler(webhookHandler, "webhook"),
		))
	}
	// ── VK Callback API (фаза 1 VK-интеграции) ───────────────────────────────
	// Монтируется только при заданных VK_CONFIRMATION и VK_CALLBACK_SECRET.
	// Поток: VK POST /vk/callback → проверка secret → Kafka topic vk-updates
	// (ключ = from_id, порядок диалога юзера сохраняется) → vk-консьюмер в bot-worker.
	vkConfirmation := getEnv("VK_CONFIRMATION", "")
	vkSecret := getEnv("VK_CALLBACK_SECRET", "")
	if vkConfirmation != "" && vkSecret != "" {
		vkProducer := kafka.NewProducer(kafkaBrokers, "vk-updates")
		defer vkProducer.Close()

		vkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var ev struct {
				Type    string          `json:"type"`
				GroupID int64           `json:"group_id"`
				Secret  string          `json:"secret"`
				Object  json.RawMessage `json:"object"`
			}
			if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if subtle.ConstantTimeCompare([]byte(ev.Secret), []byte(vkSecret)) != 1 {
				log.Warn("vk callback secret mismatch", "remote", r.RemoteAddr)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			// Подтверждение сервера: VK шлёт type=confirmation, ждёт строку в теле.
			if ev.Type == "confirmation" {
				fmt.Fprint(w, vkConfirmation)
				return
			}
			// Ключ партиционирования — автор сообщения (как у telegram-updates).
			key := ""
			var obj struct {
				Message struct {
					FromID int64 `json:"from_id"`
				} `json:"message"`
			}
			if json.Unmarshal(ev.Object, &obj) == nil && obj.Message.FromID != 0 {
				key = strconv.FormatInt(obj.Message.FromID, 10)
			}
			if err := vkProducer.Send(r.Context(), key, ev); err != nil {
				log.Error("publish vk update", "err", err)
				// 500 → VK ретраит доставку, событие не потеряется.
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, "ok") // VK требует literal "ok" в теле
		})
		mux.Handle("/vk/callback", metrics.HTTPMiddleware("vk_callback")(
			otelhttp.NewHandler(vkHandler, "vk_callback"),
		))
		log.Info("vk callback endpoint enabled")
	}

	// ── Вебхук ЮKassa ────────────────────────────────────────────────────────
	// Монтируется при заданных YOOKASSA_SHOP_ID + YOOKASSA_SECRET_KEY. ЮKassa
	// вебхуки НЕ подписывает: тело не доверяем, статус перечитываем по id через
	// API. На succeeded → публикуем в Kafka topic payments (ключ = user_id),
	// bot-worker применяет (продление плана + уведомление).
	ykShopID := getEnv("YOOKASSA_SHOP_ID", "")
	ykSecret := getEnv("YOOKASSA_SECRET_KEY", "")
	if ykShopID != "" && ykSecret != "" {
		ykClient := yookassa.NewClient(ykShopID, ykSecret)
		payProducer := kafka.NewProducer(kafkaBrokers, payment.TopicConfirmed)
		defer payProducer.Close()

		ykHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Event  string `json:"event"`
				Object struct {
					ID string `json:"id"`
				} `json:"object"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Object.ID == "" {
				// Кривое тело игнорируем (200 — ЮKassa не должна ретраить мусор).
				w.WriteHeader(http.StatusOK)
				return
			}

			// Авторитетный статус — перечтением по id (тело вебхука не подписано).
			pmt, err := ykClient.GetPayment(r.Context(), body.Object.ID)
			if err != nil {
				log.Error("yookassa: refetch payment", "id", body.Object.ID, "err", err)
				w.WriteHeader(http.StatusInternalServerError) // пусть ЮKassa повторит
				return
			}
			if pmt.Status != yookassa.StatusSucceeded {
				w.WriteHeader(http.StatusOK) // pending/canceled — не применяем
				return
			}

			userKey := pmt.Metadata["user_id"]
			userID, _ := strconv.ParseInt(userKey, 10, 64)
			ev := payment.ConfirmedEvent{YKPaymentID: pmt.ID, UserID: userID}
			if err := payProducer.Send(r.Context(), userKey, ev); err != nil {
				log.Error("yookassa: publish confirmed", "id", pmt.ID, "err", err)
				w.WriteHeader(http.StatusInternalServerError) // ретрай ЮKassa
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		mux.Handle("/yookassa/webhook", metrics.HTTPMiddleware("yookassa_webhook")(
			otelhttp.NewHandler(ykHandler, "yookassa_webhook"),
		))
		log.Info("yookassa webhook endpoint enabled")
	}

	mux.HandleFunc("/health", healthChecker.Handler())
	mux.HandleFunc("/live", health.LivenessHandler())
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{Addr: ":" + port, Handler: mux}

	log.Info("ingestor started", "port", port)

	go runMetricsUpdater(ctx, log, pool)

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// initWithRetry повторяет инициализацию с backoff'ом до успеха или отмены ctx
// (getMe на старте ходит к Telegram, канал нестабилен).
func initWithRetry[T any](ctx context.Context, log *slog.Logger, build func() (T, error)) (T, error) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		v, err := build()
		if err == nil {
			if attempt > 1 {
				log.Info("telegram init ok after retries", "attempts", attempt)
			}
			return v, nil
		}
		log.Warn("telegram init failed (getMe), retrying",
			"attempt", attempt, "backoff", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			var zero T
			return zero, fmt.Errorf("cancelled after %d attempts: %w", attempt, err)
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func runMetricsUpdater(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool) {
	tick := func() {
		ctxQ, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		var total int
		if err := pool.QueryRow(ctxQ,
			`SELECT COUNT(*) FROM subscriptions WHERE active = TRUE`).Scan(&total); err == nil {
			metrics.ActiveSubscriptions.Set(float64(total))
		}

		var users int
		if err := pool.QueryRow(ctxQ,
			`SELECT COUNT(*) FROM users`).Scan(&users); err == nil {
			metrics.TotalUsers.Set(float64(users))
		}

		rows, err := pool.Query(ctxQ, `
        SELECT p.marketplace, COUNT(*)
        FROM subscriptions s
        JOIN products p ON p.id = s.product_id
        WHERE s.active = TRUE
        GROUP BY p.marketplace`)
		if err == nil {
			defer rows.Close()
			metrics.SubscriptionsByMarketplace.Reset()
			for rows.Next() {
				var mp string
				var count int
				if err := rows.Scan(&mp, &count); err == nil {
					metrics.SubscriptionsByMarketplace.WithLabelValues(mp).Set(float64(count))
				}
			}
		}

		var searchSubs int
		if err := pool.QueryRow(ctxQ,
			`SELECT COUNT(*) FROM search_subscriptions WHERE active = TRUE`).Scan(&searchSubs); err == nil {
			metrics.ActiveSearchSubscriptions.Set(float64(searchSubs))
		}

		planRows, err := pool.Query(ctxQ,
			`SELECT plan, COUNT(*) FROM users GROUP BY plan`)
		if err == nil {
			defer planRows.Close()
			metrics.UsersByPlan.Reset()
			for planRows.Next() {
				var plan string
				var count int
				if err := planRows.Scan(&plan, &count); err == nil {
					metrics.UsersByPlan.WithLabelValues(plan).Set(float64(count))
				}
			}
		}
	}

	tick()
	ticker := time.NewTicker(30 * time.Second)
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
