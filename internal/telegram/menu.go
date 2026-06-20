package telegram

import (
	"context"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ── Главное меню ──────────────────────────────────────────────────────────────
// mainMenuKeyboard — клавиатура главного меню. Вынесена отдельно, чтобы
// приветственные экраны (рефералка) могли показать её со своим текстом.
func mainMenuKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ Добавить товар", "menu:add"),
			tgbotapi.NewInlineKeyboardButtonData("🔎 Поиск по ссылке", "menu:search"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Мои товары", "menu:list"),
			tgbotapi.NewInlineKeyboardButtonData("📡 Мои поиски", "menu:lsearch"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💳 Тарифы и подписка", "menu:plans"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👤 Профиль", "menu:profile"),
			tgbotapi.NewInlineKeyboardButtonData("🎁 Триал", "menu:trial"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👥 Пригласить друга", "menu:ref"),
			tgbotapi.NewInlineKeyboardButtonData("🎟 Промокод", "menu:promo"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❓ Помощь", "menu:help"),
			tgbotapi.NewInlineKeyboardButtonURL("👨‍💻 Поддержка", "https://t.me/kosov_andrey"),
		),
	)
}

// backToMenuKeyboard — единственная кнопка «◀️ В меню» для вложенных экранов.
func backToMenuKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
		),
	)
}

func (b *Bot) sendMainMenu(ctx context.Context, chatID int64, messageID int, edit bool) {
	text := "🍓 <b>TryberryBot</b>\n\n" +
		"Слежу за ценами на Wildberries и уведомляю, когда цена снижается.\n\n" +
		"Выбери раздел:"
	if !edit {
		messageID = 0
	}
	b.showView(chatID, messageID, text, mainMenuKeyboard())
}

func (b *Bot) sendAddMenu(chatID int64, messageID int) {
	text := "➕ <b>Добавить товар</b>\n\n" +
		"Просто отправь мне ссылку на товар с Wildberries — я сразу начну отслеживать.\n\n" +
		"Пример ссылки:\n" +
		"<code>https://www.wildberries.ru/catalog/252334498/detail.aspx</code>\n\n" +
		"Или используй команду:\n" +
		"<code>/track &lt;ссылка&gt;</code>"

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Назад", "menu:main"),
		),
	)
	b.editMenu(chatID, messageID, text, keyboard)
}

func (b *Bot) sendHelpMenu(chatID int64, messageID int, edit bool) {
	text := "❓ <b>Помощь</b>\n\n" +
		"<b>Как добавить товар:</b>\n" +
		"Просто отправь ссылку с Wildberries прямо в чат — без команд.\n\n" +
		"<b>Поиск по ссылке:</b>\n" +
		"Отправь ссылку на поисковую выдачу WB (с параметром поиска) — выберешь тип уведомления, и я буду следить за всей выдачей.\n\n" +
		"<b>Как работают уведомления:</b>\n" +
		"Цены проверяются регулярно. Когда цена падает — получишь уведомление.\n\n" +
		"<b>Команды:</b>\n" +
		"<code>/track &lt;ссылка&gt;</code> — добавить товар\n" +
		"<code>/list</code> — мои подписки\n" +
		"<code>/track_search &lt;ссылка&gt;</code> — отслеживать поиск\n" +
		"<code>/list_search</code> — мои поиск-подписки\n" +
		"<code>/plans</code> — тарифы и подписка\n" +
		"<code>/promo КОД</code> — активировать промокод\n" +
		"<code>/ref</code> — пригласить друга\n" +
		"<code>/myplan</code> — мой тариф и лимиты\n" +
		"<code>/menu</code> — главное меню\n\n" +
		"<b>Вопросы и предложения — пиши разработчику 👇</b>"

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("👨‍💻 @kosov_andrey", "https://t.me/kosov_andrey"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Назад", "menu:main"),
		),
	)

	if edit && messageID != 0 {
		b.editMenu(chatID, messageID, text, keyboard)
	} else {
		m := tgbotapi.NewMessage(chatID, text)
		m.ParseMode = "HTML"
		m.ReplyMarkup = keyboard
		b.send(m)
	}
}
