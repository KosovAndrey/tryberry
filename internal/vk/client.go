// Package vk — минимальный клиент VK API (бот сообщества) и обработчик
// входящих событий Callback API. Фаза 1 VK-интеграции: привязка аккаунтов
// и базовые ответы; полный UX — фаза 2 (см. docs/VK-INTEGRATION-PLAN.md).
//
// vk.com доступен с RU-VM напрямую — прокси, в отличие от Telegram, не нужен.
package vk

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
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
	Type    string `json:"type"`              // text | open_link
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

func (c *Client) call(ctx context.Context, method string, params url.Values) error {
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

	var out struct {
		Error *struct {
			Code int    `json:"error_code"`
			Msg  string `json:"error_msg"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("vk %s: decode: %w", method, err)
	}
	if out.Error != nil {
		return fmt.Errorf("vk %s: api error %d: %s", method, out.Error.Code, out.Error.Msg)
	}
	return nil
}
