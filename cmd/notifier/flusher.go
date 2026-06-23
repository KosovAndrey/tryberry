package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"golang.org/x/time/rate"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
)

// alertDeliverer — доставка одного алерта (богато, с фото) или пачки одним
// сообщением (гибрид). Удовлетворяется *deliverer.
type alertDeliverer interface {
	SendPriceAlert(ctx context.Context, a telegram.PriceAlert) error
	SendBundledAlert(ctx context.Context, a telegram.BundledAlert) error
}

// pendingAlertStore — outbox, который читает/обновляет флашер.
type pendingAlertStore interface {
	FetchDue(ctx context.Context, limit, maxAttempts int) ([]domain.PendingAlert, error)
	MarkSent(ctx context.Context, id int64) error
	MarkSentBatch(ctx context.Context, ids []int64) error
	MarkFailedBatch(ctx context.Context, ids []int64, errMsg string, nextAfter time.Time) error
	CountUnsent(ctx context.Context) (int, error)
	DeleteSentBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// flusherConfig — параметры доставки. Дефолты подобраны под лимит Telegram
// ~30 сообщений/с на бота (берём с запасом).
type flusherConfig struct {
	rps             float64       // глобальный rate-limit отправок в секунду
	burst           int           // burst токенов лимитера
	batch           int           // строк за один тик
	maxAttempts     int           // потолок ретраев одной строки
	tickInterval    time.Duration // как часто разгребаем очередь
	cleanupInterval time.Duration // как часто чистим доставленные
	retention       time.Duration // сколько держим доставленные строки
}

func defaultFlusherConfig() flusherConfig {
	return flusherConfig{
		rps:             25,
		burst:           25,
		batch:           500,
		maxAttempts:     10,
		tickInterval:    time.Second,
		cleanupInterval: time.Hour,
		retention:       7 * 24 * time.Hour,
	}
}

// flusherBackoff — пауза перед следующей попыткой по числу уже сделанных:
// экспонента от 5с, потолок 30 мин. attempts здесь — значение ДО инкремента
// (0 на первой неудаче).
func flusherBackoff(attempts int) time.Duration {
	const (
		base = 5 * time.Second
		cap  = 30 * time.Minute
	)
	d := base
	for i := 0; i < attempts && d < cap; i++ {
		d *= 2
	}
	if d > cap {
		d = cap
	}
	return d
}

// runFlusher — singleton-доставщик outbox. Расцепляет горячий путь консьюмера
// price-events от медленной Telegram-отправки: консьюмер пишет в pending_alerts,
// флашер шлёт с глобальным rate-limit и ретраями. Блокируется до отмены ctx.
//
// Бандлинг (Phase 1b): due-строки группируются по юзеру; 1 алерт → богатый
// SendPriceAlert (фото), ≥2 → одно бандл-сообщение SendBundledAlert. Окно
// коалесинга задаёт консьюмер через deliver_after (= now + Plan.BundleWindow).
// Лимитер тратит 1 токен на юзера (= 1 сообщение). См.
// docs/SCALING-NOTIFIER-DELIVERY.md.
func runFlusher(ctx context.Context, log *slog.Logger, store pendingAlertStore, sender alertDeliverer, cfg flusherConfig) {
	limiter := rate.NewLimiter(rate.Limit(cfg.rps), cfg.burst)

	tick := time.NewTicker(cfg.tickInterval)
	defer tick.Stop()
	cleanup := time.NewTicker(cfg.cleanupInterval)
	defer cleanup.Stop()

	log.Info("delivery flusher started", "rps", cfg.rps, "batch", cfg.batch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-cleanup.C:
			if n, err := store.DeleteSentBefore(ctx, time.Now().Add(-cfg.retention)); err != nil {
				log.Warn("flusher cleanup", "err", err)
			} else if n > 0 {
				log.Info("flusher cleanup", "deleted", n)
			}
		case <-tick.C:
			if err := flushDue(ctx, log, store, sender, limiter, cfg.batch, cfg.maxAttempts); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warn("flusher pass", "err", err)
			}
		}
	}
}

