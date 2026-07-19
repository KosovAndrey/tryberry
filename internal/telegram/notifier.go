package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"
)

// ErrTelegramPermanent — Telegram отверг запрос ПЕРМАНЕНТНО (HTTP 4xx, кроме 429):
// битая картинка («wrong type of the web page content»), юзер заблокировал бота
// (403), чат не найден и т.п. Ретрай не поможет — звать на нём кафку-петлю нельзя.
var ErrTelegramPermanent = errors.New("telegram permanent error")

type Notifier struct {
	token        string
	client       *http.Client
	chartBaseURL string       // PUBLIC_BASE_URL для кнопки «📈 График цены» ("" → не показываем)
	log          *slog.Logger // наблюдаемость тихих фолбэков (фото→текст)
}

func NewNotifier(token, chartBaseURL string, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		token:        token,
		client:       &http.Client{Timeout: 10 * time.Second},
		chartBaseURL: chartBaseURL,
		log:          log,
	}
}

// chartURL — ссылка на страницу графика товара или "" (сайт не задан / нет токена).
func (n *Notifier) chartURL(publicID string) string {
	if n.chartBaseURL == "" || publicID == "" {
		return ""
	}
	return n.chartBaseURL + "/p/" + publicID
}

type PriceAlert struct {
	ChatID         int64 // telegram_id (0 — TG не привязан, доставка только в VK)
	UserID         int64 // users.id — роутинг по notify_channel в deliverer
	SubscriptionID int64
	ProductName    string
	ProductURL     string
	// PublicID — токен товара для кнопки «📈 График цены» (ссылка на /p/<public_id>).
	// Пусто (старые in-flight алерты) → кнопку не добавляем.
	PublicID string
	ImageURL string
	OldPrice float64
	NewPrice float64
	// BackInStock — алерт о появлении товара в наличии (триггер back_in_stock),
	// а не о снижении цены. OldPrice не используется (товара не было в продаже).
	BackInStock bool
	// HonestLine — строка «честной цены» (вердикт по истории), пусто = не показывать.
	// Заполняет notifier через domain.AssessHonestPrice; рендерится только в
	// price-drop алерте (для back_in_stock не применяется).
	HonestLine string
}

// priceAlertKeyboard собирает inline-клавиатуру алерта: keep/untrack + (если есть
// ссылка) «📈 График цены» первой строкой как главный CTA при снижении цены.
func (n *Notifier) priceAlertKeyboard(a PriceAlert) map[string]any {
	rows := [][]map[string]any{
		{
			{"text": "✅ Продолжить следить", "callback_data": fmt.Sprintf("keep:%d", a.SubscriptionID)},
			{"text": "❌ Отменить отслеживание", "callback_data": fmt.Sprintf("untrack:%d", a.SubscriptionID)},
		},
	}
	if cu := n.chartURL(a.PublicID); cu != "" {
		rows = append([][]map[string]any{{{"text": "📈 График цены", "url": cu}}}, rows...)
	}
	return map[string]any{"inline_keyboard": rows}
}

func (n *Notifier) SendPriceAlert(ctx context.Context, a PriceAlert) error {
	var caption string
	if a.BackInStock {
		caption = fmt.Sprintf(
			"🔔 Снова в наличии!\n\n%s\n\nЦена: %.0f ₽\nТеперь слежу за снижением цены (поменять — /list)\n\n%s",
			a.ProductName, a.NewPrice, a.ProductURL,
		)
	} else {
		diff := a.OldPrice - a.NewPrice
		percent := math.Round(diff / a.OldPrice * 100)
		honest := ""
		if a.HonestLine != "" {
			honest = "\n" + a.HonestLine
		}
		caption = fmt.Sprintf(
			"📉 Цена снизилась!\n\n%s\n\nБыло: %.0f ₽ → Стало: %.0f ₽\nСкидка: %.0f ₽ (%.0f%%)%s\n\n%s",
			a.ProductName,
			a.OldPrice,
			a.NewPrice,
			diff,
			percent,
			honest,
			a.ProductURL,
		)
	}

	keyboard := n.priceAlertKeyboard(a)

	// Если есть картинка — sendPhoto, иначе sendMessage.
	if a.ImageURL != "" {
		err := n.sendPhoto(ctx, a.ChatID, a.ImageURL, caption, keyboard)
		// Картинку Telegram не принял (битый/недоступный URL — частый кейс для
		// трансграничных товаров без basket-картинки, «wrong type of the web page
		// content») → шлём текстом, чтобы алерт всё равно дошёл. Логируем: иначе
		// потеря фото невидима («было фото или нет?» не ответить по логам).
		if errors.Is(err, ErrTelegramPermanent) {
			n.log.Warn("price alert photo rejected, falling back to text", "image_url", a.ImageURL, "err", err)
			return n.sendMessage(ctx, a.ChatID, caption, keyboard)
		}
		return err
	}
	return n.sendMessage(ctx, a.ChatID, caption, keyboard)
}

