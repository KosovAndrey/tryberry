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
	cmdHelp     = "help"
	cmdAdd      = "add"
	cmdList     = "list"
	cmdUntrack  = "untrack"
	cmdSearch   = "search"   // как добавить поиск-подписку
	cmdLSearch  = "lsearch"  // список поиск-подписок
	cmdSTrack   = "strack"   // выбор типа триггера поиск-подписки (k=any|below|disc)
	cmdSUntrack = "suntrack" // отписка от поиска
	cmdPlans    = "plans"    // витрина тарифов
	cmdPlanCard = "plan"     // карточка тарифа (k=имя плана)
	cmdBuy      = "buy"      // заглушка оплаты (k=имя плана)
	cmdTrial    = "trial"
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
	registry        *scraper.Registry
	linkCodes       *redisrepo.LinkCodeStore
	rdb             *redis.Client // FSM ввода порога (может быть nil)
}

func NewBot(
	client *Client,
	log *slog.Logger,
	userRepo *postgres.UserRepo,
	subRepo *postgres.SubscriptionRepo,
	prodRepo *postgres.ProductRepo,
	searchQueryRepo *postgres.SearchQueryRepo,
	searchSubRepo *postgres.SearchSubscriptionRepo,
	registry *scraper.Registry,
	linkCodes *redisrepo.LinkCodeStore,
	rdb *redis.Client,
) *Bot {
	return &Bot{
		client:          client,
		log:             log,
		userRepo:        userRepo,
		subRepo:         subRepo,
		prodRepo:        prodRepo,
		searchQueryRepo: searchQueryRepo,
		searchSubRepo:   searchSubRepo,
		registry:        registry,
		linkCodes:       linkCodes,
		rdb:             rdb,
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
		}
	}

	if p.Cmd != "" {
		// Любая кнопка/команда прерывает незавершённый ввод порога.
		b.clearSearchFSM(ctx, vkID)
	} else {
		// «привязать <КОД>» / «link <КОД>» — предъявление кода, выданного в TG.
		// Код регистронезависим (Redeem приводит к UPPER), так что lower не мешает.
		// Кнопка «Привязать Telegram» сюда не попадает — у неё payload cmd=link.
		if rest, ok := cutAnyPrefix(lower, "привязать ", "link "); ok {
			b.handleLink(ctx, vkID, user, strings.TrimSpace(rest))
			return
		}
		// Ждём число (порог/процент) для поиск-подписки?
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
		b.send(ctx, vkID, b.profileText(user), kb)
	case cmdLink:
		b.send(ctx, vkID, b.linkInstructionsText(user), kb)
	case cmdHelp:
		b.send(ctx, vkID, b.helpText(user), kb)
	case cmdAdd:
		b.send(ctx, vkID, "➕ Отправь мне ссылку на товар Wildberries — начну отслеживать цену.\n\n"+
			"Пример:\nhttps://www.wildberries.ru/catalog/252334498/detail.aspx", kb)
	case cmdList:
		b.handleList(ctx, vkID, user, "")
	case cmdUntrack:
		b.handleUntrack(ctx, vkID, user, p.ID)
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
			b.send(ctx, vkID, "Этот VK-аккаунт уже пользуется ботом отдельно — "+
				"автоматически объединить аккаунты нельзя. Напиши @kosov_andrey (Telegram), объединим вручную.", nil)
			return
		}
		b.log.Error("vk: link", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	b.send(ctx, vkID, "Готово! 🎉 Аккаунты связаны.\n\n"+
		"Теперь в Telegram-боте в Профиле можно выбрать, куда слать уведомления — "+
		"в Telegram, сюда или в оба места.\n\nКнопка «Профиль» покажет статус.",
		menuKeyboard(true))
	_ = vkUser // пустая VK-строка поглощена в LinkVK
}

func (b *Bot) profileText(u *domain.User) string {
	tg := "не привязан"
	if u.TelegramID != 0 {
		tg = "привязан ✅"
	}
	now := time.Now()
	plan := u.EffectivePlan(now)
	planLine := "Тариф: " + plan.Title
	if u.PlanExpiresAt != nil && !u.PlanExpired(now) {
		planLine += " (до " + u.PlanExpiresAt.Format(dateLayout) + ")"
	}
	return fmt.Sprintf("👤 Профиль\n\nVK: привязан ✅\nTelegram: %s\n%s\n"+
		"📦 Товаров: до %d · 🔎 Поисков: до %d\n\n"+
		"Куда слать уведомления (TG/VK/оба) — настраивается в Telegram-боте: Профиль → Уведомления.",
		tg, planLine, plan.MaxProduct, plan.MaxSearch)
}

func (b *Bot) linkInstructionsText(u *domain.User) string {
	if u.TelegramID != 0 {
		return "Твой аккаунт уже связан с Telegram ✅\n\n" +
			"Уведомления настраиваются в Telegram-боте: Профиль → Уведомления."
	}
	return "🔗 Как привязать Telegram:\n\n" +
		"1. Открой Telegram-бота @TryBerryBot (t.me/TryBerryBot)\n" +
		"2. Нажми «Профиль» → «Привязать VK» — бот выдаст код\n" +
		"3. Отправь код сюда сообщением:\nпривязать КОД\n\n" +
		"Код живёт 15 минут. После привязки уведомления о ценах смогут приходить сюда."
}

func (b *Bot) helpText(u *domain.User) string {
	base := "❓ Помощь\n\nЯ TryBerry — слежу за ценами на Wildberries и уведомляю о снижении 🍓\n\n" +
		"Как добавить товар: отправь ссылку на товар WB прямо в чат — без команд.\n" +
		"Поиск по ссылке: отправь ссылку на поисковую выдачу WB — буду следить за всей выдачей.\n\n" +
		"Кнопки внизу:\n" +
		"➕ Добавить товар / 🔎 Поиск по ссылке — как добавить\n" +
		"📋 Мои товары / 📡 Мои поиски — списки, там же отписка\n" +
		"💳 Тарифы — лимиты и цены, 🎁 Триал — попробовать поиск бесплатно\n" +
		"👤 Профиль — статус аккаунта и тариф\n"
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
