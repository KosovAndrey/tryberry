// internal/telegram/init.go
//
// InitWithRetry — общий ретрай инициализации Telegram-клиента (getMe ходит
// наружу через HTTPS_PROXY, канал нестабилен — старт не должен падать в петлю
// рестартов контейнера). Раньше жил идентичными копиями в cmd/api и
// cmd/bot-worker; вынесен сюда, чтобы метрика telegram_connected ставилась
// в одном месте.
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

// InitWithRetry повторяет build с backoff'ом до успеха или отмены ctx.
// Пока не получилось — telegram_connected=0 (алерт TelegramInitFailing ловит
// битый токен/мёртвый egress: см. инцидент 2026-07-08, 2 часа Unauthorized
// без единого алерта).
func InitWithRetry[T any](ctx context.Context, log *slog.Logger, build func() (T, error)) (T, error) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		v, err := build()
		if err == nil {
			metrics.TelegramConnected.Set(1)
			if attempt > 1 {
				log.Info("telegram init ok after retries", "attempts", attempt)
			}
			return v, nil
		}
		metrics.TelegramConnected.Set(0)
		log.Warn("telegram init failed (getMe), retrying",
			"attempt", attempt, "backoff", backoff.String(), "err", ScrubToken(err))
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
