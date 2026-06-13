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
func (b *Bot) sendPlansMenu(chatID int64, messageID int) {
	var sb strings.Builder
	sb.WriteString("💳 <b>Тарифы TryberryBot</b>\n\n" +
		"Подписка открывает больше отслеживаемых товаров, поиск-подписки и частые проверки цен. Действует 30 дней с момента оплаты.\n\n")

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, sp := range domain.PlanShowcase {
		p, ok := domain.PlanByName(sp.Name)
		if !ok {
			continue
		}
		fmt.Fprintf(&sb, "▫️ <b>%s</b> — %d ₽/мес · %s\n", p.Title, p.PriceRub, sp.Tagline)
		fmt.Fprintf(&sb, "    📦 %d товаров · 🔎 %d поисков · ⏱ %s\n\n",
			p.MaxProduct, p.MaxSearch, domain.IntervalPhrase(p.Interval))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("%s — %d ₽/мес", p.Title, p.PriceRub),
				"plan:view:"+p.Name,
			),
		))
	}

	sb.WriteString("Бесплатный тариф Free — 5 товаров, без поиск-подписок. Новым пользователям доступен триал поиска: /trial.")

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
	))
	b.showView(chatID, messageID, sb.String(), tgbotapi.NewInlineKeyboardMarkup(rows...))
}

// sendPlanCard — карточка тарифа: описание, состав, цена, кнопка оплаты.
func (b *Bot) sendPlanCard(chatID int64, messageID int, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlansMenu(chatID, messageID)
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
	fmt.Fprintf(&sb, "💳 <b>Тариф %s</b> — %d ₽/мес\n", p.Title, p.PriceRub)
	if tagline != "" {
		fmt.Fprintf(&sb, "<i>%s</i>\n", tagline)
	}
	sb.WriteString("\n<b>Что входит:</b>\n")
	fmt.Fprintf(&sb, "📦 До %d отслеживаемых товаров\n", p.MaxProduct)
	fmt.Fprintf(&sb, "🔎 До %d поиск-подписок (слежу за всей поисковой выдачей)\n", p.MaxSearch)
	fmt.Fprintf(&sb, "⏱ Проверка цен %s\n", domain.IntervalPhrase(p.Interval))
	sb.WriteString("🔔 Уведомления о снижении цены в Telegram и VK\n\n")
	sb.WriteString("Подписка действует <b>30 дней</b> с момента оплаты.")

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("💳 Оплатить %d ₽", p.PriceRub),
				"plan:buy:"+p.Name,
			),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ К тарифам", "menu:plans"),
		),
	)
	b.showView(chatID, messageID, sb.String(), keyboard)
}

// handlePlanBuy — старт оплаты тарифа. Если email для чека 54-ФЗ ещё не задан —
// просим его (один раз), иначе сразу создаём платёж. Без сервиса (payments==nil)
// — заглушка как раньше.
func (b *Bot) handlePlanBuy(ctx context.Context, telegramID, chatID int64, messageID int, name string) {
	if b.payments == nil {
		b.sendPlanBuyStub(chatID, messageID, name)
		return
	}
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlansMenu(chatID, messageID)
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
// email, до создания платежа). Ошибки игнорируем — покажем полную цену.
func (b *Bot) previewAmount(ctx context.Context, userID int64, full int64) int64 {
	if b.discounts == nil {
		return full
	}
	if d, ok, err := b.discounts.Get(ctx, userID); err == nil && ok && d.Pct > 0 {
		return domain.DiscountedKopecks(full, d.Pct)
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

func backToPlansKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ К тарифам", "menu:plans"),
		),
	)
}

// sendPlanBuyStub — экран оплаты-заглушки: ЮKassa ещё на подключении,
// подписка не выдаётся.
func (b *Bot) sendPlanBuyStub(chatID int64, messageID int, name string) {
	p, ok := domain.PlanByName(name)
	if !ok || p.PriceRub <= 0 {
		b.sendPlansMenu(chatID, messageID)
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
