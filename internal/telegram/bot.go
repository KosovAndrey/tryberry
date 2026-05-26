package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

type Bot struct {
	api      *tgbotapi.BotAPI
	log      *slog.Logger
	userRepo *postgres.UserRepo
	subRepo  *postgres.SubscriptionRepo
	prodRepo *postgres.ProductRepo
	registry *scraper.Registry
}

func NewBot(
	token string,
	log *slog.Logger,
	userRepo *postgres.UserRepo,
	subRepo *postgres.SubscriptionRepo,
	prodRepo *postgres.ProductRepo,
	registry *scraper.Registry,
) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("init bot api: %w", err)
	}
	return &Bot{
		api:      api,
		log:      log,
		userRepo: userRepo,
		subRepo:  subRepo,
		prodRepo: prodRepo,
		registry: registry,
	}, nil
}

func (b *Bot) SetWebhook(webhookURL string) error {
	wh, err := tgbotapi.NewWebhook(webhookURL)
	if err != nil {
		return fmt.Errorf("new webhook: %w", err)
	}
	_, err = b.api.Request(wh)
	return err
}

func (b *Bot) SetCommands() error {
	commands := []tgbotapi.BotCommand{
		{Command: "start", Description: "Главное меню"},
		{Command: "menu", Description: "Открыть меню"},
		{Command: "track", Description: "Добавить товар — /track <ссылка>"},
		{Command: "list", Description: "Мои подписки"},
		{Command: "help", Description: "Помощь"},
	}
	cfg := tgbotapi.NewSetMyCommands(commands...)
	_, err := b.api.Request(cfg)
	return err
}

// HandleUpdate — точка входа для всех update
func (b *Bot) HandleUpdate(ctx context.Context, update tgbotapi.Update) {
	switch {
	case update.CallbackQuery != nil:
		b.handleCallback(ctx, update.CallbackQuery)
	case update.Message != nil && update.Message.IsCommand():
		b.handleCommand(ctx, update.Message)
	case update.Message != nil:
		b.handleMessage(ctx, update.Message)
	}
}

// ── Обработка обычных сообщений (не команд) ───────────────────────────────────

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	text := strings.TrimSpace(msg.Text)

	// Проверяем через registry — поддерживается ли этот маркетплейс
	if _, err := b.registry.FindByURL(text); err == nil {
		user, err := b.userRepo.Upsert(ctx, msg.From.ID, msg.From.UserName)
		if err != nil {
			b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
			return
		}
		b.doTrack(ctx, msg.Chat.ID, text, user)
		return
	}

	b.sendMainMenu(ctx, msg.Chat.ID, 0, false)
}

// ── Commands ──────────────────────────────────────────────────────────────────

func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	user, err := b.userRepo.Upsert(ctx, msg.From.ID, msg.From.UserName)
	if err != nil {
		b.log.Error("upsert user", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}

	switch msg.Command() {
	case "start":
		b.sendMainMenu(ctx, msg.Chat.ID, 0, false)
	case "menu":
		b.sendMainMenu(ctx, msg.Chat.ID, 0, false)
	case "track":
		args := strings.TrimSpace(msg.CommandArguments())
		if args == "" {
			b.reply(msg.Chat.ID,
				"Отправь ссылку на товар Wildberries — просто вставь её в чат, без команды.\n\n"+
					"Или используй:\n<code>/track https://www.wildberries.ru/catalog/.../detail.aspx</code>")
			return
		}
		b.doTrack(ctx, msg.Chat.ID, args, user)
	case "list":
		b.handleList(ctx, msg.Chat.ID, user)
	case "help":
		b.sendHelpMenu(msg.Chat.ID, 0, false)
	case "untrack":
		b.handleUntrack(ctx, msg)
	default:
		b.reply(msg.Chat.ID, "Неизвестная команда. Напиши /menu")
	}
}

// ── Главное меню ──────────────────────────────────────────────────────────────

func (b *Bot) sendMainMenu(ctx context.Context, chatID int64, messageID int, edit bool) {
	text := "🍓 <b>TryberryBot</b>\n\n" +
		"Слежу за ценами на Wildberries и уведомляю когда цена снижается.\n\n" +
		"Выбери раздел:"

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Мои подписки", "menu:list"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ Добавить товар", "menu:add"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❓ Помощь", "menu:help"),
			tgbotapi.NewInlineKeyboardButtonURL("👨‍💻 Поддержка", "https://t.me/kosov_andrey"),
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
		"<b>Как работают уведомления:</b>\n" +
		"Цены проверяются каждые 15 минут. Когда цена падает ниже той что была при подписке — " +
		"получишь уведомление с фото товара и кнопками управления.\n\n" +
		"<b>Команды:</b>\n" +
		"<code>/track &lt;ссылка&gt;</code> — добавить товар\n" +
		"<code>/list</code> — мои подписки\n" +
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

// ── Подписки ──────────────────────────────────────────────────────────────────

func (b *Bot) handleList(ctx context.Context, chatID int64, user *domain.User) {
	subs, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("get subscriptions", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	if len(subs) == 0 {
		m := tgbotapi.NewMessage(chatID,
			"📋 У тебя пока нет активных подписок.\n\n"+
				"Отправь ссылку на товар Wildberries прямо в чат — я начну отслеживать цену.",
		)
		m.ParseMode = "HTML"
		m.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
			),
		)
		b.send(m)
		return
	}

	text, keyboard := b.buildListView(subs)
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	m.ReplyMarkup = keyboard
	b.send(m)
}

