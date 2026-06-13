package vk

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// subscriptionLine — строка статуса подписки для профиля + флаг «есть активная»
// (показывать ли кнопку отмены). Пусто, если подписки нет или биллинг не подключён.
func (b *Bot) subscriptionLine(ctx context.Context, userID int64) (string, bool) {
	if b.billing == nil {
		return "", false
	}
	sub, err := b.billing.GetActiveByUserID(ctx, userID)
	if errors.Is(err, domain.ErrNotFound) {
		return "", false
	}
	if err != nil {
		b.log.Error("vk: load subscription", "user_id", userID, "err", err)
		return "", false
	}
	if sub.Status == domain.SubStatusPastDue {
		return fmt.Sprintf("🔁 Автопродление: оплата не прошла, повторим списание %s ₽.\n",
			domain.KopecksToRubString(sub.AmountKopecks)), true
	}
	return fmt.Sprintf("🔁 Автопродление: включено · %s ₽ каждые %d дней · следующее списание %s\n",
		domain.KopecksToRubString(sub.AmountKopecks), domain.PurchaseDays, sub.NextChargeAt.Format(dateLayout)), true
}

// sendSubConsent — экран явного согласия на подписку (сумма, период,
// автопродление, как отменить). Согласие фиксируется при создании платежа.
func (b *Bot) sendSubConsent(ctx context.Context, vkID int64, user *domain.User, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.SubPriceRub <= 0 || !b.subSupported() {
		b.sendPlanCard(ctx, vkID, user, name)
		return
	}
	text := fmt.Sprintf(
		"🔁 Подписка на тариф %s\n\n"+
			"• Стоимость: %d ₽ каждые 30 дней\n"+
			"• Оплата списывается автоматически с карты в конце периода\n"+
			"• Отменить автопродление можно в любой момент в профиле — "+
			"доступ сохранится до конца оплаченного периода\n"+
			"• Перед каждым списанием пришлём напоминание\n\n"+
			"Нажимая «Подключить подписку», ты соглашаешься с автосписанием на этих условиях.",
		p.Title, p.SubPriceRub)

	kb := &Keyboard{Inline: true, Buttons: [][]Button{
		{TextButton("✅ Подключить подписку", fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdSubOk, name), ColorPrimary)},
		{TextButton("◀️ Назад", fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdPlanCard, name), ColorSecondary)},
	}}
	b.send(ctx, vkID, text, kb)
}

// handleSubBuy — старт подписки (после согласия). Email-флоу как у разовой
// оплаты, дальше StartSubscription.
func (b *Bot) handleSubBuy(ctx context.Context, vkID int64, user *domain.User, name string) {
	if !b.subSupported() {
		b.handlePlanBuy(ctx, vkID, user, name)
		return
	}
	p, ok := domain.PlanByName(name)
	if !ok || p.SubPriceRub <= 0 {
		b.sendPlans(ctx, vkID, user)
		return
	}

	email, err := b.userRepo.GetEmail(ctx, user.ID)
	if err != nil {
		b.log.Error("vk: sub get email", "user_id", user.ID, "err", err)
		b.send(ctx, vkID, "⚠️ Не удалось начать оплату. Попробуй ещё раз позже.", menuKeyboard(user.TelegramID != 0))
		return
	}
	if email == "" {
		if err := b.setEmailFSM(ctx, vkID, emailFSM{Plan: name, Sub: true}); err != nil {
			b.log.Error("vk: sub set email fsm", "user_id", user.ID, "err", err)
		}
		amount := b.previewAmount(ctx, user.ID, int64(p.SubPriceRub)*100)
		b.send(ctx, vkID, emailRequestText(p, amount), menuKeyboard(user.TelegramID != 0))
		return
	}
	b.startSubCheckout(ctx, vkID, user, name, email)
}

