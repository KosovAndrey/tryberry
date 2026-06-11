package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// ── 👤 Профиль ────────────────────────────────────────────────────────────────

// handleProfile — экран профиля: идентичности, тариф кратко, настройка
// уведомлений. messageID != 0 → в том же сообщении.
func (b *Bot) handleProfile(ctx context.Context, chatID int64, messageID int, user *domain.User) {
	now := time.Now()
	plan := user.EffectivePlan(now)

	var sb strings.Builder
	sb.WriteString("👤 <b>Профиль</b>\n\n")

	fmt.Fprintf(&sb, "Тариф: <b>%s</b>", plan.Title)
	if user.PlanExpiresAt != nil && !user.PlanExpired(now) {
		fmt.Fprintf(&sb, " · до %s", user.PlanExpiresAt.Format("02.01.2006"))
	}
	sb.WriteString("\n\n")

	uname := "привязан"
	if user.Username != "" {
		uname = "@" + htmlEscape(user.Username)
	}
	fmt.Fprintf(&sb, "Telegram: ✅ %s\n", uname)

	if user.VKID != nil {
		fmt.Fprintf(&sb, "VK: ✅ привязан (id%d)\n", *user.VKID)
		fmt.Fprintf(&sb, "Уведомления: <b>%s</b>\n", notifyChannelTitle(user.NotifyChannel))
	} else {
		sb.WriteString("VK: ❌ не привязан\n")
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	if user.VKID == nil {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔗 Привязать VK", "profile:linkvk"),
		))
	} else {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔔 Уведомления", "profile:notify"),
			tgbotapi.NewInlineKeyboardButtonData("🔗 Сменить VK", "profile:relinkvk"),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("ℹ️ Тариф и лимиты", "menu:myplan"),
		tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
	))

	b.showView(chatID, messageID, sb.String(), tgbotapi.NewInlineKeyboardMarkup(rows...))
}

func notifyChannelTitle(ch string) string {
	switch ch {
	case domain.NotifyTG:
		return "Telegram"
	case domain.NotifyVK:
		return "VK"
	case domain.NotifyBoth:
		return "Telegram + VK"
	default:
		return "Telegram" // auto: зарегался в TG → TG
	}
}

// profileLinkVK — выдать код привязки VK (направление tg2vk).
func (b *Bot) profileLinkVK(ctx context.Context, chatID int64, messageID int, user *domain.User, relink bool) {
	if b.linkCodes == nil {
		b.showView(chatID, messageID, "Привязка временно недоступна, попробуй позже.", backToMenuKeyboard())
		return
	}

	code, err := b.linkCodes.Issue(ctx, user.ID, domain.LinkDirTG2VK)
	if err != nil {
		if errors.Is(err, domain.ErrLinkCodeRateLimited) {
			b.showView(chatID, messageID,
				"⏳ Код уже выдан — подожди минуту и попробуй снова, если не успел его использовать.",
				backToMenuKeyboard())
			return
		}
		b.log.Error("issue link code", "err", err)
		b.showView(chatID, messageID, "Произошла ошибка, попробуй позже.", backToMenuKeyboard())
		return
	}

	// При смене привязки старый VK отвязываем сразу: новый код докажет владение
	// новым аккаунтом, а старый юзер при следующем сообщении создаст пустой профиль.
	if relink {
		if err := b.userRepo.UnlinkVK(ctx, user.ID); err != nil {
			b.log.Error("unlink vk", "err", err)
		}
	}

	ttlMin := int(domain.LinkCodeTTL.Minutes())
	text := fmt.Sprintf(
		"🔗 <b>Привязка VK</b>\n\n"+
			"1. Открой нашего бота во ВКонтакте\n"+
			"2. Отправь ему сообщение:\n\n"+
			"<code>привязать %s</code>\n\n"+
			"Код действует <b>%d минут</b> и работает один раз. "+
			"Никому его не пересылай — это ключ к твоему аккаунту.",
		code, ttlMin)

	var rows [][]tgbotapi.InlineKeyboardButton
	if b.vkBotURL != "" {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("Открыть бота в VK", b.vkBotURL),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("👤 В профиль", "menu:profile"),
		tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
	))
	b.showView(chatID, messageID, text, tgbotapi.NewInlineKeyboardMarkup(rows...))
}

// profileToggleNotify — циклически переключить канал уведомлений (доступно,
// когда привязаны обе платформы): tg → vk → both → tg.
func (b *Bot) profileToggleNotify(ctx context.Context, chatID int64, messageID int, user *domain.User) {
	if user.VKID == nil {
		b.handleProfile(ctx, chatID, messageID, user)
		return
	}

	next := domain.NotifyVK
	switch user.NotifyChannel {
	case domain.NotifyVK:
		next = domain.NotifyBoth
	case domain.NotifyBoth:
		next = domain.NotifyTG
	}

	if err := b.userRepo.SetNotifyChannel(ctx, user.ID, next); err != nil {
		b.log.Error("set notify channel", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}
	user.NotifyChannel = next
	b.handleProfile(ctx, chatID, messageID, user)
}