// flushDue — один проход разгребания очереди: обновляет метрику глубины, берёт
// созревшие строки, группирует по юзеру и доставляет (1 токен лимитера на
// юзера). Вынесено из runFlusher ради тестируемости. Возвращает ошибку fetch
// или отмену ctx (через limiter.Wait).
func flushDue(ctx context.Context, log *slog.Logger, store pendingAlertStore, sender alertDeliverer, limiter *rate.Limiter, batch, maxAttempts int) error {
	if n, err := store.CountUnsent(ctx); err == nil {
		metrics.PendingAlertsDepth.Set(float64(n))
	}
	rows, err := store.FetchDue(ctx, batch, maxAttempts)
	if err != nil {
		return err
	}
	groups, order := groupByUser(rows)
	for _, uid := range order {
		// Глобальный лимит: один токен на юзера (= одно сообщение).
		if err := limiter.Wait(ctx); err != nil {
			return err
		}
		deliverGroup(ctx, log, store, sender, groups[uid], maxAttempts)
	}
	return nil
}

// groupByUser группирует строки по user_id, сохраняя порядок первого появления
// (он же порядок созревания — старейшие сперва).
func groupByUser(rows []domain.PendingAlert) (map[int64][]domain.PendingAlert, []int64) {
	groups := make(map[int64][]domain.PendingAlert)
	var order []int64
	for _, r := range rows {
		if _, ok := groups[r.UserID]; !ok {
			order = append(order, r.UserID)
		}
		groups[r.UserID] = append(groups[r.UserID], r)
	}
	return groups, order
}

// deliverGroup доставляет все созревшие алерты одного юзера: один — богато,
// несколько — пачкой. Все строки группы гасятся/ретраятся вместе.
func deliverGroup(ctx context.Context, log *slog.Logger, store pendingAlertStore, sender alertDeliverer, rows []domain.PendingAlert, maxAttempts int) {
	alerts := make([]telegram.PriceAlert, 0, len(rows))
	ids := make([]int64, 0, len(rows))
	maxAtt := 0
	for _, r := range rows {
		var pa telegram.PriceAlert
		if err := json.Unmarshal(r.Payload, &pa); err != nil {
			// Битый payload — не доставить никогда. Гасим отдельно, чтобы не крутить.
			log.Error("flusher: bad payload", "id", r.ID, "err", err)
			_ = store.MarkSent(ctx, r.ID)
			metrics.PendingAlertsDelivered.WithLabelValues("failed").Inc()
			continue
		}
		alerts = append(alerts, pa)
		ids = append(ids, r.ID)
		if r.Attempts > maxAtt {
			maxAtt = r.Attempts
		}
	}
	if len(alerts) == 0 {
		return
	}

	var sendErr error
	if len(alerts) == 1 {
		sendErr = sender.SendPriceAlert(ctx, alerts[0])
	} else {
		sendErr = sender.SendBundledAlert(ctx, buildBundle(alerts))
	}

	if sendErr != nil {
		next := time.Now().Add(flusherBackoff(maxAtt))
		if err := store.MarkFailedBatch(ctx, ids, sendErr.Error(), next); err != nil {
			log.Warn("flusher: mark failed batch", "err", err)
		}
		metrics.PendingAlertsDelivered.WithLabelValues("failed").Add(float64(len(ids)))
		if maxAtt+1 >= maxAttempts {
			log.Error("flusher: giving up on alert group",
				"user_id", rows[0].UserID, "count", len(ids), "attempts", maxAtt+1, "err", sendErr)
		}
		return
	}

	if err := store.MarkSentBatch(ctx, ids); err != nil {
		// Доставили, но не пометили: ретрай задвоит сообщение. БД-сбой здесь редок,
		// принимаем риск (at-least-once).
		log.Warn("flusher: mark sent batch (possible duplicate on retry)", "err", err)
	}
	metrics.PendingAlertsDelivered.WithLabelValues("sent").Add(float64(len(ids)))
}

func buildBundle(alerts []telegram.PriceAlert) telegram.BundledAlert {
	items := make([]telegram.BundledAlertItem, len(alerts))
	for i, a := range alerts {
		items[i] = telegram.BundledAlertItem{
			ProductName: a.ProductName,
			ProductURL:  a.ProductURL,
			OldPrice:    a.OldPrice,
			NewPrice:    a.NewPrice,
			BackInStock: a.BackInStock,
		}
	}
	return telegram.BundledAlert{ChatID: alerts[0].ChatID, UserID: alerts[0].UserID, Items: items}
}
