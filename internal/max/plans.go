package max

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Тарифы и триал в MAX. Витрина — domain.PlanShowcase/domain.Plans, как в TG/VK.

const dateLayout = "02.01.2006 15:04"

func (b *Bot) sendPlans(ctx context.Context, maxID int64, user *domain.User) {
	pct := b.usablePendingDiscount(ctx, user.ID)

	var sb strings.Builder
	sb.WriteString("💳 Тарифы TryberryBot\n\n" +
		"Подписка открывает больше отслеживаемых товаров, поиск-подписки и частые проверки цен. Действует 30 дней с момента оплаты.\n\n")
	if pct > 0 {
		fmt.Fprintf(&sb, "🎟 Промокод на %d%% применён — цены ниже уже со скидкой.\n\n", pct)
	}

	var rows [][]Button
	for _, sp := range domain.PlanShowcase {
		p, ok := domain.PlanByName(sp.Name)
		if !ok {
			continue
		}
		priceStr := fmt.Sprintf("%d ₽/мес", p.PriceRub)
		btnPrice := priceStr
		if pct > 0 {
			priceStr = fmt.Sprintf("%d → %s ₽/мес", p.PriceRub, discountedRub(p.PriceRub, pct))
			btnPrice = fmt.Sprintf("%s ₽/мес", discountedRub(p.PriceRub, pct))
		}
		fmt.Fprintf(&sb, "▫️ %s — %s · %s\n", p.Title, priceStr, sp.Tagline)
		fmt.Fprintf(&sb, "    📦 %d товаров · 🔎 %d поисков · 🕒 %s\n\n",
			p.MaxProduct, p.MaxSearch, domain.IntervalPhrase(p.Interval))
		rows = append(rows, []Button{TextButton(
			fmt.Sprintf("%s — %s", p.Title, btnPrice),
			fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdPlanCard, p.Name),
			ColorPrimary,
		)})
	}
	sb.WriteString("Бесплатный тариф Free — 5 товаров, без поиск-подписок. Новым пользователям доступен триал поиска — кнопка «Триал».")

	b.send(ctx, maxID, sb.String(), &Keyboard{Buttons: rows})
}

func (b *Bot) sendPlanCard(ctx context.Context, maxID int64, user *domain.User, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlans(ctx, maxID, user)
		return
	}
	tagline := ""
	for _, sp := range domain.PlanShowcase {
		if sp.Name == name {
			tagline = sp.Tagline
			break
		}
	}
	pct := b.usablePendingDiscount(ctx, user.ID)

	var sb strings.Builder
	fmt.Fprintf(&sb, "💳 Тариф %s — %d ₽/мес\n", p.Title, p.PriceRub)
	if tagline != "" {
		sb.WriteString(tagline + "\n")
	}
	sb.WriteString("\nЧто входит:\n")
	fmt.Fprintf(&sb, "📦 До %d отслеживаемых товаров\n", p.MaxProduct)
	fmt.Fprintf(&sb, "🔎 До %d поиск-подписок (слежу за всей поисковой выдачей)\n", p.MaxSearch)
	fmt.Fprintf(&sb, "🕒 Проверка цен %s\n", domain.IntervalPhrase(p.Interval))
	sb.WriteString("🔔 Уведомления о снижении цены в Telegram, VK и MAX\n\n")
	sb.WriteString("Доступ действует 30 дней с момента оплаты.")
	if pct > 0 {
		fmt.Fprintf(&sb, "\n\n🎟 Промокод: скидка %d%% применится к оплате.", pct)
	}

	buyLabel := fmt.Sprintf("💳 Оплатить %d ₽", p.PriceRub)
	subLabel := fmt.Sprintf("🔁 Подписка %d ₽/мес", p.SubPriceRub)
	buyOnceLabel := fmt.Sprintf("💳 Разовая оплата %d ₽", p.PriceRub)
	if pct > 0 {
		buyLabel = fmt.Sprintf("💳 Оплатить %s ₽", discountedRub(p.PriceRub, pct))
		subLabel = fmt.Sprintf("🔁 Подписка %s ₽/мес", discountedRub(p.SubPriceRub, pct))
		buyOnceLabel = fmt.Sprintf("💳 Разовая оплата %s ₽", discountedRub(p.PriceRub, pct))
	}

	var rows [][]Button
	if b.subSupported() && p.SubPriceRub > 0 {
		fmt.Fprintf(&sb, "\n\n🔁 С автопродлением выгоднее: %d ₽ вместо %d ₽.", p.SubPriceRub, p.PriceRub)
		rows = append(rows,
			[]Button{TextButton(subLabel, fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdSub, p.Name), ColorPrimary)},
			[]Button{TextButton(buyOnceLabel, fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdBuy, p.Name), ColorSecondary)},
		)
	} else {
		rows = append(rows, []Button{TextButton(buyLabel, fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdBuy, p.Name), ColorPrimary)})
	}
	if b.payments != nil {
		promoLabel := "🎟 У меня есть промокод"
		if pct > 0 {
			promoLabel = "🎟 Сменить промокод"
		}
		rows = append(rows, []Button{TextButton(promoLabel,
			fmt.Sprintf(`{"cmd":%q,"k":%q}`, cmdPromo, p.Name), ColorSecondary)})
	}
	rows = append(rows, []Button{TextButton("◀️ К тарифам", buttonPayload(cmdPlans), ColorSecondary)})
	b.send(ctx, maxID, sb.String(), &Keyboard{Buttons: rows})
}

func (b *Bot) subSupported() bool {
	return b.payments != nil && b.payments.SupportsSubscription()
}

