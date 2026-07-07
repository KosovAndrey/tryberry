// Package max — клиент MAX Bot API (max.ru) и обработчик входящих апдейтов.
// Полный паритет с Telegram/VK: привязка аккаунтов, трекинг товаров, поиск-
// подписки, тарифы и оплата. См. docs/MAX-INTEGRATION-PLAN.md.
//
// max.ru доступен с RU-VM напрямую — прокси, как и для VK, не нужен. Транспорт —
// официальный клиент github.com/max-messenger/max-bot-api-client-go; здесь тонкая
// обёртка, повторяющая API клавиатур VK, чтобы фичовые файлы портировались 1:1.
package max

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
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

// Лимиты дорожки фото-аттача в уведомлениях (см. SendMessagePhoto).
const (
	// maxImageBytes — потолок скачиваемой картинки товара. Больше — считаем
	// подозрительным/битым и уходим в текстовый фолбэк, чтобы не тянуть мегабайты
	// в память на волне алертов.
	maxImageBytes = 10 << 20
	// imageDownloadTimeout — таймаут скачивания картинки с CDN маркетплейса.
	// Отдельный от 30с MAX API: картинка не должна задерживать доставку текста.
	imageDownloadTimeout = 10 * time.Second
	// photoCacheTTL — сколько держим upload-токен фото. Токены MAX переиспользуемы
	// между сообщениями (schema.yaml: «Use token ... to reuse the same attachment
	// in other message»), а одно фото товара уходит десяткам подписчиков на одной
	// волне — час покрывает волну, не рискуя протухшими токенами.
	photoCacheTTL = time.Hour
)

// photoCacheEntry — закэшированный upload-токен фото с моментом протухания.
type photoCacheEntry struct {
	tokens *schemes.PhotoTokens
	exp    time.Time
}

// Client — обёртка над maxbot.Api с VK-подобным API отправки.
type Client struct {
	api *maxbot.Api
	log *slog.Logger

	// imgHTTP — отдельный клиент для СКАЧИВАНИЯ картинки товара с CDN маркетплейса.
	// Не MAX-клиент: тут не нужен российский корень, зато нужен короткий таймаут.
	imgHTTP *http.Client

	mu         sync.Mutex // защищает photoCache
	photoCache map[string]photoCacheEntry
}

func NewClient(token string) (*Client, error) {
	api, err := maxbot.New(token, maxbot.WithHTTPClient(maxHTTPClient()))
	if err != nil {
		return nil, err
	}
	return &Client{
		api:        api,
		log:        slog.Default(),
		imgHTTP:    &http.Client{Timeout: imageDownloadTimeout},
		photoCache: make(map[string]photoCacheEntry),
	}, nil
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

// SendMessagePhoto — уведомление о цене с фото товара как аттачем. Раньше image
// игнорировали в ставке на превью ссылки, но авто-превью маркетплейса приходит
// обрезанным и убогим; поэтому качаем картинку, грузим её в MAX и шлём как
// PhotoAttachment, а превью ссылки в этом сообщении гасим (SetDisableLinkPreview),
// чтобы карточка не дублировалась. Best-effort: при ЛЮБОЙ ошибке дорожки с фото
// (скачивание/upload/сборка) откатываемся на текущее поведение — текст с превью:
// доставка уведомления важнее картинки. Сигнатура совпадает с vk.Client для
// переиспользования в notifier.
func (c *Client) SendMessagePhoto(ctx context.Context, userID int64, text, imageURL string) error {
	if imageURL != "" {
		tokens, err := c.resolvePhotoTokens(ctx, imageURL)
		if err != nil {
			c.log.Warn("max: photo attach skipped, fallback to text",
				"user_id", userID, "image_url", imageURL, "err", err)
		} else {
			m := maxbot.NewMessage().SetUser(userID).SetText(text).
				SetDisableLinkPreview(true).AddPhoto(tokens)
			if sendErr := c.api.Messages.Send(ctx, m); sendErr == nil {
				return nil
			} else {
				// Send с фото не прошёл — попробуем хотя бы текст (превью включено).
				c.log.Warn("max: photo message send failed, fallback to text",
					"user_id", userID, "err", sendErr)
			}
		}
	}
	return c.SendMessage(ctx, userID, text)
}

// resolvePhotoTokens возвращает upload-токен фото по URL: сперва из кэша, иначе
// скачивает картинку своим http-клиентом и грузит в MAX. Токены переиспользуемы,
// поэтому кэшируем их по imageURL (см. photoCacheTTL) — на волне алертов одно фото
// уходит многим подписчикам, и повторные upload'ы не нужны.
func (c *Client) resolvePhotoTokens(ctx context.Context, imageURL string) (*schemes.PhotoTokens, error) {
	if t := c.cachedPhoto(imageURL); t != nil {
		return t, nil
	}
	data, err := c.downloadImage(ctx, imageURL)
	if err != nil {
		return nil, err
	}
	tokens, err := c.api.Uploads.UploadPhotoFromReader(ctx, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("max: upload photo: %w", err)
	}
	c.cachePhoto(imageURL, tokens)
	return tokens, nil
}

// downloadImage качает картинку товара с CDN маркетплейса. Лимит maxImageBytes,
// чтобы не утянуть в память лишнего; ошибка (сеть/не-200/переразмер) уводит
// вызывающего в текстовый фолбэк.
func (c *Client) downloadImage(ctx context.Context, imageURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.imgHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("max: download image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("max: download image status %d", resp.StatusCode)
	}
	// Читаем на 1 байт больше лимита: если прочлось maxImageBytes+1 — картинка
	// превышает потолок, отбрасываем.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("max: read image: %w", err)
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("max: image too large (> %d bytes)", maxImageBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("max: empty image body")
	}
	return data, nil
}

// cachedPhoto — свежий токен из кэша либо nil (промах/протухло).
func (c *Client) cachedPhoto(imageURL string) *schemes.PhotoTokens {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.photoCache[imageURL]
	if !ok || time.Now().After(e.exp) {
		return nil
	}
	return e.tokens
}

// cachePhoto кладёт токен в кэш с TTL photoCacheTTL.
func (c *Client) cachePhoto(imageURL string, tokens *schemes.PhotoTokens) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.photoCache[imageURL] = photoCacheEntry{tokens: tokens, exp: time.Now().Add(photoCacheTTL)}
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
