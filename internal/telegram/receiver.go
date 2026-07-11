// internal/telegram/receiver.go
//
// Receiver — тонкий приёмник апдейтов для сервиса-ingestor (бывший монолитный
// приём в api). Делает только getMe + webhook/polling и отдаёт каждый апдейт
// колбэку (ingestor публикует его в Kafka topic telegram-updates). Обработка
// (HandleUpdate) живёт в bot-worker, поэтому Receiver НЕ держит репозитории,
// registry и FSM — он максимально лёгкий и реплицировать его не нужно.
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

type Receiver struct {
	api *tgbotapi.BotAPI
	log *slog.Logger
}

// NewReceiver делает getMe (ходит наружу к Telegram). Оборачивать ретраями —
// на стороне вызывающего (ingestor), как и для NewBot.
func NewReceiver(token string, log *slog.Logger) (*Receiver, error) {
	api, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, pollHTTPClient())
	if err != nil {
		return nil, fmt.Errorf("init bot api: %w", err)
	}
	return &Receiver{api: api, log: log}, nil
}

// pollHTTPClient — HTTP-клиент для long-poll getUpdates через HTTPS_PROXY
// (xray/VLESS). КРИТИЧНО иметь таймаут: дефолтный tgbotapi.NewBotAPI
// создаёт http.Client БЕЗ него, и тогда запрос по «полумёртвому» keep-alive
// соединению к прокси висит до idle-таймаута прокси (минуты). Цикл
// GetUpdatesChan однопоточный — пока запрос висит, апдейты копятся на стороне
// Telegram и бот отвечает с многоминутной задержкой.
//
// Таймаут ДОЛЖЕН превышать серверный long-poll timeout, иначе режет здоровые
// поллы: берём poll timeout + запас.
func pollHTTPClient() *http.Client {
	// DisableKeepAlives: каждый getUpdates/Send открывает СВЕЖИЙ CONNECT-туннель
	// через прокси (xray). Переиспользование keep-alive соединения в цикле
	// long-poll'а ловит "unexpected EOF"/"context deadline exceeded" — прокси или
	// туннель роняет удержанное соединение, а пул Go подсовывает его снова.
	// Разовые запросы (wget, notifier) работают именно потому, что коннект свежий.
	// Proxy берём из окружения (HTTPS_PROXY=http://xray:8888).
	return &http.Client{
		// Узкий запас над серверным long-poll timeout. На нестабильном egress'е к
		// Telegram удержанный long-poll иногда тихо умирает — ответ не придёт, и
		// запрос висит ровно Client.Timeout. Большой запас (+15) превращал каждый
		// такой висяк в 20с просадку; +3 рвёт мёртвый полл быстро (здоровый отдаёт
		// на poll-й секунде, доставка ~0.3с), бот переполливает и забирает апдейт.
		Timeout: time.Duration(pollTimeoutSeconds()+3) * time.Second,
		Transport: &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			DisableKeepAlives: true,
			// Зависший TLS-хендшейк к origin падает быстро, а не висит до Client.Timeout.
			TLSHandshakeTimeout: 5 * time.Second,
		},
	}
}

// SetWebhook регистрирует webhook и (если secretToken не пуст) просит Telegram
// слать его в заголовке X-Telegram-Bot-Api-Secret-Token каждого апдейта. Это
// единственная аутентификация эндпоинта /webhook: он публичен, и без секрета
// кто угодно может запостить поддельный апдейт (спуфинг from.id → выдача себе
// прав/тарифа, утечка чужих данных). WebhookConfig в tgbotapi v5.5.1 не несёт
// поля secret_token, поэтому шлём setWebhook напрямую через MakeRequest.
func (r *Receiver) SetWebhook(webhookURL, secretToken string) error {
	params := tgbotapi.Params{"url": webhookURL}
	if secretToken != "" {
		params["secret_token"] = secretToken
	}
	_, err := r.api.MakeRequest("setWebhook", params)
	return err
}

func (r *Receiver) DeleteWebhook() error {
	_, err := r.api.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: false})
	return err
}

// SetCommands регистрирует меню команд (одноразовая bot-level настройка — место
// для неё в синглтоне-ingestor, а не в каждом bot-worker).
func (r *Receiver) SetCommands() error {
	commands := []tgbotapi.BotCommand{
		{Command: "start", Description: "Главное меню"},
		{Command: "menu", Description: "Открыть меню"},
		{Command: "track", Description: "Добавить товар — /track <ссылка>"},
		{Command: "list", Description: "Мои подписки"},
		{Command: "track_search", Description: "Отслеживать поиск — /track_search <ссылка>"},
		{Command: "list_search", Description: "Мои поиск-подписки"},
		{Command: "trial", Description: "🎁 Триал поиска (10 дней)"},
		{Command: "myplan", Description: "Мой тариф и лимиты"},
		{Command: "help", Description: "Помощь"},
	}
	_, err := r.api.Request(tgbotapi.NewSetMyCommands(commands...))
	return err
}

// RunPolling — long-poll; на каждый апдейт зовёт fn (ingestor → продюс в Kafka).
// Блокируется до отмены ctx. getUpdates — синглтон, поэтому ingestor должен быть
// single-instance (в отличие от реплицируемых bot-worker).
//
// Цикл явный, а не GetUpdatesChan: tgbotapi глотает ошибки getUpdates внутри
// (лог + retry), и отозванный на лету токен / умерший egress снаружи не видны —
// бот просто молчит (инцидент 2026-07-08). Здесь каждый успешный полл двигает
// telegram_poll_last_success_timestamp_seconds, ошибки считает
// telegram_poll_errors_total; алерт TelegramPollingStale ловит застывший цикл.
// Long-poll возвращается каждые ~timeout секунд и без апдейтов, так что метрика
// живая независимо от трафика.
func (r *Receiver) RunPolling(ctx context.Context, fn func(context.Context, tgbotapi.Update)) error {
	if err := r.DeleteWebhook(); err != nil {
		r.log.Warn("delete webhook before polling (continuing)", "err", err)
	}

	timeout := pollTimeoutSeconds() // из polling.go, env TELEGRAM_POLL_TIMEOUT_SECONDS
	r.log.Info("polling started (getUpdates)", "timeout_s", timeout)

	// Тот же ретрай-интервал, что у GetUpdatesChan внутри tgbotapi.
	const errorBackoff = 3 * time.Second
	offset := 0
	for {
		if ctx.Err() != nil {
			r.log.Info("polling stopped")
			return nil
		}
		u := tgbotapi.NewUpdate(offset)
		u.Timeout = timeout
		updates, err := r.api.GetUpdates(u)
		if err != nil {
			metrics.TelegramPollErrors.Inc()
			r.log.Warn("getUpdates failed, retrying", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(errorBackoff):
			}
			continue
		}
		metrics.TelegramPollLastSuccess.SetToCurrentTime()
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			fn(ctx, update)
		}
	}
}
