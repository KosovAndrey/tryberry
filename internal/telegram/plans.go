package telegram

import (
	"context"
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Витрина тарифов: порядок/слоганы — domain.PlanShowcase, цифры — domain.Plans.
// Кнопка «Оплатить» пока ведёт на заглушку: платёжный сервис (ЮKassa) на
// подключении, подписки НЕ выдаются. После запуска оплаты plan:buy:*
// заменяется на создание платежа.

// sendPlansMenu — список тарифов с ценами. messageID != 0 → в том же сообщении.
// При ожидающей скидке (промокод) цены в списке показываем зачёркнутой старой и
// новой рядом, а в кнопках — уже со скидкой.
func (b *Bot) sendPlansMenu(ctx context.Context, tgID, chatID int64, messageID int) {
	pct := b.pendingDiscountPct(ctx, tgID)

	var sb strings.Builder
	sb.WriteString("💳 <b>Тарифы TryberryBot</b>\n\n" +
		"Подписка открывает больше отслеживаемых товаров, поиск-подписки и частые проверки цен. Действует 30 дней с момента оплаты.\n\n")
	if pct > 0 {
		fmt.Fprintf(&sb, "🎟 Промокод на <b>%d%%</b> применён — цены ниже уже со скидкой.\n\n", pct)
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, sp := range domain.PlanShowcase {
		p, ok := domain.PlanByName(sp.Name)
		if !ok {
			continue
		}
		priceStr := fmt.Sprintf("%d ₽/мес", p.PriceRub)
		btnPrice := priceStr
		if pct > 0 {
			priceStr = fmt.Sprintf("<s>%d ₽</s> <b>%s ₽</b>/мес", p.PriceRub, discountedRub(p.PriceRub, pct))
			btnPrice = fmt.Sprintf("%s ₽/мес", discountedRub(p.PriceRub, pct))
		}
		fmt.Fprintf(&sb, "▫️ <b>%s</b> — %s · %s\n", p.Title, priceStr, sp.Tagline)
		fmt.Fprintf(&sb, "    📦 %d товаров · 🔎 %d поисков · ⏱ %s\n\n",
			p.MaxProduct, p.MaxSearch, domain.IntervalPhrase(p.Interval))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("%s — %s", p.Title, btnPrice),
				"plan:view:"+p.Name,
			),
		))
	}

	sb.WriteString("➕ Мало поисков или товаров? Собери <b>Pro+</b> или <b>Reseller Pro+</b> с нужными лимитами.\n\n")
	sb.WriteString("Бесплатный тариф Free — 5 товаров и 1 поиск-подписка (проверка раз в 6 часов). Новым пользователям доступен триал поиска: /trial.")

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("➕ Нужно больше? Собрать свой", "plus:menu"),
	))
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
	))
	b.showView(chatID, messageID, sb.String(), tgbotapi.NewInlineKeyboardMarkup(rows...))
}

