// internal/telegram/polling.go
//
// Long-polling режим бота (getUpdates) как альтернатива webhook.
//
// Зачем: на RU-хостинге Роскомнадзор режет НЕ только исходящие к Telegram, но и
// входящие соединения Telegram→RU-IP — то есть доставка webhook'ов до нашего
// nginx таймаутит. Long-polling переворачивает направление: бот САМ ходит наружу
// за апдейтами, а исходящий трафик идёт через HTTPS_PROXY (не-RU exit).
package telegram

import (
	"context"
	"os"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// DeleteWebhook снимает зарегистрированный webhook.
// getUpdates НЕ работает, пока webhook установлен — Telegram вернёт 409 Conflict.
func (b *Bot) DeleteWebhook() error {
	_, err := b.api.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: false})
	return err
}

// RunPolling запускает приём апдейтов через long-polling. Блокируется до ctx.
//
// ВАЖНО: getUpdates — синглтон. Только ОДИН процесс может его звать; иначе
// Telegram раздаст апдейты между ними и часть команд потеряется. api у нас
// single-instance.
func (b *Bot) RunPolling(ctx context.Context) error {
	if err := b.DeleteWebhook(); err != nil {
		b.log.Warn("delete webhook before polling (continuing)", "err", err)
	}

	timeout := pollTimeoutSeconds()
	u := tgbotapi.NewUpdate(0)
	u.Timeout = timeout // long-poll держит соединение до timeout секунд
	updates := b.api.GetUpdatesChan(u)
	b.log.Info("polling started (getUpdates)", "timeout_s", timeout)

	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			b.log.Info("polling stopped")
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			b.HandleUpdate(ctx, update)
		}
	}
}

// pollTimeoutSeconds — длительность long-poll в секундах.
// Короткий таймаут спасает от разрыва удерживаемого соединения резидентным
// прокси (симптом — "unexpected EOF" на холостых поллах). По умолчанию 10с;
// переопределяется env TELEGRAM_POLL_TIMEOUT_SECONDS без пересборки.
func pollTimeoutSeconds() int {
	if v := os.Getenv("TELEGRAM_POLL_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 10
}