func (b *Bot) handlePlanBuy(ctx context.Context, maxID int64, user *domain.User, name string) {
	if b.payments == nil {
		b.sendBuyStub(ctx, maxID, user, name)
		return
	}
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlans(ctx, maxID, user)
		return
	}

	email, err := b.userRepo.GetEmail(ctx, user.ID)
	if err != nil {
		b.log.Error("max: get email", "user_id", user.ID, "err", err)
		b.send(ctx, maxID, "⚠️ Не удалось начать оплату. Попробуй ещё раз позже.", menuKeyboard(user))
		return
	}
	if email == "" {
		if err := b.setEmailFSM(ctx, maxID, emailFSM{Plan: name}); err != nil {
			b.log.Error("max: set email fsm", "user_id", user.ID, "err", err)
		}
		amount := b.previewAmount(ctx, user.ID, int64(p.PriceRub)*100)
		b.send(ctx, maxID, emailRequestText(p, amount), menuKeyboard(user))
		return
	}

	b.startCheckout(ctx, maxID, user, name, email)
}

func (b *Bot) previewAmount(ctx context.Context, userID int64, full int64) int64 {
	if pct := b.usablePendingDiscount(ctx, userID); pct > 0 {
		return domain.DiscountedKopecks(full, pct)
	}
	return full
}

func (b *Bot) startCheckout(ctx context.Context, maxID int64, user *domain.User, name, email string) {
	checkout, err := b.payments.Start(ctx, user, name, email)
	if err != nil {
		b.log.Error("max: start checkout", "user_id", user.ID, "plan", name, "err", err)
		b.send(ctx, maxID, "⚠️ Не удалось создать платёж. Попробуй ещё раз позже.", menuKeyboard(user))
		return
	}
	p, _ := domain.PlanByName(name)

	var sb strings.Builder
	fmt.Fprintf(&sb, "💳 Оплата тарифа %s\n\n", p.Title)
	if checkout.DiscountPct > 0 {
		fmt.Fprintf(&sb, "Скидка по промокоду: %d%%\n", checkout.DiscountPct)
	}
	fmt.Fprintf(&sb, "К оплате: %s ₽ — подписка на %d дней.\n\n",
		domain.KopecksToRubString(checkout.AmountKopecks), domain.PurchaseDays)
	sb.WriteString("Перейди по ссылке для оплаты (карта, СБП, SberPay) — тариф подключится автоматически:\n")
	sb.WriteString(checkout.ConfirmationURL)

	kb := &Keyboard{Buttons: [][]Button{
		{LinkButton("💳 Перейти к оплате", checkout.ConfirmationURL)},
	}}
	b.send(ctx, maxID, sb.String(), kb)
}

func (b *Bot) sendBuyStub(ctx context.Context, maxID int64, user *domain.User, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlans(ctx, maxID, user)
		return
	}
	b.send(ctx, maxID, fmt.Sprintf(
		"💳 Оплата: тариф %s\n\n"+
			"К оплате: %d ₽ — подписка на 30 дней.\n\n"+
			"Оплата проходит через ЮKassa: банковская карта, СБП, SberPay.\n\n"+
			"⏳ Платёжный сервис сейчас подключается — кнопка оплаты появится здесь в ближайшие дни. "+
			"Хочешь подключить тариф уже сейчас — напиши @kosov_andrey (Telegram).",
		p.Title, p.PriceRub), menuKeyboard(user))
}

// ── Триал ─────────────────────────────────────────────────────────────────────

func (b *Bot) handleTrial(ctx context.Context, maxID int64, user *domain.User) {
	kb := menuKeyboard(user)
	now := time.Now()

	if user.TrialUsed {
		b.send(ctx, maxID, "🎁 Триал уже был активирован ранее.\n\n"+
			"Поиск-подписки есть на тарифах Lite и выше — кнопка «Тарифы».", kb)
		return
	}

	dur := domain.TrialDuration
	if user.ReferredBy != nil {
		dur = domain.ReferralTrialDuration
	}

	exp := now.Add(dur)
	ok, err := b.userRepo.ActivateTrial(ctx, user.ID, exp)
	if err != nil {
		b.log.Error("max: activate trial", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if !ok {
		b.send(ctx, maxID, "🎁 Триал уже был активирован ранее.", kb)
		return
	}

	b.restorePausedAfterUpgrade(ctx, user.ID, domain.Plans["trial"])

	days := int(dur.Hours() / 24)
	p := domain.Plans["trial"]
	b.send(ctx, maxID, fmt.Sprintf(
		"🎁 Триал активирован на %d %s!\n\n"+
			"Доступно поиск-подписок: %d.\nДействует до %s.\n\n"+
			"Отправь ссылку на поисковую выдачу Wildberries, чтобы попробовать 🔎",
		days, domain.DaysWord(days), p.MaxSearch, exp.Format(dateLayout)), kb)
}

func (b *Bot) restorePausedAfterUpgrade(ctx context.Context, userID int64, plan domain.Plan) {
	cutoff := time.Now().Add(-domain.PlanGracePeriod)

	activeS, _ := b.searchSubRepo.CountActiveByUserID(ctx, userID)
	if n, err := b.searchSubRepo.RestorePausedForUser(ctx, userID, plan.MaxSearch-activeS, cutoff); err != nil {
		b.log.Error("max: restore after upgrade: search", "err", err)
	} else if n > 0 {
		b.log.Info("max: restored paused search subs", "user", userID, "count", n)
	}

	activeP, _ := b.subRepo.CountActiveByUserID(ctx, userID)
	if n, err := b.subRepo.RestorePausedForUser(ctx, userID, plan.MaxProduct-activeP, cutoff); err != nil {
		b.log.Error("max: restore after upgrade: products", "err", err)
	} else if n > 0 {
		b.log.Info("max: restored paused product subs", "user", userID, "count", n)
	}
}
