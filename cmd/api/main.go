package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
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

	"gitlab.com/KosovAndrey/tryberrybot/internal/config"
	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/health"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/max"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment/robokassa"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment/yookassa"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
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

// assetVersion — короткий хеш коммита, вшивается линкером (-X main.assetVersion)
// при сборке (см. Dockerfile/compose). Идёт в ?v= к css/js страницы графика —
// сброс кэша ассетов при деплое. "dev" — локальная сборка без ldflags.
var assetVersion = "dev"

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

	databaseURL := config.MustEnv("DATABASE_URL")
	redisURL := config.MustEnv("REDIS_URL")
	botToken := config.MustEnv("TELEGRAM_BOT_TOKEN")
	kafkaBrokers := strings.Split(config.MustEnv("KAFKA_BROKERS"), ",")
	webhookURL := getEnv("TELEGRAM_WEBHOOK_URL", "")
	webhookSecret := getEnv("TELEGRAM_WEBHOOK_SECRET", "")
	webhookEnabled := getEnv("WEBHOOK_ENABLED", "true") == "true"
	port := getEnv("PORT", "8081")
	otlpEndpoint := getEnv("OTLP_ENDPOINT", "jaeger:4317")
	publicBaseURL := getEnv("PUBLIC_BASE_URL", "https://tryberry.ru")

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
	receiver, err := telegram.InitWithRetry(ctx, log, func() (*telegram.Receiver, error) {
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
	//
	// Ошибку ОТДАЁМ наверх, а не просто логируем: и в polling-, и в webhook-режиме
	// от неё зависит, будет ли апдейт доставлен повторно (оффсет getUpdates /
	// код ответа Telegram). Проглоченная ошибка = молча потерянное сообщение
	// пользователя при недоступной Kafka — как у VK и MAX ниже, отвечаем честно.
	publish := func(ctx context.Context, update tgbotapi.Update) error {
		key := ""
		if u := update.SentFrom(); u != nil {
			key = strconv.FormatInt(u.ID, 10)
		}
		if err := producer.Send(ctx, key, update); err != nil {
			log.Error("publish update", "err", err)
			return err
		}
		return nil
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
		if err := publish(r.Context(), update); err != nil {
			// 500 → Telegram повторит доставку апдейта (как у VK и MAX ниже).
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
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

	// ── MAX webhook ──────────────────────────────────────────────────────────
	// Монтируется при заданных MAX_BOT_TOKEN + MAX_CALLBACK_SECRET. Поток:
	// MAX POST /max/callback → проверка заголовка X-Max-Bot-Api-Secret → Kafka
	// max-updates (ключ = user_id) → max-консьюмер в bot-worker. На старте
	// подписываем webhook (MAX_WEBHOOK_URL), идемпотентно.
	maxToken := getEnv("MAX_BOT_TOKEN", "")
	maxSecret := getEnv("MAX_CALLBACK_SECRET", "")
	if maxToken != "" && maxSecret != "" {
		maxProducer := kafka.NewProducer(kafkaBrokers, "max-updates")
		defer maxProducer.Close()

		if webhookURL := getEnv("MAX_WEBHOOK_URL", ""); webhookURL != "" {
			if mc, err := max.NewClient(maxToken); err != nil {
				log.Warn("max: init client for subscribe", "err", err)
			} else {
				subCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := mc.EnsureWebhook(subCtx, webhookURL, maxSecret); err != nil {
					log.Warn("max: subscribe webhook", "url", webhookURL, "err", err)
				} else {
					log.Info("max webhook subscribed", "url", webhookURL)
				}
				cancel()
			}
		}

		maxHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Max-Bot-Api-Secret")), []byte(maxSecret)) != 1 {
				log.Warn("max callback secret mismatch", "remote", r.RemoteAddr)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Ключ партиционирования — автор апдейта (порядок диалога сохраняется).
			key := ""
			if uid := max.UserIDFromRaw(body); uid != 0 {
				key = strconv.FormatInt(uid, 10)
			}
			if err := maxProducer.Send(r.Context(), key, json.RawMessage(body)); err != nil {
				log.Error("publish max update", "err", err)
				w.WriteHeader(http.StatusInternalServerError) // MAX повторит доставку
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		mux.Handle("/max/callback", metrics.HTTPMiddleware("max_callback")(
			otelhttp.NewHandler(maxHandler, "max_callback"),
		))
		log.Info("max callback endpoint enabled")
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
			paymentID, _ := strconv.ParseInt(pmt.Metadata["payment_id"], 10, 64)
			if paymentID == 0 {
				log.Error("yookassa: empty payment_id in metadata", "yk_id", pmt.ID)
				w.WriteHeader(http.StatusOK) // не наш платёж / кривые метаданные
				return
			}
			ev := payment.ConfirmedEvent{PaymentID: paymentID, UserID: userID}
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

	// ── Вебхук Робокассы (ResultURL) ─────────────────────────────────────────
	// Монтируется при заданных ROBOKASSA_MERCHANT_LOGIN + PASSWORD2. В отличие от
	// ЮKassa, ResultURL ПОДПИСАН (Password2) — подпись и есть доверенный источник,
	// перечитывать статус не нужно. InvId == наш payments.id. На валидную подпись
	// → публикуем в Kafka topic payments; в ответ отдаём "OK{InvId}".
	rkLogin := getEnv("ROBOKASSA_MERCHANT_LOGIN", "")
	rkPw2 := getEnv("ROBOKASSA_PASSWORD2", "")
	if rkLogin != "" && rkPw2 != "" {
		rkClient := robokassa.NewClient(robokassa.Config{
			Login: rkLogin, Password2: rkPw2, HashType: getEnv("ROBOKASSA_HASH_TYPE", ""),
		})
		paymentRepo := postgres.NewPaymentRepo(pool)
		rkProducer := kafka.NewProducer(kafkaBrokers, payment.TopicConfirmed)
		defer rkProducer.Close()

		rkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Робокасса шлёт ResultURL POST'ом (form) или GET'ом — ParseForm покрывает оба.
			if err := r.ParseForm(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			outSum := r.FormValue("OutSum")
			invIDStr := r.FormValue("InvId")
			sig := r.FormValue("SignatureValue")
			if !rkClient.VerifyResult(outSum, invIDStr, sig) {
				log.Warn("robokassa: bad signature", "remote", r.RemoteAddr, "inv_id", invIDStr)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			paymentID, err := strconv.ParseInt(invIDStr, 10, 64)
			if err != nil || paymentID <= 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			userID, amountKopecks, found, err := paymentRepo.ConfirmInfo(r.Context(), paymentID)
			if err != nil {
				log.Error("robokassa: confirm info", "inv_id", paymentID, "err", err)
				w.WriteHeader(http.StatusInternalServerError) // пусть Робокасса повторит
				return
			}
			if !found {
				log.Warn("robokassa: unknown payment", "inv_id", paymentID, "remote", r.RemoteAddr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Доп. защита (подпись уже покрывает OutSum): сумма должна совпасть.
			if rubToKopecks(outSum) != amountKopecks {
				log.Warn("robokassa: amount mismatch", "inv_id", paymentID, "out_sum", outSum, "want_kopecks", amountKopecks)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			ev := payment.ConfirmedEvent{PaymentID: paymentID, UserID: userID}
			if err := rkProducer.Send(r.Context(), strconv.FormatInt(userID, 10), ev); err != nil {
				log.Error("robokassa: publish confirmed", "inv_id", paymentID, "err", err)
				w.WriteHeader(http.StatusInternalServerError) // ретрай Робокассы
				return
			}
			fmt.Fprintf(w, "OK%d", paymentID) // Робокасса требует "OK{InvId}"
		})
		mux.Handle("/robokassa/result", metrics.HTTPMiddleware("robokassa_result")(
			otelhttp.NewHandler(rkHandler, "robokassa_result"),
		))
		log.Info("robokassa result endpoint enabled")
	}

	mux.HandleFunc("/health", healthChecker.Handler())
	mux.HandleFunc("/live", health.LivenessHandler())
	mux.Handle("/metrics", promhttp.Handler())

	// Публичные read-only страницы графиков цены (/p/<public_id>) + их JSON-API,
	// sitemap и robots. Reuse пула: api уже держит подключение к Postgres.
	webHandlers, err := NewWebHandlers(
		postgres.NewProductRepo(pool),
		postgres.NewPriceHistoryRepo(pool),
		publicBaseURL,
		assetVersion,
		log,
	)
	if err != nil {
		return fmt.Errorf("init web handlers: %w", err)
	}
	webHandlers.Register(mux)

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

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// rubToKopecks — "189.00" → 18900. -1 при неразборе (не совпадёт ни с одной суммой).
func rubToKopecks(s string) int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return -1
	}
	return int64(math.Round(f * 100))
}
