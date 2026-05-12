package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

type Notifier struct {
	token  string
	client *http.Client
}

func NewNotifier(token string) *Notifier {
	return &Notifier{
		token:  token,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

type PriceAlert struct {
	ChatID         int64
	SubscriptionID int64
	ProductName    string
	ProductURL     string
	ImageURL       string
	OldPrice       float64
	NewPrice       float64
}

func (n *Notifier) SendPriceAlert(ctx context.Context, a PriceAlert) error {
	diff := a.OldPrice - a.NewPrice
	percent := math.Round(diff / a.OldPrice * 100)

	caption := fmt.Sprintf(
		"📉 Цена снизилась!\n\n%s\n\nБыло: %.0f ₽ → Стало: %.0f ₽\nСкидка: %.0f ₽ (%.0f%%)\n\n%s",
		a.ProductName,
		a.OldPrice,
		a.NewPrice,
		diff,
		percent,
		a.ProductURL,
	)

	keyboard := map[string]any{
		"inline_keyboard": [][]map[string]any{
			{
				{"text": "✅ Продолжить следить", "callback_data": fmt.Sprintf("keep:%d", a.SubscriptionID)},
				{"text": "❌ Отменить отслеживание", "callback_data": fmt.Sprintf("untrack:%d", a.SubscriptionID)},
			},
		},
	}

	// Если есть картинка — sendPhoto, иначе sendMessage
	if a.ImageURL != "" {
		return n.sendPhoto(ctx, a.ChatID, a.ImageURL, caption, keyboard)
	}
	return n.sendMessage(ctx, a.ChatID, caption, keyboard)
}

func (n *Notifier) sendPhoto(ctx context.Context, chatID int64, photo, caption string, keyboard any) error {
	payload := map[string]any{
		"chat_id":      chatID,
		"photo":        photo,
		"caption":      caption,
		"parse_mode":   "HTML",
		"reply_markup": keyboard,
	}
	return n.call(ctx, "sendPhoto", payload)
}

func (n *Notifier) sendMessage(ctx context.Context, chatID int64, text string, keyboard any) error {
	payload := map[string]any{
		"chat_id":      chatID,
		"text":         text,
		"parse_mode":   "HTML",
		"reply_markup": keyboard,
	}
	return n.call(ctx, "sendMessage", payload)
}

func (n *Notifier) call(ctx context.Context, method string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/%s", n.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	var tgResp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tgResp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !tgResp.OK {
		return fmt.Errorf("telegram error: %s", tgResp.Description)
	}
	return nil
}
