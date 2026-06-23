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

// priceAlertSender — то, что умеет доставить уведомление (роутинг по каналам
// внутри). Удовлетворяется *deliverer.
type priceAlertSender interface {
	SendPriceAlert(ctx context.Context, a telegram.PriceAlert) error
}

// pendingAlertStore — outbox, который читает/обновляет флашер.
type pendingAlertStore interface {
	FetchDue(ctx context.Context, limit, maxAttempts int) ([]domain.PendingAlert, error)
	MarkSent(ctx context.Context, id int64) error
	MarkFailed(ctx context.Context, id int64, errMsg string, nextAfter time.Time) error
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
		batch:           200,
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
// Phase 1a: по одному сообщению на строку (поведение как раньше). Бандлинг
// (группировка по юзеру) — Phase 1b, см. docs/SCALING-NOTIFIER-DELIVERY.md.
func runFlusher(ctx context.Context, log *slog.Logger, store pendingAlertStore, sender priceAlertSender, cfg flusherConfig) {
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
			if n, err := store.CountUnsent(ctx); err == nil {
				metrics.PendingAlertsDepth.Set(float64(n))
			}
			rows, err := store.FetchDue(ctx, cfg.batch, cfg.maxAttempts)
			if err != nil {
				log.Warn("flusher fetch", "err", err)
				continue
			}
			for _, row := range rows {
				// Глобальный лимит: ждём токен (или выходим по отмене ctx).
				if err := limiter.Wait(ctx); err != nil {
					return
				}
				deliverOne(ctx, log, store, sender, row, cfg.maxAttempts)
			}
		}
	}
}

func deliverOne(ctx context.Context, log *slog.Logger, store pendingAlertStore, sender priceAlertSender, row domain.PendingAlert, maxAttempts int) {
	var pa telegram.PriceAlert
	if err := json.Unmarshal(row.Payload, &pa); err != nil {
		// Битый payload — не доставить никогда. Гасим, чтобы не крутить.
		log.Error("flusher: bad payload", "id", row.ID, "err", err)
		_ = store.MarkSent(ctx, row.ID)
		metrics.PendingAlertsDelivered.WithLabelValues("failed").Inc()
		return
	}

	if err := sender.SendPriceAlert(ctx, pa); err != nil {
		next := time.Now().Add(flusherBackoff(row.Attempts))
		if mErr := store.MarkFailed(ctx, row.ID, err.Error(), next); mErr != nil {
			log.Warn("flusher: mark failed", "id", row.ID, "err", mErr)
		}
		metrics.PendingAlertsDelivered.WithLabelValues("failed").Inc()
		if row.Attempts+1 >= maxAttempts {
			log.Error("flusher: giving up on alert", "id", row.ID, "sub_id", row.SubscriptionID, "attempts", row.Attempts+1, "err", err)
		}
		return
	}

	if err := store.MarkSent(ctx, row.ID); err != nil {
		// Доставили, но не пометили: ретрай задвоит сообщение. Логируем как warn —
		// строка не созреет повторно до бэкоффа? Нет — MarkSent упал, она снова due.
		// Принимаем риск редкого дубля (at-least-once); БД-сбой здесь крайне редок.
		log.Warn("flusher: mark sent failed (possible duplicate on retry)", "id", row.ID, "err", err)
	}
	metrics.PendingAlertsDelivered.WithLabelValues("sent").Inc()
}