// sendPlanCard — карточка тарифа: описание, состав, цена, кнопка оплаты. Если у
// юзера есть ожидающая скидка (промокод) — цены и кнопки показываем уже со скидкой.
func (b *Bot) sendPlanCard(ctx context.Context, tgID, chatID int64, messageID int, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlansMenu(ctx, tgID, chatID, messageID)
		return
	}
	tagline := ""
	for _, sp := range domain.PlanShowcase {
		if sp.Name == name {
			tagline = sp.Tagline
			break
		}
	}
	pct := b.pendingDiscountPct(ctx, tgID)

	var sb strings.Builder
	fmt.Fprintf(&sb, "💳 <b>Тариф %s</b> — %d ₽/мес\n", p.Title, p.PriceRub)
	if tagline != "" {
		fmt.Fprintf(&sb, "<i>%s</i>\n", tagline)
	}
	sb.WriteString("\n<b>Что входит:</b>\n")
	fmt.Fprintf(&sb, "📦 До %d отслеживаемых товаров\n", p.MaxProduct)
	fmt.Fprintf(&sb, "🔎 До %d поиск-подписок (слежу за всей поисковой выдачей)\n", p.MaxSearch)
	fmt.Fprintf(&sb, "⏱ Проверка цен %s\n", domain.IntervalPhrase(p.Interval))
	sb.WriteString("🔔 Уведомления о снижении цены в Telegram и VK\n\n")
	sb.WriteString("Доступ действует <b>30 дней</b> с момента оплаты.")
	if pct > 0 {
		fmt.Fprintf(&sb, "\n\n🎟 Промокод: скидка <b>%d%%</b> применится к оплате.", pct)
	}

	buyLabel := fmt.Sprintf("💳 Оплатить %d ₽", p.PriceRub)
	subLabel := fmt.Sprintf("🔁 Подписка %d ₽/мес", p.SubPriceRub)
	buyOnceLabel := fmt.Sprintf("💳 Разовая оплата %d ₽", p.PriceRub)
	if pct > 0 {
		buyLabel = fmt.Sprintf("💳 Оплатить %s ₽", discountedRub(p.PriceRub, pct))
		subLabel = fmt.Sprintf("🔁 Подписка %s ₽/мес", discountedRub(p.SubPriceRub, pct))
		buyOnceLabel = fmt.Sprintf("💳 Разовая оплата %s ₽", discountedRub(p.PriceRub, pct))
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	if b.subSupported() && p.SubPriceRub > 0 {
		// Подписка — рекомендуемый (и более дешёвый) вариант, ставим первой.
		fmt.Fprintf(&sb, "\n\n🔁 С <b>автопродлением</b> — выгоднее: <b>%d ₽</b> вместо %d ₽.", p.SubPriceRub, p.PriceRub)
		rows = append(rows,
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(subLabel, "plan:sub:"+p.Name)),
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(buyOnceLabel, "plan:buy:"+p.Name)),
		)
	} else {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(buyLabel, "plan:buy:"+p.Name)))
	}
	// «Промокод» в платёжном флоу — когда оплата реально доступна. При уже
	// применённой скидке кнопка остаётся (даёт сменить код на другой — discount
	// гасится только после оплаты, так что перезапись бесплатна).
	if b.payments != nil {
		promoLabel := "🎟 У меня есть промокод"
		if pct > 0 {
			promoLabel = "🎟 Сменить промокод"
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(promoLabel, "plan:promo:"+p.Name),
		))
	}
	if domain.IsPlusPlan(p.Name) {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Изменить лимиты", "plus:cfg:"+p.Name),
		))
	} else if pb, ok := domain.PlusKeyForBase(p.Name); ok {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("➕ Нужно больше — собрать %s", pb.Title), "plus:cfg:"+pb.DefaultName()),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ К тарифам", "menu:plans"),
	))
	b.showView(chatID, messageID, sb.String(), tgbotapi.NewInlineKeyboardMarkup(rows...))
}

// sendPlusPicker — экран «Нужно больше?»: выбор линейки Pro+ / Reseller Pro+.
func (b *Bot) sendPlusPicker(chatID int64, messageID int) {
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, pb := range domain.PlusBases {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("%s — от %d ₽/мес", pb.Title, pb.FromRub()), "plus:cfg:"+pb.DefaultName()),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ К тарифам", "menu:plans"),
	))
	b.showView(chatID, messageID, domain.PlusPickerText("<b>", "</b>"), tgbotapi.NewInlineKeyboardMarkup(rows...))
}

// sendPlusConfig — конфигуратор «плюс»-тарифа: ±шаг по поискам/товарам
// пересобирает то же сообщение, «Дальше» ведёт на обычную карточку тарифа
// (оплата/подписка/промокод — общий флоу по имени плана).
func (b *Bot) sendPlusConfig(ctx context.Context, tgID, chatID int64, messageID int, name string) {
	text, ok := domain.PlusConfigText(name, "<b>", "</b>")
	if !ok {
		b.sendPlusPicker(chatID, messageID)
		return
	}
	pb, s, pr, _ := domain.ParsePlusConfig(name)

	// stepRow — пара «− / +» по одной оси; кнопку в упоре (пол/потолок) не
	// показываем, чтобы нажатие не было пустым.
	stepRow := func(ds, dp int, minus, plus string) []tgbotapi.InlineKeyboardButton {
		var row []tgbotapi.InlineKeyboardButton
		if n, _ := domain.PlusStep(name, -ds, -dp); n != name {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(minus, "plus:cfg:"+n))
		}
		if n, _ := domain.PlusStep(name, ds, dp); n != name {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(plus, "plus:cfg:"+n))
		}
		return row
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	if row := stepRow(1, 0, fmt.Sprintf("➖ %d 🔎", pb.SearchStep), fmt.Sprintf("➕ %d 🔎", pb.SearchStep)); len(row) > 0 {
		rows = append(rows, row)
	}
	if row := stepRow(0, 1, fmt.Sprintf("➖ %d 📦", pb.ProductStep), fmt.Sprintf("➕ %d 📦", pb.ProductStep)); len(row) > 0 {
		rows = append(rows, row)
	}

	payName, _ := domain.PlusPayName(name)
	payLabel := fmt.Sprintf("✅ Дальше — %d ₽/мес", pb.Price(s, pr))
	if pct := b.pendingDiscountPct(ctx, tgID); pct > 0 {
		payLabel = fmt.Sprintf("✅ Дальше — %s ₽/мес", discountedRub(pb.Price(s, pr), pct))
	}
	rows = append(rows,
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(payLabel, "plan:view:"+payName)),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("◀️ К тарифам", "menu:plans")),
	)
	b.showView(chatID, messageID, text, tgbotapi.NewInlineKeyboardMarkup(rows...))
}

