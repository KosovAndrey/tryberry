// Package max — клиент MAX Bot API (max.ru) и обработчик входящих апдейтов.
// Полный паритет с Telegram/VK: привязка аккаунтов, трекинг товаров, поиск-
// подписки, тарифы и оплата. См. docs/MAX-INTEGRATION-PLAN.md.
//
// max.ru доступен с RU-VM напрямую — прокси, как и для VK, не нужен. Транспорт —
// официальный клиент github.com/max-messenger/max-bot-api-client-go; здесь тонкая
// обёртка, повторяющая API клавиатур VK, чтобы фичовые файлы портировались 1:1.
package max

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	maxbot "github.com/max-messenger/max-bot-api-client-go"
	"github.com/max-messenger/max-bot-api-client-go/schemes"
)

// platform-api2.max.ru обслуживается за «Russian Trusted Root CA» (Минцифры),
// которого нет в Mozilla-бандле alpine → стандартная проверка TLS падает с
// "certificate signed by unknown authority". Встраиваем корень + промежуточный
// и доверяем им ТОЛЬКО для запросов к MAX API (системный пул глобально не трогаем).
//
//go:embed russian_trusted_ca.pem
var russianTrustedCA []byte

// maxHTTPClient — http.Client с системным пулом CA + российским корнем, только
// для MAX. Транспорт — клон DefaultTransport (сохраняет прокси/таймауты диалера).
func maxHTTPClient() *http.Client {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(russianTrustedCA)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Timeout: 30 * time.Second, Transport: tr}
}

// Client — обёртка над maxbot.Api с VK-подобным API отправки.
type Client struct {
	api *maxbot.Api
}

func NewClient(token string) (*Client, error) {
	api, err := maxbot.New(token, maxbot.WithHTTPClient(maxHTTPClient()))
	if err != nil {
		return nil, err
	}
	return &Client{api: api}, nil
}

// API даёт доступ к нижележащему клиенту (нужно cmd/api для подписки на webhook).
func (c *Client) API() *maxbot.Api { return c.api }

// Keyboard / Button — зеркало vk.Keyboard: поля Inline/OneTime сохранены для
// совместимости портированного кода, но в MAX клавиатуры всегда inline.
type Keyboard struct {
	OneTime bool
	Inline  bool
	Buttons [][]Button
}

type Button struct {
	Label   string
	Payload string // callback-кнопка (наш JSON {"cmd":...})
	Link    string // open_link-кнопка
}

// Цвета сохранены для совместимости с портом из VK (в MAX игнорируются).
const (
	ColorPrimary   = "primary"
	ColorSecondary = "secondary"
)

// TextButton — callback-кнопка: нажатие шлёт боту payload в message_callback.
func TextButton(label, payload, _ string) Button {
	return Button{Label: label, Payload: payload}
}

// LinkButton — кнопка-ссылка (открывает URL, боту ничего не шлёт).
func LinkButton(label, link string) Button {
	return Button{Label: label, Link: link}
}

// SendMessage — текст в ЛС пользователю (userID = max_id).
func (c *Client) SendMessage(ctx context.Context, userID int64, text string) error {
	return c.SendMessageKeyboard(ctx, userID, text, nil)
}

// SendMessageKeyboard — текст + inline-клавиатура (nil — без неё).
func (c *Client) SendMessageKeyboard(ctx context.Context, userID int64, text string, kb *Keyboard) error {
	m := maxbot.NewMessage().SetUser(userID).SetText(text)
	if kb != nil && len(kb.Buttons) > 0 {
		m.AddKeyboard(buildKeyboard(kb))
	}
	return c.api.Messages.Send(ctx, m)
}

// SendMessagePhoto — в MAX картинку отдаём превью ссылки (disable_link_preview
// по умолчанию выключен), поэтому image игнорируем: товарная ссылка в тексте
// сама разворачивается в карточку. Сигнатура совпадает с vk.Client для
// переиспользования в notifier.
func (c *Client) SendMessagePhoto(ctx context.Context, userID int64, text, _ string) error {
	return c.SendMessage(ctx, userID, text)
}