// ── Бандлинг: пачка товарных алертов одного юзера одним сообщением ────────────

// maxBundleItems — сколько позиций показываем в бандл-сообщении; остальные
// сворачиваем в «и ещё N». Telegram-сообщение влезает в 4096 символов.
const maxBundleItems = 15

type BundledAlertItem struct {
	ProductName string
	ProductURL  string
	OldPrice    float64
	NewPrice    float64
	BackInStock bool // снова в наличии (а не снижение цены)
}

// BundledAlert — несколько товарных обновлений одного юзера в одном сообщении
// (гибрид: вызывается только при ≥2 позициях; одиночный алерт идёт богатым
// SendPriceAlert с фото). См. docs/SCALING-NOTIFIER-DELIVERY.md.
type BundledAlert struct {
	ChatID int64 // telegram_id (0 — TG не привязан, доставка только в VK)
	UserID int64 // users.id — роутинг по notify_channel в deliverer
	Items  []BundledAlertItem
}

func (n *Notifier) SendBundledAlert(ctx context.Context, a BundledAlert) error {
	if len(a.Items) == 0 {
		return nil
	}

	hasBack := false
	for _, it := range a.Items {
		if it.BackInStock {
			hasBack = true
			break
		}
	}

	var sb strings.Builder
	if hasBack {
		fmt.Fprintf(&sb, "🔔 Обновления по вашим товарам (%d):\n\n", len(a.Items))
	} else {
		fmt.Fprintf(&sb, "📉 По вашим товарам снизилась цена (%d):\n\n", len(a.Items))
	}

	shown := a.Items
	if len(shown) > maxBundleItems {
		shown = shown[:maxBundleItems]
	}
	for _, it := range shown {
		name := html.EscapeString(it.ProductName)
		if it.BackInStock {
			fmt.Fprintf(&sb, "🔔 <a href=\"%s\">%s</a>\n    снова в наличии — <b>%.0f ₽</b>\n\n",
				it.ProductURL, name, it.NewPrice)
			continue
		}
		fmt.Fprintf(&sb, "📉 <a href=\"%s\">%s</a>\n", it.ProductURL, name)
		if it.OldPrice > it.NewPrice && it.OldPrice > 0 {
			pct := math.Round((it.OldPrice - it.NewPrice) / it.OldPrice * 100)
			fmt.Fprintf(&sb, "    <b>%.0f ₽</b>  (было %.0f ₽, -%.0f%%)\n\n", it.NewPrice, it.OldPrice, pct)
		} else {
			fmt.Fprintf(&sb, "    <b>%.0f ₽</b>\n\n", it.NewPrice)
		}
	}
	if len(a.Items) > maxBundleItems {
		fmt.Fprintf(&sb, "…и ещё %d — смотри /list\n\n", len(a.Items)-maxBundleItems)
	}
	sb.WriteString("Управлять отслеживанием — /list")

	return n.sendMessage(ctx, a.ChatID, sb.String(), nil)
}

// ── Search-подписки: батч подешевевших товаров по одному запросу ─────────────

type SearchAlertItem struct {
	Name         string
	URL          string
	ImageURL     string  // фото товара; у Items[0] (топ-снижение) идёт hero-картинкой
	EffectiveRub float64 // цена с учётом баллов (что пользователь платит по факту)
	PrevRub      float64 // опорная цена (baseline/last-notified); «было» в сообщении
	PointsRub    float64 // баллы за отзыв в рублях, 0 если нет
}

type SearchAlert struct {
	ChatID    int64 // telegram_id (0 — TG не привязан, доставка только в VK)
	UserID    int64 // users.id — роутинг по notify_channel в deliverer
	QueryText string
	SearchURL string // ссылка на выдачу (кнопка «Открыть выдачу»)
	TotalHits int    // сколько всего товаров подешевело (может быть > len(Items))
	Items     []SearchAlertItem
}

// SendSearchAlert — одно сообщение на подписку: топ подешевевших товаров.
// Если подходящих больше, чем в Items, добавляется приписка «нашлось больше».
// searchCaptionBudget — приблизительный бюджет ВИДИМЫХ символов подписи к
// hero-фото. Лимит подписи Telegram — 1024 (URL внутри <a href> в него не входят),
// держим запас. Позиции сверх бюджета сворачиваются в «…и ещё N».
const searchCaptionBudget = 950

