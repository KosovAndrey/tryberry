package vk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

// CallbackEvent — событие VK Callback API (сырой формат, публикуется в Kafka
// топик vk-updates как есть; secret проверяет и срезает api-ingestor).
type CallbackEvent struct {
	Type    string          `json:"type"`
	GroupID int64           `json:"group_id"`
	Object  json.RawMessage `json:"object"`
}

// messageNew — object события message_new. Payload приходит от нажатий
// text-кнопок клавиатуры (JSON-строка, см. TextButton).
type messageNew struct {
	Message struct {
		FromID  int64  `json:"from_id"`
		PeerID  int64  `json:"peer_id"`
		Text    string `json:"text"`
		Payload string `json:"payload"`
	} `json:"message"`
}

// Команды кнопок (payload). Роутим по ним, а не по label — текст кнопки можно
// менять, не ломая обработку.
const (
	cmdProfile  = "profile"
	cmdLink     = "link"
	cmdUnlinkTG = "unlinktg" // отвязать Telegram (k=confirm — подтверждено)
	cmdNotify   = "notify"   // цикл канала уведомлений tg→vk→both
	cmdHelp     = "help"
	cmdAdd      = "add"
	cmdList     = "list"
	cmdUntrack  = "untrack"
	cmdPTrack   = "ptrack"   // тип триггера товарной подписки (k=any|below|disc)
	cmdSearch   = "search"   // как добавить поиск-подписку
	cmdLSearch  = "lsearch"  // список поиск-подписок
	cmdSTrack   = "strack"   // выбор типа триггера поиск-подписки (k=any|below|disc)
	cmdSUntrack = "suntrack" // отписка от поиска
	cmdPlans    = "plans"    // витрина тарифов
	cmdPlanCard = "plan"     // карточка тарифа (k=имя плана)
	cmdBuy      = "buy"      // заглушка оплаты (k=имя плана)
	cmdTrial    = "trial"
	cmdPromo    = "promo" // как активировать промокод
	cmdRef      = "ref"   // пригласить друга (код + статистика)
	cmdMerge    = "merge" // слияние аккаунтов (k = opt:<i>|confirm:<i>|back|cancel)
)

// payloadData — payload наших кнопок: {"cmd":"...","id":N,"k":"..."}.
type payloadData struct {
	Cmd  string `json:"cmd"`
	ID   int64  `json:"id,omitempty"`
	Kind string `json:"k,omitempty"`
}

func buttonPayload(cmd string) string {
	return fmt.Sprintf(`{"cmd":%q}`, cmd)
}

// parsePayload — payload кнопки (zero value — не кнопка/не наш формат).
func parsePayload(payload string) payloadData {
	var p payloadData
	if payload == "" {
		return p
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return payloadData{}
	}
	return p
}

// Bot — обработчик входящих событий VK. Фаза 2: привязка аккаунтов, трекинг
// товаров и поиск-подписки по ссылке, тарифы (заглушка оплаты), триал.
type Bot struct {
	client          *Client
	log             *slog.Logger
	userRepo        *postgres.UserRepo
	subRepo         *postgres.SubscriptionRepo
	prodRepo        *postgres.ProductRepo
	searchQueryRepo *postgres.SearchQueryRepo
	searchSubRepo   *postgres.SearchSubscriptionRepo
	promoRepo       *postgres.PromoRepo
	referralRepo    *postgres.ReferralRepo
	registry        *scraper.Registry
	linkCodes       *redisrepo.LinkCodeStore
	rdb             *redis.Client // FSM ввода порога (может быть nil)
	botURL          string        // ссылка на VK-бота для приглашений ("" — не показывать)
}

func NewBot(
	client *Client,
	log *slog.Logger,
	userRepo *postgres.UserRepo,
	subRepo *postgres.SubscriptionRepo,
	prodRepo *postgres.ProductRepo,
	searchQueryRepo *postgres.SearchQueryRepo,
	searchSubRepo *postgres.SearchSubscriptionRepo,
	promoRepo *postgres.PromoRepo,
	referralRepo *postgres.ReferralRepo,
	registry *scraper.Registry,
	linkCodes *redisrepo.LinkCodeStore,
	rdb *redis.Client,
	botURL string,
) *Bot {
	return &Bot{
		client:          client,
		log:             log,
		userRepo:        userRepo,
		subRepo:         subRepo,
		prodRepo:        prodRepo,
		searchQueryRepo: searchQueryRepo,
		searchSubRepo:   searchSubRepo,
		promoRepo:       promoRepo,
		referralRepo:    referralRepo,
		registry:        registry,
		linkCodes:       linkCodes,
		rdb:             rdb,
		botURL:          botURL,
	}
}