// discountedRub — цена в рублях (целое) со скидкой pct%, готовая строка ("349").
func discountedRub(fullRub, pct int) string {
	return domain.KopecksToRubString(domain.DiscountedKopecks(int64(fullRub)*100, pct))
}

// pendingDiscountPct — процент ожидающей скидки юзера для превью (по telegram_id).
// 0, если скидки нет ИЛИ она уже не применится на оплате (см. usablePendingDiscount).
func (b *Bot) pendingDiscountPct(ctx context.Context, tgID int64) int {
	if b.discounts == nil {
		return 0
	}
	user, err := b.userRepo.GetByTelegramID(ctx, tgID)
	if err != nil {
		return 0
	}
	return b.usablePendingDiscount(ctx, user.ID)
}

// usablePendingDiscount — ожидающая скидка юзера, ПРИГОДНАЯ к оплате прямо сейчас:
// тот же гейт, что и на checkout (Redeemable — код жив, не исчерпан, юзер его не
// гасил). Возвращает 0, если скидки нет или она уже не сработает — чтобы превью
// (список/карточка/сумма к оплате) не обещало скидку, которой не будет на форме
// оплаты. Best-effort: при ошибке БД скидку показываем (как checkout её бы применил).
func (b *Bot) usablePendingDiscount(ctx context.Context, userID int64) int {
	if b.discounts == nil {
		return 0
	}
	d, ok, err := b.discounts.Get(ctx, userID)
	if err != nil || !ok || d.Pct <= 0 {
		return 0
	}
	if b.promoRepo != nil {
		if usable, err := b.promoRepo.Redeemable(ctx, d.CodeID, userID); err == nil && !usable {
			return 0
		}
	}
	return d.Pct
}

// subSupported — текущий провайдер умеет автосписания (показывать ли подписку).
func (b *Bot) subSupported() bool {
	return b.payments != nil && b.payments.SupportsSubscription()
}

// sendSubConsent — экран явного согласия на подписку перед оплатой: сумма,
// период, автопродление, как отменить. Согласие фиксируем при создании платежа.
func (b *Bot) sendSubConsent(ctx context.Context, tgID, chatID int64, messageID int, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.SubPriceRub <= 0 || !b.subSupported() {
		b.sendPlanCard(ctx, tgID, chatID, messageID, name)
		return
	}
	text := fmt.Sprintf(
		"🔁 <b>Подписка на тариф %s</b>\n\n"+
			"• Стоимость: <b>%d ₽ каждые 30 дней</b>\n"+
			"• Оплата списывается <b>автоматически</b> с твоей карты в конце периода через Робокассу\n"+
			"• Отменить автопродление можно в любой момент в разделе <b>«Мой тариф»</b> — "+
			"доступ сохранится до конца оплаченного периода\n"+
			"• Перед каждым списанием пришлём напоминание\n\n"+
			"Нажимая «Подключить подписку», ты даёшь согласие на регулярные автоматические "+
			"списания и на обработку персональных данных и принимаешь условия "+
			"<a href=\"https://tryberry.ru/#offer\">публичной оферты</a>.",
		p.Title, p.SubPriceRub)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Подключить подписку", "plan:subok:"+name),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Назад", "plan:view:"+name),
		),
	)
	b.showView(chatID, messageID, text, keyboard)
}

