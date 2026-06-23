package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/redis/go-redis/v9"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

type Bot struct {
	api       *tgbotapi.BotAPI
	log       *slog.Logger
	userRepo  *postgres.UserRepo
	subRepo   *postgres.SubscriptionRepo
	prodRepo  *postgres.ProductRepo
	priceRepo *postgres.PriceHistoryRepo // история цен для подсказки target (может быть nil)
	registry  *scraper.Registry
	resolver  *scraper.LinkResolver // разворачивает короткие ссылки приложений (ozon.ru/t/…, a.aliexpress.com/…)

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
	priceRepo *postgres.PriceHistoryRepo,
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
		priceRepo:       priceRepo,
		registry:        registry,
		resolver:        scraper.NewLinkResolver(0),
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

	// 1a⅒. Ждём ли промокод, введённый в платёжном флоу (кнопка на карточке тарифа)?
	if fsm, ok := b.getCheckoutPromoFSM(ctx, msg.From.ID); ok {
		b.handleCheckoutPromoInput(ctx, msg.Chat.ID, msg.From.ID, text, user, fsm)
		return
	}

	// 1a. Ждём ли число (порог/процент) для ТОВАРНОЙ подписки?
	if fsm, ok := b.getTrackFSM(ctx, msg.From.ID); ok {
		b.handleTrackThreshold(ctx, msg.Chat.ID, msg.From.ID, text, fsm)
		return
	}

	// 1b. Поиск-подписка ждёт ввод: текст-фильтр витрины продавца (SellerURL) или
	// число (порог/процент) после выбора триггера.
	if fsm, ok := b.getSearchFSM(ctx, msg.From.ID); ok {
		if fsm.SellerURL != "" {
			b.handleSellerTextFilter(ctx, msg.Chat.ID, msg.From.ID, text, user, fsm)
		} else {
			b.handleSearchThreshold(ctx, msg.Chat.ID, msg.From.ID, text, user, fsm)
		}
		return
	}

	// 1c. Код привязки, выданный в VK-боте («привязать XXXX», направление vk2tg).
	if rest, ok := cutLinkPrefix(text); ok {
		b.handleLinkCode(ctx, msg.Chat.ID, user, msg.From.UserName, rest)
		return
	}

	// 1c½. Короткие ссылки из мобильных приложений (ozon.ru/t/…, a.aliexpress.com/…)
	// сами по себе не товарные URL — разворачиваем по 3xx в канонический URL ДО гейтов
	// распознавания (bulk/поиск/товар), иначе они падают в главное меню. Сетевой запрос
	// только для allowlist-хостов; обычный текст/ссылки проходят без сети.
	text = b.resolver.ExpandInText(ctx, text)

	// 1d. Несколько товарных ссылок в одном сообщении → массовое добавление
	// (одна сводка вместо карточки на каждую). Одиночная ссылка идёт обычным флоу ниже.
	if prods := b.trackableProductURLs(text); len(prods) >= 2 {
		b.handleBulkTrack(ctx, msg.Chat.ID, prods, user)
		return
	}

	// 2. Поисковая ссылка (возможно с лишним текстом вокруг) → флоу поиск-подписки.
	// Сначала по извлечённым из текста ссылкам, затем — по всему тексту (на случай
	// «голой» ссылки без схемы, которую regex не ловит).
	if su := b.firstSearchURL(text); su != "" {
		b.startSearchTrack(ctx, msg.Chat.ID, su, user)
		return
	}

	// 3. Одна товарная ссылка → товарный флоу по ИЗВЛЕЧЁННОЙ ссылке. Пользователь
	// часто присылает «Название\nссылка» (шеринг из приложения WB) или «/track
	// ссылка» — лишний текст игнорируем, иначе он уедет в products.url и сломает
	// inline-клавиатуру списка. Фолбэк на весь текст — для «голой» ссылки без схемы.
	if prods := b.trackableProductURLs(text); len(prods) == 1 {
		b.doTrack(ctx, msg.Chat.ID, prods[0], user)
		return
	}
	if _, err := b.registry.FindByURL(text); err == nil {
		b.doTrack(ctx, msg.Chat.ID, text, user)
		return
	}

	// 4. Витрина продавца с буквенной ссылкой (/seller/имя) → резолвим слаг в
	// числовой id (через wbaas-токен) и заводим как обычную seller-подписку.
	if slug, ok := domain.SellerVanitySlug(text); ok {
		if id, err := b.registry.ResolveSellerVanity(ctx, slug); err == nil && id != "" {
			b.startSearchTrack(ctx, msg.Chat.ID, domain.RewriteSellerVanity(text, id), user)
		} else {
			if err != nil {
				b.log.Warn("resolve seller vanity", "slug", slug, "err", err)
			}
			b.reply(msg.Chat.ID, "🏬 Не получилось открыть этот магазин по буквенной ссылке. Попробуй ссылку с числовым номером (вида <code>/seller/250021611</code>) — её даёт кнопка «Поделиться» на странице продавца в приложении WB.")
		}
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
	b.clearCheckoutPromoFSM(ctx, msg.From.ID)

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
		args = b.resolver.ExpandInText(ctx, args)
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
		b.sendPlansMenu(ctx, msg.From.ID, msg.Chat.ID, 0)
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
	case "extend":
		b.handleExtend(ctx, msg)
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

// ── doTrack — основная логика добавления товара ───────────────────────────────

// ozonComingSoonMsg — заглушка для ссылок Ozon, пока маркетплейс не запущен
// (антибот FAB требует аккаунт-сессию + RU-мобильный прокси, см. docs/OZON-STATUS.md).
const ozonComingSoonMsg = "🔵 <b>Ozon скоро будет</b> — отслеживание этого маркетплейса ещё в разработке.\n\n" +
	"Пока отслеживаю <b>Wildberries</b> 🟣 — пришли ссылку на товар оттуда."

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
		b.sendPlansMenu(ctx, cb.From.ID, chatID, messageID)

	case strings.HasPrefix(cb.Data, "plan:view:"):
		b.sendPlanCard(ctx, cb.From.ID, chatID, messageID, strings.TrimPrefix(cb.Data, "plan:view:"))

	case strings.HasPrefix(cb.Data, "plan:buy:"):
		b.handlePlanBuy(ctx, cb.From.ID, chatID, messageID, strings.TrimPrefix(cb.Data, "plan:buy:"))

	case strings.HasPrefix(cb.Data, "plan:promo:"):
		b.promptCheckoutPromo(ctx, cb.From.ID, chatID, messageID, strings.TrimPrefix(cb.Data, "plan:promo:"))

	case strings.HasPrefix(cb.Data, "plan:subok:"):
		b.handleSubBuy(ctx, cb.From.ID, chatID, messageID, strings.TrimPrefix(cb.Data, "plan:subok:"))

	case strings.HasPrefix(cb.Data, "plan:sub:"):
		b.sendSubConsent(ctx, cb.From.ID, chatID, messageID, strings.TrimPrefix(cb.Data, "plan:sub:"))

	case cb.Data == "sub:cancel":
		b.handleSubCancelConfirm(ctx, cb.From.ID, chatID, messageID)

	case cb.Data == "sub:cancelok":
		b.handleSubCancel(ctx, cb.From.ID, chatID, messageID)

	case cb.Data == "menu:promo":
		b.promptCheckoutPromo(ctx, cb.From.ID, chatID, messageID, "")

	case cb.Data == "menu:help":
		b.sendHelpMenu(chatID, messageID, true)

	case strings.HasPrefix(cb.Data, "merge:"):
		b.handleMergeCallback(ctx, cb)

	case strings.HasPrefix(cb.Data, "untrack:"):
		b.callbackUntrack(ctx, cb)

	case strings.HasPrefix(cb.Data, "ptgt:"):
		b.handleTrackTargetCallback(ctx, cb)

	case strings.HasPrefix(cb.Data, "ptrack:"):
		b.handleTrackTriggerCallback(ctx, cb)

	case strings.HasPrefix(cb.Data, "strack:"):
		b.handleSearchTriggerCallback(ctx, cb)

	case cb.Data == "sfskip":
		b.handleSellerSkipFilter(ctx, cb)

	case strings.HasPrefix(cb.Data, "suntrack:"):
		b.callbackUntrackSearch(ctx, cb)

	case strings.HasPrefix(cb.Data, "keep:"):
		b.answerCallback(cb.ID, "Продолжаем следить 👍")
	}
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
	case "aliexpress":
		return "🔴"
	default:
		return "📦"
	}
}