// HandleEvent — точка входа для события из Kafka.
func (b *Bot) HandleEvent(ctx context.Context, ev CallbackEvent) {
	if ev.Type != "message_new" {
		return // лайки/подписки и прочее — не интересуют в фазе 1
	}
	var m messageNew
	if err := json.Unmarshal(ev.Object, &m); err != nil {
		b.log.Error("vk: decode message_new", "err", err)
		return
	}
	// Только личные сообщения (peer_id > 2e9 — чаты, их игнорируем).
	if m.Message.PeerID != m.Message.FromID {
		return
	}
	b.handleMessage(ctx, m.Message.FromID, strings.TrimSpace(m.Message.Text), m.Message.Payload)
}

// menuKeyboard — постоянная клавиатура: до привязки на первом месте
// «Привязать Telegram».
func menuKeyboard(linked bool) *Keyboard {
	var rows [][]Button
	if !linked {
		rows = append(rows, []Button{
			TextButton("🔗 Привязать Telegram", buttonPayload(cmdLink), ColorPrimary),
		})
	}
	rows = append(rows,
		[]Button{
			TextButton("➕ Добавить товар", buttonPayload(cmdAdd), ColorPrimary),
			TextButton("📋 Мои товары", buttonPayload(cmdList), ColorPrimary),
		},
		[]Button{
			TextButton("🔎 Поиск по ссылке", buttonPayload(cmdSearch), ColorSecondary),
			TextButton("📡 Мои поиски", buttonPayload(cmdLSearch), ColorSecondary),
		},
		[]Button{
			TextButton("💳 Тарифы", buttonPayload(cmdPlans), ColorSecondary),
			TextButton("🎁 Триал", buttonPayload(cmdTrial), ColorSecondary),
		},
		[]Button{
			TextButton("🎟 Промокод", buttonPayload(cmdPromo), ColorSecondary),
			TextButton("👥 Пригласить друга", buttonPayload(cmdRef), ColorSecondary),
		},
		[]Button{
			TextButton("👤 Профиль", buttonPayload(cmdProfile), ColorSecondary),
			TextButton("❓ Помощь", buttonPayload(cmdHelp), ColorSecondary),
		},
	)
	return &Keyboard{Buttons: rows}
}

