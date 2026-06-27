package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Согласие на обработку персональных данных (152-ФЗ). Перед первым использованием
// бота просим пользователя подтвердить согласие явным действием (кнопка
// «Принимаю») — до этого момента запросы не обрабатываем, показываем экран
// согласия. Полный текст — на сайте: tryberry.ru/#consent и tryberry.ru/#privacy.
// Дата согласия хранится в users.pd_consent_at (см. migrations/023).

// needsPDConsent — TG-пользователь ещё не дал согласие. Для VK-only (telegram_id=0)
// гейт здесь не применяем — у VK-бота отдельный флоу.
func needsPDConsent(u *domain.User) bool {
	return u != nil && u.TelegramID != 0 && u.PDConsentAt == nil
}

// siteBaseURL — публичный адрес сайта для ссылок на документы. Берём
// PUBLIC_BASE_URL (chartBaseURL), иначе надёжный фолбэк на прод-домен.
func (b *Bot) siteBaseURL() string {
	if b.chartBaseURL != "" {
		return b.chartBaseURL
	}
	return "https://tryberry.ru"
}

// pendingStartKey хранит deep-link payload (?start=ref_…|promo_…), пришедший до
// согласия, чтобы применить его сразу после нажатия «Принимаю» (иначе атрибуция
// реферала/промокода терялась бы на экране согласия).
func pendingStartKey(tgID int64) string { return fmt.Sprintf("pending_start:%d", tgID) }

func (b *Bot) stashPendingStart(ctx context.Context, tgID int64, payload string) {
	if b.rdb == nil || payload == "" {
		return
	}
	b.rdb.Set(ctx, pendingStartKey(tgID), payload, fsmTTL)
}

func (b *Bot) takePendingStart(ctx context.Context, tgID int64) string {
	if b.rdb == nil {
		return ""
	}
	v, err := b.rdb.Get(ctx, pendingStartKey(tgID)).Result()
	if err != nil {
		return ""
	}
	b.rdb.Del(ctx, pendingStartKey(tgID))
	return v
}

// sendPDConsent показывает экран согласия. messageID != 0 → правит существующее
// сообщение (вызов из callback), 0 → шлёт новое (ответ на команду/сообщение).
func (b *Bot) sendPDConsent(chatID int64, messageID int) {
	base := b.siteBaseURL()
	text := "🍓 <b>Добро пожаловать в TryBerry!</b>\n\n" +
		"Чтобы следить за ценами и присылать уведомления, я сохраняю минимум данных: " +
		"твой Telegram-идентификатор и имя пользователя, ссылки на товары, которые ты добавляешь, " +
		"и выбранный тариф.\n\n" +
		"Перед началом подтверди <b>согласие на обработку персональных данных</b> (152-ФЗ) — " +
		"полный текст и Политику конфиденциальности можно открыть по кнопкам ниже.\n\n" +
		"Нажимая «Принимаю», ты соглашаешься с обработкой персональных данных на условиях Согласия и Политики."

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("📄 Согласие на обработку ПД", base+"/#consent"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("🔒 Политика конфиденциальности", base+"/#privacy"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Принимаю — продолжить", "consent:accept"),
		),
	)
	b.showView(chatID, messageID, text, keyboard)
}

// handlePDConsentAccept фиксирует согласие и продолжает: применяет отложенный
// deep-link payload (?start=ref_…|promo_…), иначе показывает главное меню.
// Callback уже отвечен в handleCallback (b.answerCallback), повторно не отвечаем.
func (b *Bot) handlePDConsentAccept(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	chatID := cb.Message.Chat.ID
	messageID := cb.Message.MessageID

	user, err := b.userRepo.Upsert(ctx, cb.From.ID, cb.From.UserName)
	if err != nil {
		b.log.Error("consent: upsert user", "err", err)
		b.editMenu(chatID, messageID, "Произошла ошибка, попробуй ещё раз: /start", backToMenuKeyboard())
		return
	}
	if err := b.userRepo.MarkPDConsent(ctx, user.ID, time.Now()); err != nil {
		b.log.Error("consent: mark", "err", err)
		b.editMenu(chatID, messageID, "Произошла ошибка, попробуй ещё раз: /start", backToMenuKeyboard())
		return
	}
	// Чтобы дальнейшие хендлеры (handlePromo/handleRefStart) не загейтили снова.
	now := time.Now()
	user.PDConsentAt = &now

	// Заменяем экран согласия главным меню.
	b.sendMainMenu(ctx, chatID, messageID, true)

	// Применяем отложенный deep-link, если он был до согласия.
	if payload := b.takePendingStart(ctx, cb.From.ID); payload != "" {
		if code, ok := strings.CutPrefix(payload, "promo_"); ok {
			b.handlePromo(ctx, chatID, user, code)
			return
		}
		if ref, ok := strings.CutPrefix(payload, "ref_"); ok {
			b.handleRefStart(ctx, chatID, user, ref)
			return
		}
	}
}
