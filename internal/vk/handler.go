package vk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
	cmdProfile = "profile"
	cmdLink    = "link"
	cmdHelp    = "help"
	cmdAdd     = "add"
	cmdList    = "list"
	cmdUntrack = "untrack"
)

// payloadData — payload наших кнопок: {"cmd":"...","id":N}.
type payloadData struct {
	Cmd string `json:"cmd"`
	ID  int64  `json:"id,omitempty"`
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
// товаров по ссылке, список подписок. Поиск-подписки и тарифы — пока в TG.
type Bot struct {
	client    *Client
	log       *slog.Logger
	userRepo  *postgres.UserRepo
	subRepo   *postgres.SubscriptionRepo
	prodRepo  *postgres.ProductRepo
	registry  *scraper.Registry
	linkCodes *redisrepo.LinkCodeStore
}

func NewBot(
	client *Client,
	log *slog.Logger,
	userRepo *postgres.UserRepo,
	subRepo *postgres.SubscriptionRepo,
	prodRepo *postgres.ProductRepo,
	registry *scraper.Registry,
	linkCodes *redisrepo.LinkCodeStore,
) *Bot {
	return &Bot{
		client:    client,
		log:       log,
		userRepo:  userRepo,
		subRepo:   subRepo,
		prodRepo:  prodRepo,
		registry:  registry,
		linkCodes: linkCodes,
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
		}
	}

	if p.Cmd == "" {
		// «привязать <КОД>» / «link <КОД>» — предъявление кода, выданного в TG.
		// Код регистронезависим (Redeem приводит к UPPER), так что lower не мешает.
		// Кнопка «Привязать Telegram» сюда не попадает — у неё payload cmd=link.
		if rest, ok := cutAnyPrefix(lower, "привязать ", "link "); ok {
			b.handleLink(ctx, vkID, user, strings.TrimSpace(rest))
			return
		}
		// Поисковая ссылка — пока только в TG (FSM порога/типа уведомления там).
		if _, err := b.registry.FindSearchByURL(text); err == nil {
			b.send(ctx, vkID, "🔎 Поиск-подписки пока доступны только в Telegram-боте: @TryBerryBot.\n\n"+
				"Здесь я умею следить за отдельными товарами — отправь ссылку на товар.", kb)
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
	return fmt.Sprintf("👤 Профиль\n\nVK: привязан ✅\nTelegram: %s\nТариф: %s\n\n"+
		"Управление подписками и тарифом — пока в Telegram-боте.", tg, u.Plan)
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
		"Как добавить товар: отправь ссылку на товар WB прямо в чат — без команд.\n\n" +
		"Кнопки внизу:\n" +
		"➕ Добавить товар — как добавить\n" +
		"📋 Мои товары — список подписок, там же отписка\n" +
		"👤 Профиль — статус аккаунта и тариф\n"
	if u.TelegramID == 0 {
		base += "🔗 Привязать Telegram — связать аккаунты\n"
	}
	base += "\nПоиск-подписки (слежение за всей поисковой выдачей), тарифы и тонкая настройка уведомлений — " +
		"в Telegram-боте: @TryBerryBot.\n\nВопросы — пиши @kosov_andrey (Telegram)."
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