// startSubCheckout — создаёт первый платёж подписки и шлёт ссылку оплаты.
func (b *Bot) startSubCheckout(ctx context.Context, vkID int64, user *domain.User, name, email string) {
	checkout, err := b.payments.StartSubscription(ctx, user, name, email, "vk")
	if err != nil {
		b.log.Error("vk: start sub checkout", "user_id", user.ID, "plan", name, "err", err)
		b.send(ctx, vkID, "⚠️ Не удалось создать платёж. Попробуй ещё раз позже.", menuKeyboard(user.TelegramID != 0))
		return
	}
	p, _ := domain.PlanByName(name)

	var sb strings.Builder
	fmt.Fprintf(&sb, "🔁 Подписка на тариф %s\n\n", p.Title)
	if checkout.DiscountPct > 0 {
		fmt.Fprintf(&sb, "Скидка по промокоду на первый платёж: %d%%\n", checkout.DiscountPct)
	}
	fmt.Fprintf(&sb, "Первый платёж: %s ₽\n", domain.KopecksToRubString(checkout.AmountKopecks))
	fmt.Fprintf(&sb, "Далее: %s ₽ каждые %d дней (автосписание)\n\n",
		domain.KopecksToRubString(checkout.RenewalKopecks), domain.PurchaseDays)
	sb.WriteString("Перейди по ссылке для оплаты — тариф подключится автоматически. " +
		"Отменить автопродление можно в профиле:\n")
	sb.WriteString(checkout.ConfirmationURL)

	kb := &Keyboard{Inline: true, Buttons: [][]Button{
		{LinkButton("💳 Перейти к оплате", checkout.ConfirmationURL)},
	}}
	b.send(ctx, vkID, sb.String(), kb)
}

// handleSubCancelConfirm — экран подтверждения отмены автопродления.
func (b *Bot) handleSubCancelConfirm(ctx context.Context, vkID int64, user *domain.User) {
	if b.billing == nil {
		b.send(ctx, vkID, "Подписки недоступны.", menuKeyboard(user.TelegramID != 0))
		return
	}
	if _, err := b.billing.GetActiveByUserID(ctx, user.ID); errors.Is(err, domain.ErrNotFound) {
		b.send(ctx, vkID, "У тебя нет активной подписки.", menuKeyboard(user.TelegramID != 0))
		return
	} else if err != nil {
		b.log.Error("vk: sub cancel confirm", "user_id", user.ID, "err", err)
		b.send(ctx, vkID, "⚠️ Не удалось открыть подписку, попробуй позже.", menuKeyboard(user.TelegramID != 0))
		return
	}
	text := "🚫 Отменить автопродление?\n\n" +
		"Доступ к тарифу сохранится до конца уже оплаченного периода. Дальнейших списаний не будет."
	kb := &Keyboard{Inline: true, Buttons: [][]Button{
		{TextButton("✅ Да, отменить", buttonPayload(cmdSubCancelOk), ColorPrimary)},
		{TextButton("◀️ Оставить подписку", buttonPayload(cmdProfile), ColorSecondary)},
	}}
	b.send(ctx, vkID, text, kb)
}

// handleSubCancel — отменяет автопродление (доступ до конца периода).
func (b *Bot) handleSubCancel(ctx context.Context, vkID int64, user *domain.User) {
	if b.billing == nil {
		b.send(ctx, vkID, "Подписки недоступны.", menuKeyboard(user.TelegramID != 0))
		return
	}
	canceled, err := b.billing.Cancel(ctx, user.ID)
	if err != nil {
		b.log.Error("vk: sub cancel", "user_id", user.ID, "err", err)
		b.send(ctx, vkID, "⚠️ Не удалось отменить, попробуй позже.", menuKeyboard(user.TelegramID != 0))
		return
	}
	if !canceled {
		b.send(ctx, vkID, "У тебя нет активной подписки.", menuKeyboard(user.TelegramID != 0))
		return
	}
	b.send(ctx, vkID,
		"✅ Автопродление отменено.\n\nДоступ к тарифу сохранится до конца оплаченного периода. "+
			"Подключить заново — в разделе «Тарифы».",
		menuKeyboard(user.TelegramID != 0))
}
