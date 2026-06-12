package vk

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Тарифы и триал в VK. Витрина — domain.PlanShowcase/domain.Plans, как в TG.
// Кнопка «Оплатить» — заглушка до запуска ЮKassa, подписки не выдаются.

const dateLayout = "02.01.2006 15:04"

func (b *Bot) sendPlans(ctx context.Context, vkID int64, user *domain.User) {
	var sb strings.Builder
	sb.WriteString("💳 Тарифы TryberryBot\n\n" +
		"Подписка открывает больше отслеживаемых товаров, поиск-подписки и частые проверки цен. Действует 30 дней с момента оплаты.\n\n")

	var rows [][]Button
	for _, sp := range domain.PlanShowcase {
		p, ok := domain.PlanByName(sp.Name)
		if !ok {
			continue
		}
		fmt.Fprintf(&sb, "▫️ %s — %d ₽/мес · %s\n", p.Title, p.PriceRub, sp.Tagline)
		fmt.Fprintf(&sb, "    📦 %d товаров · 🔎 %d поисков · ⏱ %s\n\n",
			p.MaxProduct, p.MaxSearch, domain.IntervalPhrase(p.Interval))
		rows = append(rows, []Button{TextButton(
			fmt.Sprintf("%s — %d ₽/мес", p.Title, p.PriceRub),
			fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdPlanCard, p.Name),
			ColorPrimary,
		)})
	}
	sb.WriteString("Бесплатный тариф Free — 5 товаров, без поиск-подписок. Новым пользователям доступен триал поиска — кнопка «Триал».")

	b.send(ctx, vkID, sb.String(), &Keyboard{Inline: true, Buttons: rows})
}

func (b *Bot) sendPlanCard(ctx context.Context, vkID int64, user *domain.User, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlans(ctx, vkID, user)
		return
	}
	tagline := ""
	for _, sp := range domain.PlanShowcase {
		if sp.Name == name {
			tagline = sp.Tagline
			break
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "💳 Тариф %s — %d ₽/мес\n", p.Title, p.PriceRub)
	if tagline != "" {
		sb.WriteString(tagline + "\n")
	}
	sb.WriteString("\nЧто входит:\n")
	fmt.Fprintf(&sb, "📦 До %d отслеживаемых товаров\n", p.MaxProduct)
	fmt.Fprintf(&sb, "🔎 До %d поиск-подписок (слежу за всей поисковой выдачей)\n", p.MaxSearch)
	fmt.Fprintf(&sb, "⏱ Проверка цен %s\n", domain.IntervalPhrase(p.Interval))
	sb.WriteString("🔔 Уведомления о снижении цены в Telegram и VK\n\n")
	sb.WriteString("Подписка действует 30 дней с момента оплаты.")

	kb := &Keyboard{Inline: true, Buttons: [][]Button{
		{TextButton(fmt.Sprintf("💳 Оплатить %d ₽", p.PriceRub),
			fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdBuy, p.Name), ColorPrimary)},
		{TextButton("◀️ К тарифам", buttonPayload(cmdPlans), ColorSecondary)},
	}}
	b.send(ctx, vkID, sb.String(), kb)
}

func (b *Bot) sendBuyStub(ctx context.Context, vkID int64, user *domain.User, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlans(ctx, vkID, user)
		return
	}
	b.send(ctx, vkID, fmt.Sprintf(
		"💳 Оплата: тариф %s\n\n"+
			"К оплате: %d ₽ — подписка на 30 дней.\n\n"+
			"Оплата проходит через ЮKassa: банковская карта, СБП, SberPay.\n\n"+
			"⏳ Платёжный сервис сейчас подключается — кнопка оплаты появится здесь в ближайшие дни. "+
			"Хочешь подключить тариф уже сейчас — напиши @kosov_andrey (Telegram).",
		p.Title, p.PriceRub), menuKeyboard(user.TelegramID != 0))
}

// ── Триал ─────────────────────────────────────────────────────────────────────

func (b *Bot) handleTrial(ctx context.Context, vkID int64, user *domain.User) {
	kb := menuKeyboard(user.TelegramID != 0)
	now := time.Now()

	if user.TrialUsed {
		b.send(ctx, vkID, "🎁 Триал уже был активирован ранее.\n\n"+
			"Поиск-подписки есть на тарифах Lite и выше — кнопка «Тарифы».", kb)
		return
	}

	// Приглашённым по реферальной ссылке — расширенный триал (как в TG).
	dur := domain.TrialDuration
	if user.ReferredBy != nil {
		dur = domain.ReferralTrialDuration
	}

	exp := now.Add(dur)
	ok, err := b.userRepo.ActivateTrial(ctx, user.ID, exp)
	if err != nil {
		b.log.Error("vk: activate trial", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if !ok {
		b.send(ctx, vkID, "🎁 Триал уже был активирован ранее.", kb)
		return
	}

	b.restorePausedAfterUpgrade(ctx, user.ID, domain.Plans["trial"])

	days := int(dur.Hours() / 24)
	p := domain.Plans["trial"]
	b.send(ctx, vkID, fmt.Sprintf(
		"🎁 Триал активирован на %d %s!\n\n"+
			"Доступно поиск-подписок: %d.\nДействует до %s.\n\n"+
			"Отправь ссылку на поисковую выдачу Wildberries, чтобы попробовать 🔎",
		days, domain.DaysWord(days), p.MaxSearch, exp.Format(dateLayout)), kb)
}

// restorePausedAfterUpgrade — мгновенно вернуть паузные подписки (в пределах
// grace) до лимитов нового плана; reconciler сделал бы это на ближайшем тике,
// хук убирает лаг. Ошибки только логируем.
func (b *Bot) restorePausedAfterUpgrade(ctx context.Context, userID int64, plan domain.Plan) {
	cutoff := time.Now().Add(-domain.PlanGracePeriod)

	activeS, _ := b.searchSubRepo.CountActiveByUserID(ctx, userID)
	if n, err := b.searchSubRepo.RestorePausedForUser(ctx, userID, plan.MaxSearch-activeS, cutoff); err != nil {
		b.log.Error("vk: restore after upgrade: search", "err", err)
	} else if n > 0 {
		b.log.Info("vk: restored paused search subs", "user", userID, "count", n)
	}

	activeP, _ := b.subRepo.CountActiveByUserID(ctx, userID)
	if n, err := b.subRepo.RestorePausedForUser(ctx, userID, plan.MaxProduct-activeP, cutoff); err != nil {
		b.log.Error("vk: restore after upgrade: products", "err", err)
	} else if n > 0 {
		b.log.Info("vk: restored paused product subs", "user", userID, "count", n)
	}
}