func (n *Notifier) SendSearchAlert(ctx context.Context, a SearchAlert) error {
	if len(a.Items) == 0 {
		return nil
	}
	keyboard := searchAlertKeyboard(a)

	// Hero-фото: картинка топ-снижения (Items отсортированы по величине падения в
	// cmd/notifier). Есть фото у топа → sendPhoto с подписью в пределах лимита,
	// хвост в «…и ещё N». Битую картинку Telegram отвергает (ErrTelegramPermanent,
	// частый кейс для трансграничных товаров) → фолбэк на полный текст. Нет фото у
	// топа → сразу текст (полный список до 4096). Один механизм на free-дайджест
	// (много позиций) и платную одиночку (богатая карточка).
	if hero := a.Items[0].ImageURL; hero != "" {
		caption := renderSearchAlert(a, searchCaptionBudget)
		err := n.sendPhoto(ctx, a.ChatID, hero, caption, keyboard)
		if errors.Is(err, ErrTelegramPermanent) {
			n.log.Warn("search alert hero photo rejected, falling back to text", "image_url", hero, "err", err)
			return n.sendMessage(ctx, a.ChatID, renderSearchAlert(a, 0), keyboard)
		}
		return err
	}
	// Без фото у топа — обычный текст; лог, чтобы отличать «hero не было» от
	// «hero отвергнут» при разборах.
	n.log.Info("search alert without hero (no image on top item)", "sub_id", a.UserID)
	return n.sendMessage(ctx, a.ChatID, renderSearchAlert(a, 0), keyboard)
}

func searchAlertKeyboard(a SearchAlert) any {
	if a.SearchURL == "" {
		return nil
	}
	return map[string]any{
		"inline_keyboard": [][]map[string]any{
			{{"text": "🔎 Открыть выдачу", "url": a.SearchURL}},
		},
	}
}

// renderSearchAlert собирает текст уведомления. budget>0 — лимит ВИДИМЫХ символов
// (подпись к фото): рендерим позиции, пока влезают, остальные — «…и ещё N».
// budget<=0 — без лимита (обычное сообщение до 4096). Топ-позиция показывается
// всегда, даже если одна её длина превышает бюджет.
func renderSearchAlert(a SearchAlert, budget int) string {
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
	visible := len([]rune(a.QueryText)) + 60 // грубая оценка заголовка

	shown := 0
	for _, it := range a.Items {
		name := it.Name
		if budget > 0 && len([]rune(name)) > 64 {
			name = string([]rune(name)[:63]) + "…"
		}
		blockVisible := len([]rune(name)) + 40 // имя + строка цены, без URL-энтити
		if budget > 0 && shown > 0 && visible+blockVisible > budget {
			break
		}
		fmt.Fprintf(&sb, "📉 <a href=\"%s\">%s</a>\n", it.URL, html.EscapeString(name))
		if it.PrevRub > it.EffectiveRub && it.PrevRub > 0 {
			fmt.Fprintf(&sb, "    <b>%.0f ₽</b>  (было %.0f ₽)", it.EffectiveRub, it.PrevRub)
		} else {
			fmt.Fprintf(&sb, "    <b>%.0f ₽</b>", it.EffectiveRub)
		}
		if it.PointsRub > 0 {
			fmt.Fprintf(&sb, "  +%.0f баллов", it.PointsRub)
		}
		sb.WriteString("\n\n")
		visible += blockVisible
		shown++
	}
	if shown < len(a.Items) {
		fmt.Fprintf(&sb, "…и ещё %d", len(a.Items)-shown)
	}
	return sb.String()
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

// SendTrialWinback — пуш win-back-цепочки конца триала (стадии 1/2/3) с
// персональным discount-кодом. Тексты стадий собирает domain-независимый
// WinbackText в notifier — сюда приходит готовый HTML.
func (n *Notifier) SendTrialWinback(ctx context.Context, chatID int64, html, code string) error {
	keyboard := map[string]any{
		"inline_keyboard": [][]map[string]any{
			{{"text": "🎟 Применить скидку", "callback_data": "promo:apply:" + code}},
			{{"text": "💳 Тарифы", "callback_data": "menu:plans"}},
		},
	}
	return n.sendMessage(ctx, chatID, html, keyboard)
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

// SendDigest шлёт персональный дайджест ПЛОСКИМ текстом (без parse_mode): тот же
// текст годится для VK и не требует экранирования имён товаров с & / <. Превью
// ссылок выключено — их в дайджесте много.
func (n *Notifier) SendDigest(ctx context.Context, chatID int64, text string) error {
	return n.call(ctx, "sendMessage", map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
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
		// 4xx (кроме 429) — перманентно: запрос некорректен / недоставляем. Ретрай
		// бесполезен, помечаем sentinel'ом, чтобы выше не зациклить кафку.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("%w: telegram error: %s", ErrTelegramPermanent, tgResp.Description)
		}
		return fmt.Errorf("telegram error: %s", tgResp.Description)
	}
	return nil
}
