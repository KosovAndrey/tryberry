package telegram

import (
	"context"
	"errors"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// subscriptionLine — строка статуса подписки для экрана «Мой тариф» + кнопка
// отмены, если есть активная/просроченная подписка. Пусто, если подписки нет
// или биллинг не подключён.
func (b *Bot) subscriptionLine(ctx context.Context, userID int64) (string, []tgbotapi.InlineKeyboardButton) {
	if b.billing == nil {
		return "", nil
	}
	sub, err := b.billing.GetActiveByUserID(ctx, userID)
	if errors.Is(err, domain.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		b.log.Error("myplan: load subscription", "user_id", userID, "err", err)
		return "", nil
	}

	switch sub.Status {
	case domain.SubStatusPastDue:
		return fmt.Sprintf("\n🔁 Автопродление: <b>оплата не прошла</b>, повторим списание %s ₽.\n",
			domain.KopecksToRubString(sub.AmountKopecks)), nil
	default: // active
		line := fmt.Sprintf("\n🔁 Автопродление: <b>включено</b> · %s ₽ каждые %d дней · следующее списание %s\n",
			domain.KopecksToRubString(sub.AmountKopecks), domain.PurchaseDays, sub.NextChargeAt.Format(dateLayout))
		btn := tgbotapi.NewInlineKeyboardButtonData("🚫 Отменить автопродление", "sub:cancel")
		return line, []tgbotapi.InlineKeyboardButton{btn}
	}
}

// handleSubCancelConfirm — экран подтверждения отмены автопродления.
func (b *Bot) handleSubCancelConfirm(ctx context.Context, telegramID, chatID int64, messageID int) {
	user, err := b.userRepo.GetByTelegramID(ctx, telegramID)
	if err != nil {
		b.log.Error("sub cancel confirm: load user", "telegram_id", telegramID, "err", err)
		b.showView(chatID, messageID, "⚠️ Не удалось открыть подписку, попробуй позже.", backToMenuKeyboard())
		return
	}
	if _, err := b.billing.GetActiveByUserID(ctx, user.ID); errors.Is(err, domain.ErrNotFound) {
		b.showView(chatID, messageID, "У тебя нет активной подписки.", backToMenuKeyboard())
		return
	} else if err != nil {
		b.log.Error("sub cancel confirm: load sub", "user_id", user.ID, "err", err)
		b.showView(chatID, messageID, "⚠️ Не удалось открыть подписку, попробуй позже.", backToMenuKeyboard())
		return
	}

	text := "🚫 <b>Отменить автопродление?</b>\n\n" +
		"Доступ к тарифу сохранится до конца уже оплаченного периода. Дальнейших списаний не будет.\n\n" +
		"Подключить заново можно в любой момент в разделе «Тарифы»."
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Да, отменить", "sub:cancelok"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Оставить подписку", "menu:myplan"),
		),
	)
	b.showView(chatID, messageID, text, keyboard)
}

// handleSubCancel — отменяет автопродление (доступ остаётся до конца периода).
func (b *Bot) handleSubCancel(ctx context.Context, telegramID, chatID int64, messageID int) {
	user, err := b.userRepo.GetByTelegramID(ctx, telegramID)
	if err != nil {
		b.log.Error("sub cancel: load user", "telegram_id", telegramID, "err", err)
		b.showView(chatID, messageID, "⚠️ Не удалось отменить, попробуй позже.", backToMenuKeyboard())
		return
	}
	canceled, err := b.billing.Cancel(ctx, user.ID)
	if err != nil {
		b.log.Error("sub cancel: cancel", "user_id", user.ID, "err", err)
		b.showView(chatID, messageID, "⚠️ Не удалось отменить, попробуй позже.", backToMenuKeyboard())
		return
	}
	if !canceled {
		b.showView(chatID, messageID, "У тебя нет активной подписки.", backToMenuKeyboard())
		return
	}
	b.showView(chatID, messageID,
		"✅ <b>Автопродление отменено.</b>\n\nДоступ к тарифу сохранится до конца оплаченного периода. "+
			"Подключить заново — в разделе «Тарифы».",
		tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("💳 Тарифы", "menu:plans"),
				tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
			),
		))
}