// handlePlanBuy — старт оплаты тарифа. Если email для чека 54-ФЗ ещё не задан —
// просим его (один раз), иначе сразу создаём платёж. Без сервиса (payments==nil)
// — заглушка как раньше.
func (b *Bot) handlePlanBuy(ctx context.Context, telegramID, chatID int64, messageID int, name string) {
	if b.payments == nil {
		b.sendPlanBuyStub(ctx, telegramID, chatID, messageID, name)
		return
	}
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlansMenu(ctx, telegramID, chatID, messageID)
		return
	}

	user, err := b.userRepo.GetByTelegramID(ctx, telegramID)
	if err != nil {
		b.log.Error("plan buy: load user", "telegram_id", telegramID, "err", err)
		b.showView(chatID, messageID, "⚠️ Не удалось начать оплату. Попробуй ещё раз позже.",
			backToPlansKeyboard())
		return
	}

	email, err := b.userRepo.GetEmail(ctx, user.ID)
	if err != nil {
		b.log.Error("plan buy: get email", "user_id", user.ID, "err", err)
		b.showView(chatID, messageID, "⚠️ Не удалось начать оплату. Попробуй ещё раз позже.",
			backToPlansKeyboard())
		return
	}
	if email == "" {
		// Нет email — просим перед оплатой (с пояснением про чек 54-ФЗ).
		if err := b.setEmailFSM(ctx, telegramID, emailFSM{Plan: name}); err != nil {
			b.log.Error("plan buy: set email fsm", "user_id", user.ID, "err", err)
		}
		amount := b.previewAmount(ctx, user.ID, int64(p.PriceRub)*100)
		b.showView(chatID, messageID, emailRequestText(p, amount), backToPlansKeyboard())
		return
	}

	b.startCheckout(ctx, user, chatID, messageID, name, email)
}

// previewAmount — сумма к оплате с учётом ожидающей скидки (для текста запроса
// email, до создания платежа). Через тот же гейт, что список/карточка/checkout,
// чтобы не показать заниженную сумму по уже негодному коду.
func (b *Bot) previewAmount(ctx context.Context, userID int64, full int64) int64 {
	if pct := b.usablePendingDiscount(ctx, userID); pct > 0 {
		return domain.DiscountedKopecks(full, pct)
	}
	return full
}

// startCheckout — создаёт платёж ЮKassa и показывает кнопку перехода на оплату.
func (b *Bot) startCheckout(ctx context.Context, user *domain.User, chatID int64, messageID int, name, email string) {
	checkout, err := b.payments.Start(ctx, user, name, email)
	if err != nil {
		b.log.Error("plan buy: start checkout", "user_id", user.ID, "plan", name, "err", err)
		b.showView(chatID, messageID,
			"⚠️ Не удалось создать платёж. Попробуй ещё раз позже или напиши в поддержку.",
			backToPlansKeyboard())
		return
	}
	p, _ := domain.PlanByName(name)

	var sb strings.Builder
	fmt.Fprintf(&sb, "💳 <b>Оплата тарифа %s</b>\n\n", p.Title)
	if checkout.DiscountPct > 0 {
		fmt.Fprintf(&sb, "Скидка по промокоду: <b>%d%%</b>\n", checkout.DiscountPct)
	}
	fmt.Fprintf(&sb, "К оплате: <b>%s ₽</b> — подписка на %d дней.\n\n",
		domain.KopecksToRubString(checkout.AmountKopecks), domain.PurchaseDays)
	sb.WriteString("Нажми «Перейти к оплате» — откроется форма ЮKassa (карта, СБП, SberPay). " +
		"Тариф подключится автоматически сразу после оплаты.")

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("💳 Перейти к оплате", checkout.ConfirmationURL),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ К тарифам", "menu:plans"),
			tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
		),
	)
	b.showView(chatID, messageID, sb.String(), keyboard)
}

