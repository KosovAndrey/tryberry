package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

type Bot struct {
	api      *tgbotapi.BotAPI
	log      *slog.Logger
	userRepo *postgres.UserRepo
	subRepo  *postgres.SubscriptionRepo
	prodRepo *postgres.ProductRepo
	registry *scraper.Registry

	// Поиск-подписки
	searchQueryRepo *postgres.SearchQueryRepo
	searchSubRepo   *postgres.SearchSubscriptionRepo
	rdb             *redis.Client // FSM для ввода порога (может быть nil)

	promoRepo    *postgres.PromoRepo
	referralRepo *postgres.ReferralRepo

	linkCodes *redisrepo.LinkCodeStore // коды привязки VK (nil, если redis недоступен)
	vkBotURL  string                   // ссылка на VK-бота для кнопки привязки ("" — не показывать)

	// Оплата. payments == nil → платёжный сервис не настроен (env пуст),
	// витрина показывает заглушку. discounts хранит «ожидающую скидку» (nil без redis).
	// billing — рекуррентные подписки (статус/отмена); nil без оплаты.
	payments  *payment.Service
	discounts *redisrepo.DiscountStore
	billing   *postgres.BillingSubscriptionRepo

	adminIDs map[int64]bool // кто может выдавать тарифы
}

// SetPayments подключает платёжный сервис (опционально: при пустом конфиге не
// вызывается, и витрина показывает заглушку оплаты).
func (b *Bot) SetPayments(svc *payment.Service) {
	b.payments = svc
}

// SetBilling подключает репозиторий подписок (для экрана «Моя подписка» и отмены).
func (b *Bot) SetBilling(repo *postgres.BillingSubscriptionRepo) {
	b.billing = repo
}