func (b *Bot) buildListView(subs []*domain.Subscription) (string, tgbotapi.InlineKeyboardMarkup) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 <b>Твои подписки — %d активных</b>\n\n", len(subs))

	for i, sub := range subs {
		priceEmoji := ""
		if sub.CurrentPrice > 0 && sub.CurrentPrice < sub.BaselinePrice {
			priceEmoji = "📉 "
		}
		currentPriceStr := "нет данных"
		if sub.CurrentPrice > 0 {
			currentPriceStr = fmt.Sprintf("%s%.0f ₽", priceEmoji, sub.CurrentPrice)
		}

		fmt.Fprintf(&sb, "%d. %s <b>%s</b>\n   сейчас %s  |  при подписке %.0f ₽\n\n",
			i+1, marketplaceIcon(sub.ProductMarketplace), sub.ProductName, currentPriceStr, sub.BaselinePrice,
		)
	}

	// Кнопки: ссылка + отмена для каждой подписки
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, sub := range subs {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL(
				fmt.Sprintf("🔗 #%d %s", i+1, truncate(sub.ProductName, 20)),
				sub.ProductURL,
			),
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("❌ Отменить #%d", i+1),
				fmt.Sprintf("untrack:%d", sub.ID),
			),
		))
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
	))

	return sb.String(), tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (b *Bot) handleUntrack(ctx context.Context, msg *tgbotapi.Message) {
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		b.reply(msg.Chat.ID, "Укажи номер подписки из /list.\nПример: /untrack 3")
		return
	}
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		b.reply(msg.Chat.ID, "Номер подписки должен быть числом.")
		return
	}
	if err := b.subRepo.Deactivate(ctx, id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.reply(msg.Chat.ID, "Подписка не найдена.")
			return
		}
		b.log.Error("deactivate subscription", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}
	b.reply(msg.Chat.ID, "✅ Отслеживание отменено.")
}

// ── doTrack — основная логика добавления товара ───────────────────────────────

func (b *Bot) doTrack(ctx context.Context, chatID int64, rawURL string, user *domain.User) {
	tracer := otel.Tracer("bot")
	ctx, span := tracer.Start(ctx, "bot.handleTrack",
		trace.WithAttributes(
			attribute.String("url", rawURL),
			attribute.Int64("user.id", user.ID),
			attribute.Int64("chat.id", chatID),
		),
	)
	defer span.End()

	// Находим подходящий скрейпер
	s, err := b.registry.FindByURL(rawURL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "no scraper matches URL")
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		// ... остальной код без изменений
		supported := b.registry.SupportedMarketplaces()
		b.reply(chatID, fmt.Sprintf(
			"Не могу распознать ссылку.\n\nПоддерживаемые маркетплейсы: %v\n\n"+
				"Пример ссылки:\n<code>https://www.wildberries.ru/catalog/123456789/detail.aspx</code>",
			supported,
		))
		return
	}

	span.SetAttributes(attribute.String("marketplace", string(s.Marketplace())))

	wait := tgbotapi.NewMessage(chatID, "⏳ Получаю данные о товаре...")
	wait.ParseMode = "HTML"
	sent, _ := b.api.Send(wait)

	result, err := s.Scrape(ctx, rawURL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "scrape failed")
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		b.log.Error("scrape on track", "url", rawURL, "marketplace", s.Marketplace(), "err", err)

		msg := "❌ Не удалось получить данные о товаре. Попробуй позже."
		if errors.Is(err, scraper.ErrNotImplemented) {
			msg = fmt.Sprintf("⚠️ Маркетплейс <b>%s</b> пока не поддерживается. Сейчас доступен только Wildberries.", s.Marketplace())
		}

		edit := tgbotapi.NewEditMessageText(chatID, sent.MessageID, msg)
		edit.ParseMode = "HTML"
		b.api.Send(edit) //nolint:errcheck
		return
	}

	span.SetAttributes(
		attribute.String("product.name", result.Name),
		attribute.Float64("product.price", result.Price),
	)

	product, err := b.prodRepo.Upsert(ctx, rawURL, result.Name, result.ImageURL, string(s.Marketplace()))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upsert product failed")
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		b.log.Error("upsert product", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	_, created, err := b.subRepo.Upsert(ctx, user.ID, product.ID, result.Price)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upsert subscription failed")
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		b.log.Error("upsert subscription", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	span.SetAttributes(attribute.Bool("subscription.created", created))

	// ... остальной код responseText без изменений
	var responseText string
	if created {
		metrics.TrackCommands.WithLabelValues("success").Inc()
		responseText = fmt.Sprintf(
			"✅ <b>Добавил в отслеживание!</b>\n\n"+
				"<b>%s</b>\n"+
				"💰 Текущая цена: <b>%.0f ₽</b>\n\n"+
				"Уведомлю когда цена снизится 🔔",
			result.Name, result.Price,
		)
	} else {
		metrics.TrackCommands.WithLabelValues("reactivated").Inc()
		responseText = fmt.Sprintf(
			"🔄 <b>Отслеживание возобновлено!</b>\n\n"+
				"<b>%s</b>\n"+
				"💰 Текущая цена: <b>%.0f ₽</b>",
			result.Name, result.Price,
		)
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Мои подписки", "menu:list"),
			tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
		),
	)

	edit := tgbotapi.NewEditMessageText(chatID, sent.MessageID, responseText)
	edit.ParseMode = "HTML"
	edit.ReplyMarkup = &keyboard
	b.api.Send(edit) //nolint:errcheck
}

