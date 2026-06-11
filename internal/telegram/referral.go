package telegram

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// ── /start ref_<telegram_id> ─────────────────────────────────────────────────

// handleRefStart — атрибуция по deep-link. Засчитывается только свежим
// аккаунтам (окно атрибуции), один раз и не самому себе — всё это правила SQL.
// Приветствие и меню — одним сообщением, без двойной отправки.
func (b *Bot) handleRefStart(ctx context.Context, chatID int64, user *domain.User, payload string) {
	referrerTgID, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || referrerTgID <= 0 {
		b.sendMainMenu(ctx, chatID, 0, false)
		return
	}

	ok, err := b.referralRepo.SetReferrer(ctx, user.TelegramID, referrerTgID, domain.ReferralAttributionWindow)
	if err != nil {
		b.log.Error("referral: set referrer", "err", err)
	}
	if !ok {
		b.sendMainMenu(ctx, chatID, 0, false)
		return
	}

	days := int(domain.ReferralTrialDuration.Hours() / 24)
	text := fmt.Sprintf(
		"🍓 <b>Привет! Ты здесь по приглашению друга</b>\n\n"+
			"Я слежу за ценами на Wildberries и пишу, когда они падают.\n\n"+
			"🎁 За приглашение тебе доступен расширенный триал — "+
			"<b>%d %s вместо %d</b>. Жми «Триал» ниже, когда захочешь попробовать поиск по ссылке.\n\n"+
			"А начать можно просто: отправь мне ссылку на любой товар WB 👇",
		days, daysWord(days), int(domain.TrialDuration.Hours()/24))
	b.showView(chatID, 0, text, mainMenuKeyboard())
}

// ── /ref — моя реферальная ссылка и статистика ───────────────────────────────

// daysWord — «3 дня», «7 дней», «21 день».
func daysWord(n int) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return "день"
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return "дня"
	default:
		return "дней"
	}
}

// handleRef — экран рефералки. messageID != 0 → в том же сообщении (меню),
// 0 → новым сообщением (команда /ref).
func (b *Bot) handleRef(ctx context.Context, chatID int64, messageID int, user *domain.User) {
	stats, err := b.referralRepo.Stats(ctx, user.ID)
	if err != nil {
		b.log.Error("referral: stats", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	link := fmt.Sprintf("https://t.me/%s?start=ref_%d", b.api.Self.UserName, user.TelegramID)
	trialDays := int(domain.ReferralTrialDuration.Hours() / 24)

	text := fmt.Sprintf(
		"👥 <b>Приведи друга — получи дни тарифа</b>\n\n"+

			"Твоя ссылка (нажми — скопируется):\n<code>%s</code>\n\n"+

			"🎁 <b>Друг получит</b>\n"+
			"Триал на <b>%d %s</b> вместо %d.\n\n"+

			"⭐️ <b>Ты получишь</b>\n"+
			"• <b>+%d %s</b> к тарифу — когда друг добавит подписку и начнёт пользоваться ботом;\n"+
			"• <b>+%d %s</b> — когда друг оплатит любой тариф.\n\n"+

			"Дни продлевают твой текущий платный тариф. Если ты на бесплатном — "+
			"на эти дни включится тариф <b>Lite</b>.\n\n"+

			"📊 <b>Твоя статистика</b>\n"+
			"Приглашено: <b>%d</b>\n"+
			"Освоились в боте: <b>%d</b>\n"+
			"Начислено дней: <b>%d</b>",
		link,
		trialDays, daysWord(trialDays), int(domain.TrialDuration.Hours()/24),
		domain.ReferralActivatedRewardDays, daysWord(domain.ReferralActivatedRewardDays),
		domain.ReferralPaidRewardDays, daysWord(domain.ReferralPaidRewardDays),
		stats.Invited, stats.Activated, stats.DaysGranted)

	shareText := fmt.Sprintf(
		"Бот следит за ценами на Wildberries и пишет, когда они падают. По моей ссылке — триал %d %s 🍓",
		trialDays, daysWord(trialDays))
	shareURL := "https://t.me/share/url?url=" + url.QueryEscape(link) + "&text=" + url.QueryEscape(shareText)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("📤 Отправить другу", shareURL),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
		),
	)

	b.showView(chatID, messageID, text, keyboard)
}