func NewBot(
	token string,
	log *slog.Logger,
	userRepo *postgres.UserRepo,
	subRepo *postgres.SubscriptionRepo,
	prodRepo *postgres.ProductRepo,
	registry *scraper.Registry,
	searchQueryRepo *postgres.SearchQueryRepo,
	searchSubRepo *postgres.SearchSubscriptionRepo,
	promoRepo *postgres.PromoRepo,
	referralRepo *postgres.ReferralRepo,
	rdb *redis.Client,
	adminIDs map[int64]bool,
	vkBotURL string,
) (*Bot, error) {
	// Тот же таймаут-клиент, что у Receiver: ответы bot-worker и (в монолитном
	// режиме) Bot.RunPolling ходят к Telegram через HTTPS_PROXY — без таймаута
	// запрос по мёртвому keep-alive соединению к прокси висит до idle-таймаута
	// tinyproxy. См. pollHTTPClient в receiver.go.
	api, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, pollHTTPClient())
	if err != nil {
		return nil, fmt.Errorf("init bot api: %w", err)
	}
	var linkCodes *redisrepo.LinkCodeStore
	var discounts *redisrepo.DiscountStore
	if rdb != nil {
		linkCodes = redisrepo.NewLinkCodeStore(rdb)
		discounts = redisrepo.NewDiscountStore(rdb)
	}
	return &Bot{
		api:             api,
		log:             log,
		userRepo:        userRepo,
		subRepo:         subRepo,
		prodRepo:        prodRepo,
		registry:        registry,
		searchQueryRepo: searchQueryRepo,
		searchSubRepo:   searchSubRepo,
		promoRepo:       promoRepo,
		referralRepo:    referralRepo,
		rdb:             rdb,
		adminIDs:        adminIDs,
		linkCodes:       linkCodes,
		discounts:       discounts,
		vkBotURL:        vkBotURL,
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
		{Command: "track_search", Description: "Отслеживать поиск — /track_search <ссылка>"},
		{Command: "list_search", Description: "Мои поиск-подписки"},
		{Command: "plans", Description: "💳 Тарифы и подписка"},
		{Command: "trial", Description: "🎁 Триал поиска (3 дня)"},
		{Command: "promo", Description: "🎟 Активировать промокод"},
		{Command: "ref", Description: "👥 Пригласить друга"},
		{Command: "profile", Description: "👤 Профиль"},
		{Command: "myplan", Description: "Мой тариф и лимиты"},
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

	user, err := b.userRepo.Upsert(ctx, msg.From.ID, msg.From.UserName)
	if err != nil {
		b.log.Error("upsert user", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}

	// 1a0. Ждём ли email для чека 54-ФЗ перед оплатой?
	if fsm, ok := b.getEmailFSM(ctx, msg.From.ID); ok {
		b.handleEmailInput(ctx, msg.Chat.ID, msg.From.ID, text, user, fsm)
		return
	}

	// 1a. Ждём ли число (порог/процент) для ТОВАРНОЙ подписки?
	if fsm, ok := b.getTrackFSM(ctx, msg.From.ID); ok {
		b.handleTrackThreshold(ctx, msg.Chat.ID, msg.From.ID, text, fsm)
		return
	}

	// 1b. Ждём ли мы от пользователя число (порог/процент) для поиск-подписки?
	if fsm, ok := b.getSearchFSM(ctx, msg.From.ID); ok {
		b.handleSearchThreshold(ctx, msg.Chat.ID, msg.From.ID, text, user, fsm)
		return
	}

	// 1c. Код привязки, выданный в VK-боте («привязать XXXX», направление vk2tg).
	if rest, ok := cutLinkPrefix(text); ok {
		b.handleLinkCode(ctx, msg.Chat.ID, user, msg.From.UserName, rest)
		return
	}

	// 2. Поисковая ссылка WB (?search=...) → флоу поиск-подписки.
	if b.isSearchURL(text) {
		b.startSearchTrack(ctx, msg.Chat.ID, text, user)
		return
	}

	// 3. Ссылка на товар → существующая логика.
	if _, err := b.registry.FindByURL(text); err == nil {
		b.doTrack(ctx, msg.Chat.ID, text, user)
		return
	}

	b.sendMainMenu(ctx, msg.Chat.ID, 0, false)
}

// ── Commands ──────────────────────────────────────────────────────────────────

func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	// Любая команда прерывает незавершённый ввод порога (поиск- и товарных
	// подписок) и ввод email перед оплатой.
	b.clearSearchFSM(ctx, msg.From.ID)
	b.clearTrackFSM(ctx, msg.From.ID)
	b.clearEmailFSM(ctx, msg.From.ID)

	user, err := b.userRepo.Upsert(ctx, msg.From.ID, msg.From.UserName)
	if err != nil {
		b.log.Error("upsert user", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}

	switch msg.Command() {
	case "start":
		// Deep-link payload: t.me/bot?start=promo_XXX | ref_XXX.
		payload := strings.TrimSpace(msg.CommandArguments())
		if code, ok := strings.CutPrefix(payload, "promo_"); ok {
			b.handlePromo(ctx, msg.Chat.ID, user, code)
			return
		}
		if ref, ok := strings.CutPrefix(payload, "ref_"); ok {
			b.handleRefStart(ctx, msg.Chat.ID, user, ref)
			return
		}
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
	case "track_search":
		args := strings.TrimSpace(msg.CommandArguments())
		if args == "" {
			b.reply(msg.Chat.ID,
				"Отправь <b>поисковую</b> ссылку Wildberries прямо в чат (с параметром поиска), и я предложу настроить уведомление.\n\n"+
					"Например:\n<code>https://www.wildberries.ru/catalog/0/search.aspx?search=наушники</code>")
			return
		}
		b.startSearchTrack(ctx, msg.Chat.ID, args, user)
	case "list_search":
		b.handleListSearch(ctx, msg.Chat.ID, user)
	case "plans":
		b.sendPlansMenu(msg.Chat.ID, 0)
	case "trial":
		b.handleTrial(ctx, msg.Chat.ID, 0, user)
	case "promo":
		b.handlePromo(ctx, msg.Chat.ID, user, msg.CommandArguments())
	case "ref":
		b.handleRef(ctx, msg.Chat.ID, 0, user)
	case "profile":
		b.handleProfile(ctx, msg.Chat.ID, 0, user)
	case "promo_create":
		b.handlePromoCreate(ctx, msg)
	case "promo_off":
		b.handlePromoOff(ctx, msg)
	case "promo_list":
		b.handlePromoList(ctx, msg)
	case "myplan":
		b.handleMyPlan(ctx, msg.Chat.ID, 0, user)
	case "grant":
		b.handleGrant(ctx, msg)
	case "revoke":
		b.handleRevoke(ctx, msg)
	case "users":
		b.handleUsers(ctx, msg)
	case "whois":
		b.handleWhois(ctx, msg)
	case "help":
		b.sendHelpMenu(msg.Chat.ID, 0, false)
	case "untrack":
		b.handleUntrack(ctx, msg)
	default:
		b.reply(msg.Chat.ID, "Неизвестная команда. Напиши /menu")
	}
}

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
		if sub.CurrentPrice > 0 && sub.CurrentPrice < sub.FirstSeenPrice {
			priceEmoji = "📉 "
		}
		currentPriceStr := "нет данных"
		if sub.CurrentPrice > 0 {
			currentPriceStr = fmt.Sprintf("%s%.0f ₽", priceEmoji, sub.CurrentPrice)
		}

		fmt.Fprintf(&sb, "%d. %s <b>%s</b>\n   сейчас %s  |  при подписке %.0f ₽\n   %s\n\n",
			i+1, marketplaceIcon(sub.ProductMarketplace), sub.ProductName, currentPriceStr, sub.FirstSeenPrice,
			domain.TriggerDescription(sub.TriggerType, sub.TargetPrice, sub.DiscountPct),
		)
	}

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
	user, err := b.userRepo.GetByTelegramID(ctx, msg.From.ID)
	if err != nil {
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}
	if err := b.subRepo.Deactivate(ctx, id, user.ID); err != nil {
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

// ozonComingSoonMsg — заглушка для ссылок Ozon, пока маркетплейс не запущен
// (антибот FAB требует аккаунт-сессию + RU-мобильный прокси, см. docs/OZON-STATUS.md).
const ozonComingSoonMsg = "🔵 <b>Ozon скоро будет</b> — отслеживание этого маркетплейса ещё в разработке.\n\n" +
	"Пока отслеживаю <b>Wildberries</b> 🟣 — пришли ссылку на товар оттуда."

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

	s, err := b.registry.FindByURL(rawURL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
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
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		b.log.Error("scrape on track", "url", rawURL, "marketplace", s.Marketplace(), "err", err)

		msg := "❌ Не удалось получить данные о товаре. Попробуй позже."
		switch {
		case errors.Is(err, scraper.ErrAgeRestricted):
			// 18+ товар: цена скрыта за возрастным гейтом Ozon — понятное сообщение
			// вместо «не удалось».
			msg = "🔞 Это товар <b>18+</b>. Ozon прячет его цену за подтверждением возраста — пока не могу отслеживать такие товары."
		case s.Marketplace() == scraper.MarketplaceOzon &&
			(errors.Is(err, scraper.ErrNotImplemented) || errors.Is(err, scraper.ErrMarketplaceBlocked)):
			// Заглушка: Ozon ещё в разработке (антибот/прокси). Не пугаем «ошибкой» —
			// показываем понятное «скоро будет». Когда Ozon заработает стабильно,
			// сюда дойдёт обычный успешный путь, и заглушка не сработает.
			msg = ozonComingSoonMsg
		case errors.Is(err, scraper.ErrNotImplemented):
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
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		b.log.Error("upsert product", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	// Лимит тарифа на товарные подписки. Повторная ссылка на уже
	// отслеживаемый товар лимит не расходует (это обновление, не новая).
	plan := user.EffectivePlan(time.Now())
	active, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		span.RecordError(err)
		b.log.Error("count active subs", "err", err)
		edit := tgbotapi.NewEditMessageText(chatID, sent.MessageID, "Произошла ошибка, попробуй позже.")
		edit.ParseMode = "HTML"
		b.api.Send(edit) //nolint:errcheck
		return
	}
	alreadyTracked := false
	for _, sub := range active {
		if sub.ProductID == product.ID {
			alreadyTracked = true
			break
		}
	}
	if !alreadyTracked && len(active) >= plan.MaxProduct {
		metrics.TrackCommands.WithLabelValues("limit").Inc()
		edit := tgbotapi.NewEditMessageText(chatID, sent.MessageID, productLimitText(plan, len(active)))
		edit.ParseMode = "HTML"
		b.api.Send(edit) //nolint:errcheck
		return
	}

	sub, created, err := b.subRepo.Upsert(ctx, user.ID, product.ID, result.Price)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		b.log.Error("upsert subscription", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	span.SetAttributes(attribute.Bool("subscription.created", created))

	var responseText string
	if created {
		metrics.TrackCommands.WithLabelValues("success").Inc()
		responseText = fmt.Sprintf(
			"✅ <b>Добавил в отслеживание!</b>\n\n"+
				"<b>%s</b>\n"+
				"💰 Текущая цена: <b>%.0f ₽</b>\n\n"+
				"🔔 Сейчас уведомлю при <b>любом снижении</b>. Можно сменить тип уведомления кнопками ниже 👇",
			result.Name, result.Price,
		)
	} else {
		metrics.TrackCommands.WithLabelValues("reactivated").Inc()
		responseText = fmt.Sprintf(
			"🔄 <b>Отслеживание возобновлено!</b>\n\n"+
				"<b>%s</b>\n"+
				"💰 Текущая цена: <b>%.0f ₽</b>\n\n"+
				"🔔 Тип уведомления: <b>любое снижение</b>. Сменить — кнопками ниже 👇",
			result.Name, result.Price,
		)
	}

	keyboard := trackTriggerKeyboard(sub.ID, domain.TriggerAnyDrop)

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

	case cb.Data == "menu:search":
		b.sendSearchMenu(chatID, messageID)

	case cb.Data == "menu:lsearch":
		user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
		if err != nil {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		subs, err := b.searchSubRepo.GetActiveByUserID(ctx, user.ID)
		if err != nil {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		if len(subs) == 0 {
			b.editMenu(chatID, messageID,
				"🔎 У тебя пока нет поиск-подписок.\n\nОтправь ссылку на поисковую выдачу Wildberries прямо в чат.",
				tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
					),
				),
			)
			return
		}
		text, keyboard := b.buildSearchListView(subs)
		b.editMenu(chatID, messageID, text, keyboard)

	case cb.Data == "menu:trial":
		user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
		if err != nil {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		b.handleTrial(ctx, chatID, messageID, user)

	case cb.Data == "menu:myplan":
		user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
		if err != nil {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		b.handleMyPlan(ctx, chatID, messageID, user)

	case cb.Data == "menu:ref":
		user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
		if err != nil {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		b.handleRef(ctx, chatID, messageID, user)

	case cb.Data == "menu:profile":
		user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
		if err != nil {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		b.handleProfile(ctx, chatID, messageID, user)

	case cb.Data == "profile:linkvk", cb.Data == "profile:relinkvk":
		user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
		if err != nil {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		b.profileLinkVK(ctx, chatID, messageID, user, cb.Data == "profile:relinkvk")

	case cb.Data == "profile:notify":
		user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
		if err != nil {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		b.profileToggleNotify(ctx, chatID, messageID, user)

	case cb.Data == "profile:email":
		b.promptChangeEmail(ctx, cb.From.ID, chatID, messageID)

	case cb.Data == "menu:plans":
		b.sendPlansMenu(chatID, messageID)

	case strings.HasPrefix(cb.Data, "plan:view:"):
		b.sendPlanCard(chatID, messageID, strings.TrimPrefix(cb.Data, "plan:view:"))

	case strings.HasPrefix(cb.Data, "plan:buy:"):
		b.handlePlanBuy(ctx, cb.From.ID, chatID, messageID, strings.TrimPrefix(cb.Data, "plan:buy:"))

	case strings.HasPrefix(cb.Data, "plan:subok:"):
		b.handleSubBuy(ctx, cb.From.ID, chatID, messageID, strings.TrimPrefix(cb.Data, "plan:subok:"))

	case strings.HasPrefix(cb.Data, "plan:sub:"):
		b.sendSubConsent(chatID, messageID, strings.TrimPrefix(cb.Data, "plan:sub:"))

	case cb.Data == "sub:cancel":
		b.handleSubCancelConfirm(ctx, cb.From.ID, chatID, messageID)

	case cb.Data == "sub:cancelok":
		b.handleSubCancel(ctx, cb.From.ID, chatID, messageID)

	case cb.Data == "menu:promo":
		b.sendPromoMenu(chatID, messageID)

	case cb.Data == "menu:help":
		b.sendHelpMenu(chatID, messageID, true)

	case strings.HasPrefix(cb.Data, "merge:"):
		b.handleMergeCallback(ctx, cb)

	case strings.HasPrefix(cb.Data, "untrack:"):
		b.callbackUntrack(ctx, cb)

	case strings.HasPrefix(cb.Data, "ptrack:"):
		b.handleTrackTriggerCallback(ctx, cb)

	case strings.HasPrefix(cb.Data, "strack:"):
		b.handleSearchTriggerCallback(ctx, cb)

	case strings.HasPrefix(cb.Data, "suntrack:"):
		b.callbackUntrackSearch(ctx, cb)

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

	// id — из callback_data; гасим только если подписка принадлежит этому юзеру.
	user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	if err := b.subRepo.Deactivate(ctx, id, user.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		b.log.Error("deactivate via callback", "err", err)
		b.answerCallback(cb.ID, "Ошибка, попробуй позже")
		return
	}

	subs, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err == nil && len(subs) > 0 {
		text, keyboard := b.buildListView(subs)
		b.editMenu(cb.Message.Chat.ID, cb.Message.MessageID, text, keyboard)
		b.answerCallback(cb.ID, "✅ Отслеживание отменено")
		return
	}

	b.sendMainMenu(ctx, cb.Message.Chat.ID, cb.Message.MessageID, true)
	b.answerCallback(cb.ID, "✅ Отслеживание отменено")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// showView — единая точка вывода экрана: messageID != 0 → правим существующее
// сообщение (навигация по меню живёт в одном сообщении), 0 → шлём новое
// (ответ на команду). Все menu:* колбэки должны передавать сюда cb.Message.MessageID.
func (b *Bot) showView(chatID int64, messageID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	if messageID != 0 {
		b.editMenu(chatID, messageID, text, keyboard)
		return
	}
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	m.ReplyMarkup = keyboard
	b.send(m)
}

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

// NotifyHTML — одиночное HTML-сообщение юзеру (для асинхронных уведомлений,
// напр. об успешной оплате из платёжного консьюмера).
func (b *Bot) NotifyHTML(chatID int64, text string) {
	b.reply(chatID, text)
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