// AnswerCallback — гасит «часики» на нажатой кнопке (ответ не обязателен в MAX,
// но без него клиент крутит индикатор). Текст ответа не шлём — UX как в VK,
// где бот отвечает новым сообщением.
func (c *Client) AnswerCallback(ctx context.Context, callbackID string) {
	if callbackID == "" {
		return
	}
	_, _ = c.api.Messages.AnswerOnCallback(ctx, callbackID, &schemes.CallbackAnswer{})
}

// EnsureWebhook подписывает бота на доставку апдейтов по webhook на url с secret.
func (c *Client) EnsureWebhook(ctx context.Context, url, secret string) error {
	_, err := c.api.Subscriptions.Subscribe(ctx, url,
		[]string{
			string(schemes.TypeMessageCreated),
			string(schemes.TypeMessageCallback),
			string(schemes.TypeBotStarted),
		}, secret)
	return err
}

func buildKeyboard(kb *Keyboard) *maxbot.Keyboard {
	mk := &maxbot.Keyboard{}
	for _, row := range kb.Buttons {
		if len(row) == 0 {
			continue
		}
		r := mk.AddRow()
		for _, btn := range row {
			if btn.Link != "" {
				r.AddLink(btn.Label, schemes.DEFAULT, btn.Link)
			} else {
				r.AddCallback(btn.Label, schemes.DEFAULT, btn.Payload)
			}
		}
	}
	return mk
}

// ErrUnsupportedUpdate — апдейт валиден, но не из тех, что мы обрабатываем
// (например, bot_removed). Консьюмер тихо пропускает.
var ErrUnsupportedUpdate = errors.New("max: unsupported update type")

// DecodeUpdate превращает сырое тело апдейта в типизированный update. Зеркалит
// приватный maxbot.bytesToProperUpdate, но только для нужных нам типов.
func DecodeUpdate(raw []byte) (schemes.UpdateInterface, error) {
	base := &schemes.Update{}
	if err := json.Unmarshal(raw, base); err != nil {
		return nil, fmt.Errorf("max: decode base update: %w", err)
	}
	switch base.GetUpdateType() {
	case schemes.TypeMessageCreated:
		u := &schemes.MessageCreatedUpdate{}
		if err := json.Unmarshal(raw, u); err != nil {
			return nil, err
		}
		return u, nil
	case schemes.TypeMessageCallback:
		u := &schemes.MessageCallbackUpdate{}
		if err := json.Unmarshal(raw, u); err != nil {
			return nil, err
		}
		return u, nil
	case schemes.TypeBotStarted:
		u := &schemes.BotStartedUpdate{}
		if err := json.Unmarshal(raw, u); err != nil {
			return nil, err
		}
		return u, nil
	default:
		return nil, ErrUnsupportedUpdate
	}
}

// UserIDFromRaw достаёт user_id отправителя из сырого апдейта (ключ партиции
// Kafka — порядок диалога одного юзера сохраняется). 0 — не наш тип.
func UserIDFromRaw(raw []byte) int64 {
	var probe struct {
		UpdateType string `json:"update_type"`
		User       struct {
			UserID int64 `json:"user_id"`
		} `json:"user"`
		Callback struct {
			User struct {
				UserID int64 `json:"user_id"`
			} `json:"user"`
		} `json:"callback"`
		Message struct {
			Sender struct {
				UserID int64 `json:"user_id"`
			} `json:"sender"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return 0
	}
	switch {
	case probe.Message.Sender.UserID != 0:
		return probe.Message.Sender.UserID
	case probe.Callback.User.UserID != 0:
		return probe.Callback.User.UserID
	default:
		return probe.User.UserID
	}
}
