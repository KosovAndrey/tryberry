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
)

// CallbackEvent — событие VK Callback API (сырой формат, публикуется в Kafka
// топик vk-updates как есть; secret проверяет и срезает api-ingestor).
type CallbackEvent struct {
	Type    string          `json:"type"`
	GroupID int64           `json:"group_id"`
	Object  json.RawMessage `json:"object"`
}

// messageNew — object события message_new.
type messageNew struct {
	Message struct {
		FromID int64  `json:"from_id"`
		PeerID int64  `json:"peer_id"`
		Text   string `json:"text"`
	} `json:"message"`
}

// Bot — обработчик входящих событий VK. Фаза 1: регистрация, привязка по коду,
// статус профиля. Трекинг/меню в VK — фаза 2.
type Bot struct {
	client    *Client
	log       *slog.Logger
	userRepo  *postgres.UserRepo
	linkCodes *redisrepo.LinkCodeStore
}

func NewBot(client *Client, log *slog.Logger, userRepo *postgres.UserRepo, linkCodes *redisrepo.LinkCodeStore) *Bot {
	return &Bot{client: client, log: log, userRepo: userRepo, linkCodes: linkCodes}
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
	b.handleMessage(ctx, m.Message.FromID, strings.TrimSpace(m.Message.Text))
}

func (b *Bot) handleMessage(ctx context.Context, vkID int64, text string) {
	user, err := b.userRepo.UpsertVK(ctx, vkID)
	if err != nil {
		b.log.Error("vk: upsert user", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.")
		return
	}

	lower := strings.ToLower(text)

	// «привязать <КОД>» / «link <КОД>» — предъявление кода, выданного в TG.
	// Код регистронезависим (Redeem приводит к UPPER), так что lower не мешает.
	if rest, ok := cutAnyPrefix(lower, "привязать ", "link "); ok {
		b.handleLink(ctx, vkID, user, strings.TrimSpace(rest))
		return
	}

	switch lower {
	case "профиль", "profile":
		b.send(ctx, vkID, b.profileText(user))
	default:
		b.send(ctx, vkID, b.welcomeText(user))
	}
}

// handleLink — гасим код привязки (направление tg2vk: код выдан в Telegram,
// предъявлен здесь — владение обоими аккаунтами доказано).
func (b *Bot) handleLink(ctx context.Context, vkID int64, vkUser *domain.User, code string) {
	if b.linkCodes == nil {
		b.send(ctx, vkID, "Привязка временно недоступна, попробуй позже.")
		return
	}
	dir, tgUserID, err := b.linkCodes.Redeem(ctx, code)
	if err != nil || dir != domain.LinkDirTG2VK {
		// Неверный/истёкший код и чужое направление неразличимы для юзера.
		b.send(ctx, vkID, "Код не подошёл 😕 Проверь, что скопировал его целиком, "+
			"или получи новый в Telegram-боте: Профиль → Привязать VK (код живёт 15 минут).")
		return
	}

	if err := b.userRepo.LinkVK(ctx, tgUserID, vkID); err != nil {
		if errors.Is(err, domain.ErrVKAccountBusy) {
			b.send(ctx, vkID, "Этот VK-аккаунт уже пользуется ботом отдельно — "+
				"автоматически объединить аккаунты нельзя. Напиши @kosov_andrey (Telegram), объединим вручную.")
			return
		}
		b.log.Error("vk: link", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.")
		return
	}

	b.send(ctx, vkID, "Готово! 🎉 Аккаунты связаны.\n\n"+
		"Теперь в Telegram-боте в Профиле можно выбрать, куда слать уведомления — "+
		"в Telegram, сюда или в оба места.\n\nНапиши «профиль», чтобы проверить статус.")
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

func (b *Bot) welcomeText(u *domain.User) string {
	if u.TelegramID != 0 {
		return "Привет! Твой аккаунт связан с Telegram ✅\n\n" +
			"Сюда будут приходить уведомления о ценах (настройка — в Telegram-боте: Профиль → Уведомления).\n\n" +
			"Команды: «профиль» — статус аккаунта."
	}
	return "Привет! Я TryBerry — слежу за ценами на Wildberries 🍓\n\n" +
		"Пока я живу в основном в Telegram: @TryBerryBot\n\n" +
		"Если ты уже пользуешься Telegram-ботом — привяжи аккаунт, и уведомления смогут приходить сюда:\n" +
		"в Telegram-боте открой Профиль → «Привязать VK», получи код и отправь мне:\n" +
		"привязать КОД"
}

func (b *Bot) send(ctx context.Context, peerID int64, text string) {
	if err := b.client.SendMessage(ctx, peerID, text); err != nil {
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
