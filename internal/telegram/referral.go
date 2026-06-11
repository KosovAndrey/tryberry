package telegram

import (
	"context"
	"fmt"
	"strconv"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// ── /start ref_<telegram_id> ─────────────────────────────────────────────────

// handleRefStart — атрибуция по deep-link. Засчитывается только свежим
// аккаунтам (окно атрибуции), один раз и не самому себе — всё это правила SQL.
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
	if ok {
		days := int(domain.ReferralTrialDuration.Hours() / 24)
		b.reply(chatID, fmt.Sprintf(
			"👋 Привет! Ты пришёл по приглашению — держи бонус:\n"+
				"🎁 расширенный триал на <b>%d дней</b> вместо %d.\n\n"+
				"Активируй: /trial",
			days, int(domain.TrialDuration.Hours()/24)))
	}
	b.sendMainMenu(ctx, chatID, 0, false)
}

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

// ── /ref — моя реферальная ссылка и статистика ───────────────────────────────

func (b *Bot) handleRef(ctx context.Context, chatID int64, user *domain.User) {
	stats, err := b.referralRepo.Stats(ctx, user.ID)
	if err != nil {
		b.log.Error("referral: stats", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	link := fmt.Sprintf("https://t.me/%s?start=ref_%d", b.api.Self.UserName, user.TelegramID)
	trialDays := int(domain.ReferralTrialDuration.Hours() / 24)

	b.reply(chatID, fmt.Sprintf(
		"👥 <b>Приведи друга</b>\n\n"+
			"Твоя ссылка:\n<code>%s</code>\n\n"+
			"Что получает друг: триал на <b>%d дней</b> вместо %d.\n"+
			"Что получаешь ты: <b>+%d дней</b> тарифа за каждого друга, который "+
			"добавит подписку и останется с ботом (на Free выдаём Lite), "+
			"и <b>+%d дней</b> — когда друг оформит платный тариф.\n\n"+
			"📊 Приглашено: <b>%d</b> · активировалось: <b>%d</b> · начислено дней: <b>%d</b>\n\n"+
			"<i>Потолок начислений — %d дней в год.</i>",
		link,
		trialDays, int(domain.TrialDuration.Hours()/24),
		domain.ReferralActivatedRewardDays, domain.ReferralPaidRewardDays,
		stats.Invited, stats.Activated, stats.DaysGranted,
		domain.ReferralYearlyCapDays))
}
