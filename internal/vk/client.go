// Package vk — минимальный клиент VK API (бот сообщества) и обработчик
// входящих событий Callback API. Фаза 1 VK-интеграции: привязка аккаунтов
// и базовые ответы; полный UX — фаза 2 (см. docs/VK-INTEGRATION-PLAN.md).
//
// vk.com доступен с RU-VM напрямую — прокси, в отличие от Telegram, не нужен.
package vk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const apiVersion = "5.199"

type Client struct {
	token string
	http  *http.Client
}

func NewClient(token string) *Client {
	return &Client{
		token: token,
		http:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Keyboard — клавиатура VK (messages.send, параметр keyboard).
// Inline=false → постоянная клавиатура под полем ввода: остаётся между
// сообщениями, обновляется с каждым ответом бота.
type Keyboard struct {
	OneTime bool       `json:"one_time"`
	Inline  bool       `json:"inline,omitempty"`
	Buttons [][]Button `json:"buttons"`
}

type Button struct {
	Action ButtonAction `json:"action"`
	Color  string       `json:"color,omitempty"`
}

type ButtonAction struct {
	Type    string `json:"type"` // text | open_link
	Label   string `json:"label,omitempty"`
	Payload string `json:"payload,omitempty"` // JSON-строка, прилетает в message_new.payload
	Link    string `json:"link,omitempty"`
}

// Цвета text-кнопок (open_link цвет не поддерживает).
const (
	ColorPrimary   = "primary"
	ColorSecondary = "secondary"
)

// TextButton — кнопка, нажатие которой шлёт боту label + payload.
func TextButton(label, payload, color string) Button {
	return Button{Action: ButtonAction{Type: "text", Label: label, Payload: payload}, Color: color}
}

// LinkButton — кнопка-ссылка (открывает URL, боту ничего не шлёт).
func LinkButton(label, link string) Button {
	return Button{Action: ButtonAction{Type: "open_link", Label: label, Link: link}}
}

// SendMessage — текст в ЛС пользователю (peerID = vk user id).
// random_id защищает от дублей при ретраях.
func (c *Client) SendMessage(ctx context.Context, peerID int64, text string) error {
	return c.SendMessageKeyboard(ctx, peerID, text, nil)
}

// SendMessageKeyboard — текст + клавиатура (nil — не трогать текущую).
func (c *Client) SendMessageKeyboard(ctx context.Context, peerID int64, text string, kb *Keyboard) error {
	params := url.Values{
		"peer_id":   {strconv.FormatInt(peerID, 10)},
		"message":   {text},
		"random_id": {strconv.FormatInt(rand.Int63(), 10)}, //nolint:gosec // не криптослучайность, защита от дублей
	}
	if kb != nil {
		raw, err := json.Marshal(kb)
		if err != nil {
			return fmt.Errorf("vk keyboard: %w", err)
		}
		params.Set("keyboard", string(raw))
	}
	return c.call(ctx, "messages.send", params)
}

// SendMessagePhoto — текст с фото-вложением (картинка товара). imageURL качаем и
// грузим в VK. Если с фото что-то не так — шлём текст без картинки: уведомление
// важнее картинки (best-effort). Возвращает ошибку только самой отправки.
func (c *Client) SendMessagePhoto(ctx context.Context, peerID int64, text, imageURL string) error {
	params := url.Values{
		"peer_id":   {strconv.FormatInt(peerID, 10)},
		"message":   {text},
		"random_id": {strconv.FormatInt(rand.Int63(), 10)}, //nolint:gosec // защита от дублей
	}
	if imageURL != "" {
		if att, err := c.uploadPhotoFromURL(ctx, peerID, imageURL); err == nil && att != "" {
			params.Set("attachment", att)
		}
	}
	return c.call(ctx, "messages.send", params)
}

// uploadPhotoFromURL качает картинку по URL и грузит её в VK как вложение для ЛС:
// photos.getMessagesUploadServer → multipart-upload → photos.saveMessagesPhoto →
// строка "photo<owner>_<id>".
func (c *Client) uploadPhotoFromURL(ctx context.Context, peerID int64, imageURL string) (string, error) {
	var srv struct {
		Response struct {
			UploadURL string `json:"upload_url"`
		} `json:"response"`
	}
	if err := c.callJSON(ctx, "photos.getMessagesUploadServer",
		url.Values{"peer_id": {strconv.FormatInt(peerID, 10)}}, &srv); err != nil {
		return "", err
	}
	if srv.Response.UploadURL == "" {
		return "", fmt.Errorf("vk: empty upload_url")
	}

	// качаем картинку
	imgReq, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", err
	}
	imgResp, err := c.http.Do(imgReq)
	if err != nil {
		return "", fmt.Errorf("vk: download image: %w", err)
	}
	defer imgResp.Body.Close()
	if imgResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vk: download image status %d", imgResp.StatusCode)
	}
	imgData, err := io.ReadAll(io.LimitReader(imgResp.Body, 25<<20))
	if err != nil {
		return "", err
	}

	// multipart upload на upload_url (поле photo)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("photo", "image.jpg")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(imgData); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.Response.UploadURL, &body)
	if err != nil {
		return "", err
	}
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upResp, err := c.http.Do(upReq)
	if err != nil {
		return "", fmt.Errorf("vk: upload photo: %w", err)
	}
	defer upResp.Body.Close()
	var uploaded struct {
		Server int    `json:"server"`
		Photo  string `json:"photo"`
		Hash   string `json:"hash"`
	}
	if err := json.NewDecoder(upResp.Body).Decode(&uploaded); err != nil {
		return "", fmt.Errorf("vk: upload decode: %w", err)
	}
	if uploaded.Photo == "" || uploaded.Photo == "[]" {
		return "", fmt.Errorf("vk: empty upload result")
	}

	// сохраняем → owner_id/id
	var saved struct {
		Response []struct {
			OwnerID int64 `json:"owner_id"`
			ID      int64 `json:"id"`
		} `json:"response"`
	}
	if err := c.callJSON(ctx, "photos.saveMessagesPhoto", url.Values{
		"server": {strconv.Itoa(uploaded.Server)},
		"photo":  {uploaded.Photo},
		"hash":   {uploaded.Hash},
	}, &saved); err != nil {
		return "", err
	}
	if len(saved.Response) == 0 {
		return "", fmt.Errorf("vk: empty save result")
	}
	p := saved.Response[0]
	return fmt.Sprintf("photo%d_%d", p.OwnerID, p.ID), nil
}

func (c *Client) call(ctx context.Context, method string, params url.Values) error {
	return c.callJSON(ctx, method, params, nil)
}

// callJSON вызывает метод VK API и (если out != nil) декодирует ответ в out.
func (c *Client) callJSON(ctx context.Context, method string, params url.Values, out any) error {
	params.Set("access_token", c.token)
	params.Set("v", apiVersion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.vk.com/method/"+method, nil)
	if err != nil {
		return err
	}
	req.URL.RawQuery = params.Encode()

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vk %s: %w", method, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("vk %s: read: %w", method, err)
	}
	var probe struct {
		Error *struct {
			Code int    `json:"error_code"`
			Msg  string `json:"error_msg"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &probe); err == nil && probe.Error != nil {
		return fmt.Errorf("vk %s: api error %d: %s", method, probe.Error.Code, probe.Error.Msg)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("vk %s: decode: %w", method, err)
		}
	}
	return nil
}