// ── Callbacks ─────────────────────────────────────────────────────────────────

func (b *Bot) handleCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	b.answerCallback(cb.ID, "")

	chatID := cb.Message.Chat.ID
	messageID := cb.Message.MessageID

	switch {
	case cb.Data == "menu:main":
		b.sendMainMenu(ctx, chatID, messageID, true)

	case cb.Data == "menu:list":
		user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
		if err != nil {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		subs, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
		if err != nil {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		if len(subs) == 0 {
			b.editMenu(chatID, messageID,
				"📋 У тебя пока нет активных подписок.\n\n"+
					"Просто отправь ссылку на товар Wildberries прямо в чат.",
				tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
					),
				),
			)
			return
		}
		text, keyboard := b.buildListView(subs)
		b.editMenu(chatID, messageID, text, keyboard)

	case cb.Data == "menu:add":
		b.sendAddMenu(chatID, messageID)

	case cb.Data == "menu:help":
		b.sendHelpMenu(chatID, messageID, true)

	case strings.HasPrefix(cb.Data, "untrack:"):
		b.callbackUntrack(ctx, cb)

	case strings.HasPrefix(cb.Data, "keep:"):
		b.answerCallback(cb.ID, "Продолжаем следить 👍")
	}
}

func (b *Bot) callbackUntrack(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	idStr := strings.TrimPrefix(cb.Data, "untrack:")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}

	if err := b.subRepo.Deactivate(ctx, id); err != nil && !errors.Is(err, domain.ErrNotFound) {
		b.log.Error("deactivate via callback", "err", err)
		b.answerCallback(cb.ID, "Ошибка, попробуй позже")
		return
	}

	// Обновляем список подписок в том же сообщении
	user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
	if err == nil {
		subs, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
		if err == nil && len(subs) > 0 {
			text, keyboard := b.buildListView(subs)
			b.editMenu(cb.Message.Chat.ID, cb.Message.MessageID, text, keyboard)
			b.answerCallback(cb.ID, "✅ Отслеживание отменено")
			return
		}
	}

	// Подписок не осталось — показываем меню
	b.sendMainMenu(ctx, cb.Message.Chat.ID, cb.Message.MessageID, true)
	b.answerCallback(cb.ID, "✅ Отслеживание отменено")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (b *Bot) editMenu(chatID int64, messageID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = &keyboard
	if _, err := b.api.Send(msg); err != nil {
		b.log.Error("edit message", "err", err)
	}
}

func (b *Bot) reply(chatID int64, text string) {
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	b.send(m)
}

func (b *Bot) send(c tgbotapi.Chattable) {
	if _, err := b.api.Send(c); err != nil {
		b.log.Error("telegram send", "err", err)
	}
}

func (b *Bot) answerCallback(callbackID, text string) {
	answer := tgbotapi.NewCallback(callbackID, text)
	if _, err := b.api.Request(answer); err != nil {
		b.log.Error("answer callback", "err", err)
	}
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

func marketplaceIcon(mp string) string {
	switch mp {
	case "wildberries":
		return "🟣"
	case "yandex_market":
		return "🟡"
	case "ozon":
		return "🔵"
	default:
		return "📦"
	}
}
