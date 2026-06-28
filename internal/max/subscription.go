package max

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

func (b *Bot) subscriptionLine(ctx context.Context, userID int64) (string, bool) {
	if b.billing == nil {
		return "", false
	}
	sub, err := b.billing.GetActiveByUserID(ctx, userID)
	if errors.Is(err, domain.ErrNotFound) {
		return "", false
	}
	if err != nil {
		b.log.Error("max: load subscription", "user_id", userID, "err", err)
		return "", false
	}
	if sub.Status == domain.SubStatusPastDue {
		return fmt.Sprintf("🔁 Автопродление: оплата не прошла, повторим списание %s ₽.\n",
			domain.KopecksToRubString(sub.AmountKopecks)), true
	}
	return fmt.Sprintf("🔁 Автопродление: включено · %s ₽ каждые %d дней · следующее списание %s\n",
		domain.KopecksToRubString(sub.AmountKopecks), domain.PurchaseDays, sub.NextChargeAt.Format(dateLayout)), true
}

func (b *Bot) sendSubConsent(ctx context.Context, maxID int64, user *domain.User, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.SubPriceRub <= 0 || !b.subSupported() {
		b.sendPlanCard(ctx, maxID, user, name)
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

	kb := &Keyboard{Buttons: [][]Button{
		{TextButton("✅ Подключить подписку", fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdSubOk, name), ColorPrimary)},
		{TextButton("◀️ Назад", fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdPlanCard, name), ColorSecondary)},
	}}
	b.send(ctx, maxID, text, kb)
}

func (b *Bot) handleSubBuy(ctx context.Context, maxID int64, user *domain.User, name string) {
	if !b.subSupported() {
		b.handlePlanBuy(ctx, maxID, user, name)
		return
	}
	p, ok := domain.PlanByName(name)
	if !ok || p.SubPriceRub <= 0 {
		b.sendPlans(ctx, maxID, user)
		return
	}

	email, err := b.userRepo.GetEmail(ctx, user.ID)
	if err != nil {
		b.log.Error("max: sub get email", "user_id", user.ID, "err", err)
		b.send(ctx, maxID, "⚠️ Не удалось начать оплату. Попробуй ещё раз позже.", menuKeyboard(user))
		return
	}
	if email == "" {
		if err := b.setEmailFSM(ctx, maxID, emailFSM{Plan: name, Sub: true}); err != nil {
			b.log.Error("max: sub set email fsm", "user_id", user.ID, "err", err)
		}
		amount := b.previewAmount(ctx, user.ID, int64(p.SubPriceRub)*100)
		b.send(ctx, maxID, emailRequestText(p, amount), menuKeyboard(user))
		return
	}
	b.startSubCheckout(ctx, maxID, user, name, email)
}

func (b *Bot) startSubCheckout(ctx context.Context, maxID int64, user *domain.User, name, email string) {
	checkout, err := b.payments.StartSubscription(ctx, user, name, email, "max")
	if err != nil {
		b.log.Error("max: start sub checkout", "user_id", user.ID, "plan", name, "err", err)
		b.send(ctx, maxID, "⚠️ Не удалось создать платёж. Попробуй ещё раз позже.", menuKeyboard(user))
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

	kb := &Keyboard{Buttons: [][]Button{
		{LinkButton("💳 Перейти к оплате", checkout.ConfirmationURL)},
	}}
	b.send(ctx, maxID, sb.String(), kb)
}

func (b *Bot) handleSubCancelConfirm(ctx context.Context, maxID int64, user *domain.User) {
	if b.billing == nil {
		b.send(ctx, maxID, "Подписки недоступны.", menuKeyboard(user))
		return
	}
	if _, err := b.billing.GetActiveByUserID(ctx, user.ID); errors.Is(err, domain.ErrNotFound) {
		b.send(ctx, maxID, "У тебя нет активной подписки.", menuKeyboard(user))
		return
	} else if err != nil {
		b.log.Error("max: sub cancel confirm", "user_id", user.ID, "err", err)
		b.send(ctx, maxID, "⚠️ Не удалось открыть подписку, попробуй позже.", menuKeyboard(user))
		return
	}
	text := "🚫 Отменить автопродление?\n\n" +
		"Доступ к тарифу сохранится до конца уже оплаченного периода. Дальнейших списаний не будет."
	kb := &Keyboard{Buttons: [][]Button{
		{TextButton("✅ Да, отменить", buttonPayload(cmdSubCancelOk), ColorPrimary)},
		{TextButton("◀️ Оставить подписку", buttonPayload(cmdProfile), ColorSecondary)},
	}}
	b.send(ctx, maxID, text, kb)
}

func (b *Bot) handleSubCancel(ctx context.Context, maxID int64, user *domain.User) {
	if b.billing == nil {
		b.send(ctx, maxID, "Подписки недоступны.", menuKeyboard(user))
		return
	}
	canceled, err := b.billing.Cancel(ctx, user.ID)
	if err != nil {
		b.log.Error("max: sub cancel", "user_id", user.ID, "err", err)
		b.send(ctx, maxID, "⚠️ Не удалось отменить, попробуй позже.", menuKeyboard(user))
		return
	}
	if !canceled {
		b.send(ctx, maxID, "У тебя нет активной подписки.", menuKeyboard(user))
		return
	}
	b.send(ctx, maxID,
		"✅ Автопродление отменено.\n\nДоступ к тарифу сохранится до конца оплаченного периода. "+
			"Подключить заново — в разделе «Тарифы».",
		menuKeyboard(user))
}
