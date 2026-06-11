package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"net/http"
	"strings"
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

// ── Search-подписки: батч подешевевших товаров по одному запросу ─────────────

type SearchAlertItem struct {
	Name         string
	URL          string
	EffectiveRub float64 // цена с учётом баллов (что пользователь платит по факту)
	PrevRub      float64 // опорная цена (baseline/last-notified); «было» в сообщении
	PointsRub    float64 // баллы за отзыв в рублях, 0 если нет
}

type SearchAlert struct {
	ChatID    int64
	QueryText string
	SearchURL string // ссылка на выдачу (кнопка «Открыть выдачу»)
	TotalHits int    // сколько всего товаров подешевело (может быть > len(Items))
	Items     []SearchAlertItem
}

// SendSearchAlert — одно сообщение на подписку: топ подешевевших товаров.
// Если подходящих больше, чем в Items, добавляется приписка «нашлось больше».
func (n *Notifier) SendSearchAlert(ctx context.Context, a SearchAlert) error {
	if len(a.Items) == 0 {
		return nil
	}

	var sb strings.Builder
	q := html.EscapeString(a.QueryText)
	if a.TotalHits > len(a.Items) {
		fmt.Fprintf(&sb,
			"🔎 По запросу «%s» подешевело <b>%d</b> товаров.\n"+
				"Нашлось больше, чем нужно — показываю лучшие %d. Если ищешь что-то конкретное, сузь ссылку отслеживания.\n\n",
			q, a.TotalHits, len(a.Items))
	} else {
		fmt.Fprintf(&sb, "🔎 По запросу «%s» подешевело <b>%d</b> товаров:\n\n", q, a.TotalHits)
	}

	for _, it := range a.Items {
		name := html.EscapeString(it.Name)
		fmt.Fprintf(&sb, "📉 <a href=\"%s\">%s</a>\n", it.URL, name)
		if it.PrevRub > it.EffectiveRub && it.PrevRub > 0 {
			fmt.Fprintf(&sb, "    <b>%.0f ₽</b>  (было %.0f ₽)", it.EffectiveRub, it.PrevRub)
		} else {
			fmt.Fprintf(&sb, "    <b>%.0f ₽</b>", it.EffectiveRub)
		}
		if it.PointsRub > 0 {
			fmt.Fprintf(&sb, "  +%.0f баллов", it.PointsRub)
		}
		sb.WriteString("\n\n")
	}

	var keyboard any
	if a.SearchURL != "" {
		keyboard = map[string]any{
			"inline_keyboard": [][]map[string]any{
				{{"text": "🔎 Открыть выдачу", "url": a.SearchURL}},
			},
		}
	}
	return n.sendMessage(ctx, a.ChatID, sb.String(), keyboard)
}

// SendPlanPausedNotice — разовое уведомление: план истёк, поиск-подписки на
// паузе. Зовётся reconciler'ом в момент постановки на паузу. chatID = telegram_id.
func (n *Notifier) SendPlanPausedNotice(ctx context.Context, chatID int64) error {
	text := "⏳ <b>Триал закончился</b>\n\n" +
		"Часть твоих подписок <b>приостановлена</b> (вышли за лимит бесплатного " +
		"тарифа). Я сохраню их настройки ещё <b>7 дней</b> — оформи подписку за это " +
		"время, и я верну их и продолжу следить за ценами. Потом они удалятся.\n\n" +
		"Твой тариф и лимиты: /myplan"
	keyboard := map[string]any{
		"inline_keyboard": [][]map[string]any{
			{{"text": "💳 Оформить подписку", "url": "https://t.me/kosov_andrey"}},
		},
	}
	return n.sendMessage(ctx, chatID, text, keyboard)
}

// SendPlanExpiringReminder — разовое напоминание за сутки до конца тарифа.
func (n *Notifier) SendPlanExpiringReminder(ctx context.Context, chatID int64) error {
	text := "⏳ <b>Тариф скоро закончится</b>\n\n" +
		"Завтра истекает срок твоего тарифа. Продли, чтобы не потерять подписки и " +
		"лимиты — иначе часть из них будет приостановлена.\n\n" +
		"Твой тариф и лимиты: /myplan"
	keyboard := map[string]any{
		"inline_keyboard": [][]map[string]any{
			{{"text": "💳 Продлить", "url": "https://t.me/kosov_andrey"}},
		},
	}
	return n.sendMessage(ctx, chatID, text, keyboard)
}

// SendReferralRewardNotice — другу засчитана активация, рефереру начислены дни.
// granted=false — награда записана в аудит, но план не менялся (бессрочный план).
func (n *Notifier) SendReferralRewardNotice(ctx context.Context, chatID int64, friendName string, days int, granted bool) error {
	who := "Твой друг"
	if friendName != "" {
		who = "Твой друг @" + friendName
	}
	text := fmt.Sprintf("🎉 <b>%s освоился в боте!</b>\n\n", who)
	if granted {
		text += fmt.Sprintf("За это тебе начислено <b>+%d дн.</b> тарифа — спасибо, что зовёшь друзей!\n\n", days)
	} else {
		text += "Награда записана — спасибо, что зовёшь друзей!\n\n"
	}
	text += "Твоя ссылка и статистика: /ref"
	return n.sendMessage(ctx, chatID, text, nil)
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
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"reply_markup":             keyboard,
		"disable_web_page_preview": true,
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