func (b *Bot) handleMessage(ctx context.Context, vkID int64, text, payload string) {
	user, err := b.userRepo.UpsertVK(ctx, vkID)
	if err != nil {
		b.log.Error("vk: upsert user", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	kb := menuKeyboard(user.TelegramID != 0)

	p := parsePayload(payload)
	lower := strings.ToLower(text)
	if p.Cmd == "" {
		switch lower {
		case "профиль", "profile":
			p.Cmd = cmdProfile
		case "помощь", "help", "начать", "start":
			p.Cmd = cmdHelp
		case "мои товары", "список", "list":
			p.Cmd = cmdList
		case "добавить товар", "добавить":
			p.Cmd = cmdAdd
		case "мои поиски":
			p.Cmd = cmdLSearch
		case "тарифы":
			p.Cmd = cmdPlans
		case "триал":
			p.Cmd = cmdTrial
		case "промокод":
			p.Cmd = cmdPromo
		}
	}

	if p.Cmd != "" {
		// Любая кнопка/команда прерывает незавершённый ввод порога.
		b.clearSearchFSM(ctx, vkID)
		b.clearTrackFSM(ctx, vkID)
	} else {
		// «привязать <КОД>» / «link <КОД>» — предъявление кода, выданного в TG.
		// Код регистронезависим (Redeem приводит к UPPER), так что lower не мешает.
		// Кнопка «Привязать Telegram» сюда не попадает — у неё payload cmd=link.
		if rest, ok := cutAnyPrefix(lower, "привязать ", "link "); ok {
			b.handleLink(ctx, vkID, user, strings.TrimSpace(rest))
			return
		}
		// «промокод <КОД>» — активация промокода.
		if rest, ok := cutAnyPrefix(lower, "промокод ", "promo "); ok {
			b.clearSearchFSM(ctx, vkID)
			b.clearTrackFSM(ctx, vkID)
			b.handlePromoCode(ctx, vkID, user, strings.TrimSpace(rest))
			return
		}
		// «друг <КОД>» — код приглашения (= users.id реферера).
		if rest, ok := cutAnyPrefix(lower, "друг ", "friend "); ok {
			b.clearSearchFSM(ctx, vkID)
			b.clearTrackFSM(ctx, vkID)
			b.handleRefCode(ctx, vkID, user, strings.TrimSpace(rest))
			return
		}
		// Ждём число (порог/процент)? Сначала товарный FSM, потом поисковый.
		if fsm, ok := b.getTrackFSM(ctx, vkID); ok {
			b.handleTrackThreshold(ctx, vkID, user, text, fsm)
			return
		}
		if fsm, ok := b.getSearchFSM(ctx, vkID); ok {
			b.handleSearchThreshold(ctx, vkID, user, text, fsm)
			return
		}
		// Поисковая ссылка → флоу поиск-подписки.
		if _, err := b.registry.FindSearchByURL(text); err == nil {
			b.startSearchTrack(ctx, vkID, user, text)
			return
		}
		// Ссылка на товар → трекинг.
		if _, err := b.registry.FindByURL(text); err == nil {
			b.handleTrack(ctx, vkID, user, text)
			return
		}
	}

	switch p.Cmd {
	case cmdProfile:
		b.sendProfile(ctx, vkID, user)
	case cmdNotify:
		b.toggleNotify(ctx, vkID, user)
	case cmdLink:
		b.issueLinkCode(ctx, vkID, user)
	case cmdUnlinkTG:
		b.handleUnlinkTG(ctx, vkID, user, p.Kind == "confirm")
	case cmdHelp:
		b.send(ctx, vkID, b.helpText(user), kb)
	case cmdAdd:
		b.send(ctx, vkID, "➕ Отправь мне ссылку на товар Wildberries — начну отслеживать цену.\n\n"+
			"Пример:\nhttps://www.wildberries.ru/catalog/252334498/detail.aspx", kb)
	case cmdList:
		b.handleList(ctx, vkID, user, "")
	case cmdUntrack:
		b.handleUntrack(ctx, vkID, user, p.ID)
	case cmdPTrack:
		b.handleProductTrigger(ctx, vkID, user, p)
	case cmdSearch:
		b.send(ctx, vkID, "🔎 Поиск по ссылке\n\n"+
			"Отправь ссылку на поисковую выдачу Wildberries — буду следить за всей выдачей и напишу, когда товары подешевеют.\n\n"+
			"Как получить ссылку: на сайте WB введи запрос в поиск и скопируй адрес страницы.\n\n"+
			"Пример:\nhttps://www.wildberries.ru/catalog/0/search.aspx?search=наушники", kb)
	case cmdLSearch:
		b.handleListSearch(ctx, vkID, user, "")
	case cmdSTrack:
		b.handleSearchTrigger(ctx, vkID, user, p)
	case cmdSUntrack:
		b.handleUntrackSearch(ctx, vkID, user, p.ID)
	case cmdPlans:
		b.sendPlans(ctx, vkID, user)
	case cmdPlanCard:
		b.sendPlanCard(ctx, vkID, user, p.Kind)
	case cmdBuy:
		b.sendBuyStub(ctx, vkID, user, p.Kind)
	case cmdTrial:
		b.handleTrial(ctx, vkID, user)
	case cmdPromo:
		b.send(ctx, vkID, "🎟 Промокод\n\nЕсть код? Отправь его сообщением:\nпромокод КОД\n\n"+
			"Промокоды дают дни тарифа бесплатно или скидку на оплату.", kb)
	case cmdRef:
		b.sendRef(ctx, vkID, user)
	case cmdMerge:
		b.handleMergeAction(ctx, vkID, user, p.Kind)
	default:
		b.send(ctx, vkID, b.welcomeText(user), kb)
	}
}

// handleLink — гасим код привязки (направление tg2vk: код выдан в Telegram,
// предъявлен здесь — владение обоими аккаунтами доказано).
func (b *Bot) handleLink(ctx context.Context, vkID int64, vkUser *domain.User, code string) {
	if b.linkCodes == nil {
		b.send(ctx, vkID, "Привязка временно недоступна, попробуй позже.", nil)
		return
	}
	dir, tgUserID, err := b.linkCodes.Redeem(ctx, code)
	if err != nil || dir != domain.LinkDirTG2VK {
		// Неверный/истёкший код и чужое направление неразличимы для юзера.
		b.send(ctx, vkID, "Код не подошёл 😕 Проверь, что скопировал его целиком, "+
			"или получи новый в Telegram-боте: Профиль → Привязать VK (код живёт 15 минут).",
			menuKeyboard(false))
		return
	}

	if err := b.userRepo.LinkVK(ctx, tgUserID, vkID); err != nil {
		if errors.Is(err, domain.ErrVKAccountBusy) {
			// Оба аккаунта непустые — предлагаем объединение (код уже доказал
			// владение обеими сторонами).
			b.startMergeFlow(ctx, vkID, vkUser, tgUserID)
			return
		}
		b.log.Error("vk: link", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	b.send(ctx, vkID, "Готово! 🎉 Аккаунты связаны.\n\n"+
		"Теперь в Профиле (кнопка внизу) можно выбрать, куда слать уведомления — "+
		"в Telegram, сюда или в оба места.",
		menuKeyboard(true))
}

// sendProfile — экран профиля: идентичности, тариф, использование лимитов,
// настройка уведомлений и управление привязкой Telegram (только TG — VK здесь
// менять нельзя, симметрично TG-боту, где меняется только VK).
func (b *Bot) sendProfile(ctx context.Context, vkID int64, u *domain.User) {
	now := time.Now()
	plan := u.EffectivePlan(now)

	prod, _ := b.subRepo.CountActiveByUserID(ctx, u.ID)
	srch, _ := b.searchSubRepo.CountActiveByUserID(ctx, u.ID)

	tg := "❌ не привязан (кнопка внизу)"
	if u.TelegramID != 0 {
		tg = "✅ привязан"
	}
	planLine := "Тариф: " + plan.Title
	if u.PlanExpiresAt != nil && !u.PlanExpired(now) {
		planLine += " (до " + u.PlanExpiresAt.Format(dateLayout) + ")"
	}

	var sb strings.Builder
	sb.WriteString("👤 Профиль\n\n")
	fmt.Fprintf(&sb, "VK: ✅ привязан\nTelegram: %s\n%s\n", tg, planLine)
	fmt.Fprintf(&sb, "📦 Товаров: %d из %d · 🔎 Поисков: %d из %d\n", prod, plan.MaxProduct, srch, plan.MaxSearch)

	var kb *Keyboard
	if u.TelegramID != 0 {
		fmt.Fprintf(&sb, "🔔 Уведомления: %s\n", domain.NotifyChannelTitle(u.NotifyChannel))
		kb = &Keyboard{Inline: true, Buttons: [][]Button{
			{TextButton("🔔 Уведомления: "+domain.NotifyChannelTitle(u.NotifyChannel),
				buttonPayload(cmdNotify), ColorPrimary)},
			{TextButton("🔗 Отвязать Telegram", buttonPayload(cmdUnlinkTG), ColorSecondary)},
		}}
	} else {
		kb = menuKeyboard(false)
	}
	b.send(ctx, vkID, sb.String(), kb)
}

// toggleNotify — циклически переключить канал уведомлений: tg → vk → both → tg.
func (b *Bot) toggleNotify(ctx context.Context, vkID int64, u *domain.User) {
	if u.TelegramID == 0 {
		b.sendProfile(ctx, vkID, u)
		return
	}
	next := domain.NotifyVK
	switch u.NotifyChannel {
	case domain.NotifyVK:
		next = domain.NotifyBoth
	case domain.NotifyBoth:
		next = domain.NotifyTG
	}
	if err := b.userRepo.SetNotifyChannel(ctx, u.ID, next); err != nil {
		b.log.Error("vk: set notify channel", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	u.NotifyChannel = next
	b.sendProfile(ctx, vkID, u)
}

// issueLinkCode — выдать код привязки Telegram (направление vk2tg: код выдан
// здесь, предъявляется в TG-боте).
func (b *Bot) issueLinkCode(ctx context.Context, vkID int64, u *domain.User) {
	if u.TelegramID != 0 {
		b.send(ctx, vkID, "Твой аккаунт уже связан с Telegram ✅\n\n"+
			"Сменить привязку: сначала «Отвязать Telegram» в Профиле, потом привязать заново.",
			menuKeyboard(true))
		return
	}
	if b.linkCodes == nil {
		b.send(ctx, vkID, "Привязка временно недоступна, попробуй позже.", nil)
		return
	}
	code, err := b.linkCodes.Issue(ctx, u.ID, domain.LinkDirVK2TG)
	if err != nil {
		if errors.Is(err, domain.ErrLinkCodeRateLimited) {
			b.send(ctx, vkID, "⏳ Код уже выдан — подожди минуту и попробуй снова, если не успел его использовать.", nil)
			return
		}
		b.log.Error("vk: issue link code", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	ttlMin := int(domain.LinkCodeTTL.Minutes())
	b.send(ctx, vkID, fmt.Sprintf(
		"🔗 Привязка Telegram\n\n"+
			"1. Открой Telegram-бота @TryBerryBot (t.me/TryBerryBot)\n"+
			"2. Отправь ему сообщение:\n\nпривязать %s\n\n"+
			"Код действует %d минут и работает один раз. Никому его не пересылай — "+
			"это ключ к твоему аккаунту.\n\n"+
			"Если уже пользуешься Telegram-ботом, можно наоборот: в TG Профиль → "+
			"«Привязать VK» и прислать код сюда.",
		code, ttlMin), menuKeyboard(false))
}

// handleUnlinkTG — отвязка Telegram из VK (с подтверждением). VK-идентичность
// отсюда отвязать нельзя — только Telegram (зеркально TG-боту).
func (b *Bot) handleUnlinkTG(ctx context.Context, vkID int64, u *domain.User, confirmed bool) {
	if u.TelegramID == 0 {
		b.sendProfile(ctx, vkID, u)
		return
	}
	if !confirmed {
		kb := &Keyboard{Inline: true, Buttons: [][]Button{
			{TextButton("⚠️ Да, отвязать", fmt.Sprintf(`{"cmd":%q,"k":"confirm"}`, cmdUnlinkTG), ColorSecondary)},
			{TextButton("◀️ Отмена", buttonPayload(cmdProfile), ColorPrimary)},
		}}
		b.send(ctx, vkID, "Отвязать Telegram от этого аккаунта?\n\n"+
			"Подписки и тариф останутся здесь, в VK. Telegram-аккаунт начнёт с чистого листа "+
			"при следующем заходе в TG-бота.", kb)
		return
	}
	if err := b.userRepo.UnlinkTG(ctx, u.ID); err != nil {
		b.log.Error("vk: unlink tg", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	u.TelegramID = 0
	b.send(ctx, vkID, "✅ Telegram отвязан. Уведомления теперь приходят сюда, в VK.\n\n"+
		"Привязать снова — кнопка «Привязать Telegram» внизу.", menuKeyboard(false))
}

func (b *Bot) helpText(u *domain.User) string {
	base := "❓ Помощь\n\nЯ TryBerry — слежу за ценами на Wildberries и уведомляю о снижении 🍓\n\n" +
		"Как добавить товар: отправь ссылку на товар WB прямо в чат — без команд.\n" +
		"Поиск по ссылке: отправь ссылку на поисковую выдачу WB — буду следить за всей выдачей.\n\n" +
		"Кнопки внизу:\n" +
		"➕ Добавить товар / 🔎 Поиск по ссылке — как добавить\n" +
		"📋 Мои товары / 📡 Мои поиски — списки, там же отписка\n" +
		"💳 Тарифы — лимиты и цены, 🎁 Триал — попробовать поиск бесплатно\n" +
		"🎟 Промокод — активировать код, 👥 Пригласить друга — бонусные дни\n" +
		"👤 Профиль — аккаунт, уведомления, привязка\n"
	if u.TelegramID == 0 {
		base += "🔗 Привязать Telegram — связать аккаунты\n"
	}
	base += "\nВопросы — пиши @kosov_andrey (Telegram)."
	return base
}

func (b *Bot) welcomeText(u *domain.User) string {
	if u.TelegramID != 0 {
		return "Привет! Твой аккаунт связан с Telegram ✅\n\n" +
			"Отправь ссылку на товар Wildberries — начну отслеживать. " +
			"Подписки общие с Telegram, уведомления — куда настроишь (Профиль → Уведомления в TG-боте).\n\n" +
			"Кнопки внизу: «Мои товары» — список, «Помощь» — что я умею."
	}
	return "Привет! Я TryBerry — слежу за ценами на Wildberries 🍓\n\n" +
		"Отправь мне ссылку на товар — начну отслеживать и напишу, когда цена снизится.\n\n" +
		"Уже пользуешься Telegram-ботом @TryBerryBot? Нажми «Привязать Telegram» внизу — " +
		"подписки и тариф станут общими."
}

func (b *Bot) send(ctx context.Context, peerID int64, text string, kb *Keyboard) {
	if err := b.client.SendMessageKeyboard(ctx, peerID, text, kb); err != nil {
		b.log.Error("vk: send", "err", err, "peer", peerID)
	}
}

// cutAnyPrefix — первый подошедший префикс: остаток и ok.
func cutAnyPrefix(s string, prefixes ...string) (string, bool) {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest, true
		}
	}
	return "", false
}
