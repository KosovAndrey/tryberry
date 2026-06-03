// internal/telegram/polling.go
//
// Long-polling режим бота (getUpdates) как альтернатива webhook.
//
// Зачем: на RU-хостинге Роскомнадзор режет НЕ только исходящие к Telegram, но и
// входящие соединения Telegram→RU-IP — то есть доставка webhook'ов до нашего
// nginx таймаутит (getWebhookInfo показывает "Connection timed out", апдейты
// копятся в pending_update_count). Long-polling переворачивает направление: бот
// САМ ходит наружу за апдейтами, а исходящий трафик у нас уже идёт через
// HTTPS_PROXY (не-RU exit) и потому пробивается.
package telegram

import (
	"context"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// DeleteWebhook снимает зарегистрированный webhook.
// getUpdates НЕ работает, пока webhook установлен — Telegram вернёт 409 Conflict.
// Поэтому перед стартом polling'а webhook надо снять.
func (b *Bot) DeleteWebhook() error {
	_, err := b.api.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: false})
	return err
}

// RunPolling запускает приём апдейтов через long-polling (getUpdates).
// Блокируется до отмены ctx.
//
// ВАЖНО: getUpdates — синглтон. Его может вызывать только ОДИН процесс
// одновременно; если запустить polling в нескольких репликах api, Telegram
// раздаст апдейты между ними и часть команд потеряется. Сейчас api и так
// single-instance, так что это ок. Когда дойдём до реплицируемого приёма —
// этот цикл переедет в отдельный тонкий ingestor-синглтон, а обработка уйдёт
// в consumer-группу (см. план по telegram-updates).
func (b *Bot) RunPolling(ctx context.Context) error {
	// Снять webhook, иначе getUpdates вернёт 409.
	if err := b.DeleteWebhook(); err != nil {
		b.log.Warn("delete webhook before polling (continuing)", "err", err)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 10 // long-poll: держим соединение до 30с — меньше пустых запросов
	updates := b.api.GetUpdatesChan(u)
	b.log.Info("polling started (getUpdates)", "timeout_s", u.Timeout)

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
