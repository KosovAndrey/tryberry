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
		fmt.Fprintf(&sb, "Уведомления: <b>%s</b>\n", domain.NotifyChannelTitle(user.NotifyChannel))
	} else {
		sb.WriteString("VK: ❌ не привязан\n")
	}

	// Email для чека 54-ФЗ — только когда оплата подключена.
	var hasEmail bool
	if b.payments != nil {
		if email, err := b.userRepo.GetEmail(ctx, user.ID); err == nil && email != "" {
			hasEmail = true
			fmt.Fprintf(&sb, "Email для чека: <b>%s</b>\n", htmlEscape(email))
		} else {
			sb.WriteString("Email для чека: ❌ не указан\n")
		}
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
	if b.payments != nil {
		label := "✉️ Указать email"
		if hasEmail {
			label = "✉️ Изменить email"
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, "profile:email"),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("ℹ️ Тариф и лимиты", "menu:myplan"),
		tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
	))

	b.showView(chatID, messageID, sb.String(), tgbotapi.NewInlineKeyboardMarkup(rows...))
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

// cutLinkPrefix — «привязать XXXX» / «link XXXX» (регистронезависимо) → код.
func cutLinkPrefix(text string) (string, bool) {
	lower := strings.ToLower(text)
	for _, p := range []string{"привязать ", "link "} {
		if strings.HasPrefix(lower, p) {
			return strings.TrimSpace(text[len(p):]), true
		}
	}
	return "", false
}

// handleLinkCode — предъявление кода, выданного в VK-боте (vk2tg): владение
// обоими аккаунтами доказано, телеграм-идентичность переезжает на VK-аккаунт
// (пустой TG-аккаунт поглощается, непустой → ручной merge).
func (b *Bot) handleLinkCode(ctx context.Context, chatID int64, user *domain.User, username, code string) {
	if b.linkCodes == nil {
		b.reply(chatID, "Привязка временно недоступна, попробуй позже.")
		return
	}
	dir, vkUserID, err := b.linkCodes.Redeem(ctx, code)
	// Код, выданный в VK (vk2tg) или MAX (max2tg) для предъявления здесь. В обоих
	// случаях привязываем нашу TG-идентичность к аккаунту-эмитенту (vkUserID).
	if err != nil || (dir != domain.LinkDirVK2TG && dir != domain.LinkDirMax2TG) {
		// Неверный/истёкший код и чужое направление неразличимы для юзера.
		b.reply(chatID, "Код не подошёл 😕 Проверь, что скопировал его целиком, "+
			"или получи новый в VK/MAX-боте: кнопка «Привязать Telegram» (код живёт 15 минут).")
		return
	}

	if err := b.userRepo.LinkTG(ctx, vkUserID, user.TelegramID, username); err != nil {
		if errors.Is(err, domain.ErrTGAccountBusy) {
			// Оба аккаунта непустые — предлагаем объединение (код уже доказал
			// владение обеими сторонами).
			b.startMergeFlow(ctx, chatID, user, vkUserID)
			return
		}
		b.log.Error("link tg", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	b.reply(chatID, "Готово! 🎉 Аккаунты связаны.\n\n"+
		"Подписки и тариф теперь общие с VK. Куда слать уведомления — настраивается в Профиле (/profile).")
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
