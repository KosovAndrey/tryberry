package max

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Промокоды и рефералка в MAX. Логика та же, что в telegram/vk; код приглашения =
// users.id реферера. Deep-link ?start=ref_<users.id> обрабатывает handleStart.

func (b *Bot) handlePromoCode(ctx context.Context, maxID int64, user *domain.User, codeArg string) {
	if domain.NormalizePromoCode(codeArg) == "" {
		b.promptPromo(ctx, maxID, user, "")
		return
	}
	b.applyPromoCode(ctx, maxID, user, codeArg, "")
}

func (b *Bot) applyGrantPromo(ctx context.Context, maxID int64, user *domain.User, promo *domain.PromoCode, kb *Keyboard) {
	now := time.Now()

	expiresAt, err := domain.ApplyGrantPromo(user, *promo, now)
	if err != nil {
		cur := user.EffectivePlan(now)
		b.send(ctx, maxID, fmt.Sprintf(
			"🎟 У тебя уже активен тариф %s — этот код к нему не применить.\n"+
				"Код можно будет использовать, когда текущий тариф закончится.", cur.Title), kb)
		return
	}

	if err := b.promoRepo.RedeemGrant(ctx, promo.ID, user.ID, promo.Plan, expiresAt); err != nil {
		switch {
		case errors.Is(err, domain.ErrPromoAlreadyRedeemed):
			b.send(ctx, maxID, "🎟 Этот промокод ты уже активировал.", kb)
		case errors.Is(err, domain.ErrPromoExhausted):
			b.send(ctx, maxID, "🎟 Увы, лимит активаций этого промокода исчерпан.", kb)
		default:
			b.log.Error("max: promo redeem", "err", err, "code", promo.Code)
			b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		}
		return
	}

	plan, _ := domain.PlanByName(promo.Plan)
	b.restorePausedAfterUpgrade(ctx, user.ID, plan)

	b.send(ctx, maxID, fmt.Sprintf(
		"🎉 Промокод активирован!\n\nТариф: %s\n📦 Товаров: %d · 🔎 Поисков: %d\nДействует до %s.",
		plan.Title, plan.MaxProduct, plan.MaxSearch, expiresAt.Format(dateLayout)), kb)
}

// ── Рефералка ─────────────────────────────────────────────────────────────────

func (b *Bot) sendRef(ctx context.Context, maxID int64, user *domain.User) {
	kb := menuKeyboard(user)

	stats, err := b.referralRepo.Stats(ctx, user.ID)
	if err != nil {
		b.log.Error("max: referral stats", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	trialDays := int(domain.ReferralTrialDuration.Hours() / 24)
	botRef := "этого бота"
	if b.botURL != "" {
		botRef = b.botURL
	}

	text := fmt.Sprintf(
		"👥 Пригласи друга\n\n"+
			"Твой код приглашения: %d\n\n"+
			"Как это работает:\n"+
			"1. Друг открывает %s и пишет сообщение:\nдруг %d\n"+
			"2. Друг получает расширенный триал — %d %s вместо %d.\n"+
			"3. Когда друг освоится (поживёт пару дней с активной подпиской) — тебе +%d %s тарифа.\n"+
			"Когда друг оплатит тариф — тебе +%d %s (после запуска оплаты).\n\n"+
			"Потолок наград: %d дней в год.\n\n"+
			"📊 Приглашено: %d · Активировалось: %d · Начислено дней: %d",
		user.ID,
		botRef, user.ID,
		trialDays, domain.DaysWord(trialDays), int(domain.TrialDuration.Hours()/24),
		domain.ReferralActivatedRewardDays, domain.DaysWord(domain.ReferralActivatedRewardDays),
		domain.ReferralPaidRewardDays, domain.DaysWord(domain.ReferralPaidRewardDays),
		domain.ReferralYearlyCapDays,
		stats.Invited, stats.Activated, stats.DaysGranted)

	if user.TelegramID != 0 {
		text += fmt.Sprintf("\n\nДрузьям в Telegram удобнее ссылка:\nhttps://t.me/TryBerryBot?start=ref_%d", user.TelegramID)
	}
	b.send(ctx, maxID, text, kb)
}

func (b *Bot) handleRefCode(ctx context.Context, maxID int64, user *domain.User, arg string) {
	kb := menuKeyboard(user)

	refID, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || refID <= 0 {
		b.send(ctx, maxID, "Код приглашения — это число. Отправь сообщением:\nдруг КОД", kb)
		return
	}

	ok, err := b.referralRepo.SetReferrerByID(ctx, user.ID, refID, domain.ReferralAttributionWindow)
	if err != nil {
		b.log.Error("max: set referrer", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if !ok {
		b.send(ctx, maxID, "Не получилось засчитать приглашение 😕\n\n"+
			"Код работает только для новых аккаунтов (первые минуты после знакомства с ботом), "+
			"один раз и не на свой собственный код.", kb)
		return
	}

	trialDays := int(domain.ReferralTrialDuration.Hours() / 24)
	b.send(ctx, maxID, fmt.Sprintf(
		"🎉 Приглашение засчитано!\n\n"+
			"Тебе доступен расширенный триал — %d %s вместо %d. Жми «Триал» внизу, когда "+
			"захочешь попробовать поиск по ссылке.\n\n"+
			"А начать можно просто: отправь мне ссылку на любой товар WB 👇",
		trialDays, domain.DaysWord(trialDays), int(domain.TrialDuration.Hours()/24)), kb)
}