// handleSubBuy — старт подписки (после экрана согласия). Логика email как у
// разовой оплаты, но дальше идёт StartSubscription.
func (b *Bot) handleSubBuy(ctx context.Context, telegramID, chatID int64, messageID int, name string) {
	if !b.subSupported() {
		b.handlePlanBuy(ctx, telegramID, chatID, messageID, name)
		return
	}
	p, ok := domain.PlanByName(name)
	if !ok || p.SubPriceRub <= 0 {
		b.sendPlansMenu(ctx, telegramID, chatID, messageID)
		return
	}

	user, err := b.userRepo.GetByTelegramID(ctx, telegramID)
	if err != nil {
		b.log.Error("sub buy: load user", "telegram_id", telegramID, "err", err)
		b.showView(chatID, messageID, "⚠️ Не удалось начать оплату. Попробуй ещё раз позже.", backToPlansKeyboard())
		return
	}

	email, err := b.userRepo.GetEmail(ctx, user.ID)
	if err != nil {
		b.log.Error("sub buy: get email", "user_id", user.ID, "err", err)
		b.showView(chatID, messageID, "⚠️ Не удалось начать оплату. Попробуй ещё раз позже.", backToPlansKeyboard())
		return
	}
	if email == "" {
		if err := b.setEmailFSM(ctx, telegramID, emailFSM{Plan: name, Sub: true}); err != nil {
			b.log.Error("sub buy: set email fsm", "user_id", user.ID, "err", err)
		}
		amount := b.previewAmount(ctx, user.ID, int64(p.SubPriceRub)*100)
		b.showView(chatID, messageID, emailRequestText(p, amount), backToPlansKeyboard())
		return
	}

	b.startSubCheckout(ctx, user, chatID, messageID, name, email)
}

// startSubCheckout — создаёт первый платёж подписки и показывает ссылку оплаты
// вместе с условиями автопродления.
func (b *Bot) startSubCheckout(ctx context.Context, user *domain.User, chatID int64, messageID int, name, email string) {
	checkout, err := b.payments.StartSubscription(ctx, user, name, email, "tg")
	if err != nil {
		b.log.Error("sub buy: start checkout", "user_id", user.ID, "plan", name, "err", err)
		b.showView(chatID, messageID,
			"⚠️ Не удалось создать платёж. Попробуй ещё раз позже или напиши в поддержку.",
			backToPlansKeyboard())
		return
	}
	p, _ := domain.PlanByName(name)

	var sb strings.Builder
	fmt.Fprintf(&sb, "🔁 <b>Подписка на тариф %s</b>\n\n", p.Title)
	if checkout.DiscountPct > 0 {
		fmt.Fprintf(&sb, "Скидка по промокоду на первый платёж: <b>%d%%</b>\n", checkout.DiscountPct)
	}
	fmt.Fprintf(&sb, "Первый платёж: <b>%s ₽</b>\n", domain.KopecksToRubString(checkout.AmountKopecks))
	fmt.Fprintf(&sb, "Далее: <b>%s ₽ каждые %d дней</b> (автосписание)\n\n",
		domain.KopecksToRubString(checkout.RenewalKopecks), domain.PurchaseDays)
	sb.WriteString("Нажми «Перейти к оплате» и подтверди оплату. Тариф подключится автоматически. " +
		"Отменить автопродление — в разделе «Мой тариф».")

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("💳 Перейти к оплате", checkout.ConfirmationURL),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ К тарифам", "menu:plans"),
			tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
		),
	)
	b.showView(chatID, messageID, sb.String(), keyboard)
}

func backToPlansKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ К тарифам", "menu:plans"),
		),
	)
}

// sendPlanBuyStub — экран оплаты-заглушки: ЮKassa ещё на подключении,
// подписка не выдаётся.
func (b *Bot) sendPlanBuyStub(ctx context.Context, tgID, chatID int64, messageID int, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlansMenu(ctx, tgID, chatID, messageID)
		return
	}

	text := fmt.Sprintf(
		"💳 <b>Оплата: тариф %s</b>\n\n"+
			"К оплате: <b>%d ₽</b> — подписка на 30 дней.\n\n"+
			"Оплата проходит через ЮKassa: банковская карта, СБП, SberPay.\n\n"+
			"⏳ Платёжный сервис сейчас подключается — кнопка оплаты появится здесь в ближайшие дни. "+
			"Хочешь подключить тариф уже сейчас — напиши в поддержку 👇",
		p.Title, p.PriceRub)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("👨‍💻 Поддержка", "https://t.me/kosov_andrey"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ К тарифам", "menu:plans"),
			tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
		),
	)
	b.showView(chatID, messageID, text, keyboard)
}
