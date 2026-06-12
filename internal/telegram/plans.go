package telegram

import (
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Витрина тарифов: порядок и слоганы — здесь, цифры (цены/лимиты/интервалы) —
// в domain.Plans. Кнопка «Оплатить» пока ведёт на заглушку: платёжный сервис
// (ЮKassa) на подключении, подписки НЕ выдаются. После запуска оплаты
// plan:buy:* заменяется на создание платежа.
var showcasePlans = []struct {
	name    string
	tagline string
}{
	{"lite", "следить за своими покупками"},
	{"pro", "большие списки и быстрые проверки"},
	{"reseller_start", "для перекупов: проверка раз в минуту"},
	{"reseller_pro", "максимум скорости и объёма"},
}

// intervalPhrase — «каждую минуту / каждые 15 минут / каждый час».
func intervalPhrase(d time.Duration) string {
	m := int(d.Minutes())
	switch {
	case m <= 1:
		return "каждую минуту"
	case m == 60:
		return "каждый час"
	default:
		return fmt.Sprintf("каждые %d минут", m)
	}
}

// sendPlansMenu — список тарифов с ценами. messageID != 0 → в том же сообщении.
func (b *Bot) sendPlansMenu(chatID int64, messageID int) {
	var sb strings.Builder
	sb.WriteString("💳 <b>Тарифы TryberryBot</b>\n\n" +
		"Подписка открывает больше отслеживаемых товаров, поиск-подписки и частые проверки цен. Действует 30 дней с момента оплаты.\n\n")

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, sp := range showcasePlans {
		p, ok := domain.PlanByName(sp.name)
		if !ok {
			continue
		}
		fmt.Fprintf(&sb, "▫️ <b>%s</b> — %d ₽/мес · %s\n", p.Title, p.PriceRub, sp.tagline)
		fmt.Fprintf(&sb, "    📦 %d товаров · 🔎 %d поисков · ⏱ %s\n\n",
			p.MaxProduct, p.MaxSearch, intervalPhrase(p.Interval))
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
	for _, sp := range showcasePlans {
		if sp.name == name {
			tagline = sp.tagline
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
	fmt.Fprintf(&sb, "⏱ Проверка цен %s\n", intervalPhrase(p.Interval))
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
