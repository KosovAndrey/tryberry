package vk

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Промокоды и рефералка в VK. Логика та же, что в telegram/promo.go и
// telegram/referral.go; код приглашения в VK = users.id реферера (в TG —
// deep-link t.me/?start=ref_<telegram_id>, оба ведут на одну строку users).

// handlePromoCode — «промокод <КОД>» и «промокод» без кода. Без кода — запускаем
// диалог ввода (FSM), с кодом — сразу через общий applyPromoCode (grant → дни,
// discount → ожидающая скидка + переход к оплате).
func (b *Bot) handlePromoCode(ctx context.Context, vkID int64, user *domain.User, codeArg string) {
	if domain.NormalizePromoCode(codeArg) == "" {
		b.promptPromo(ctx, vkID, user, "")
		return
	}
	b.applyPromoCode(ctx, vkID, user, codeArg, "")
}

func (b *Bot) applyGrantPromo(ctx context.Context, vkID int64, user *domain.User, promo *domain.PromoCode, kb *Keyboard) {
	now := time.Now()

	expiresAt, err := domain.ApplyGrantPromo(user, *promo, now)
	if err != nil {
		cur := user.EffectivePlan(now)
		b.send(ctx, vkID, fmt.Sprintf(
			"🎟 У тебя уже активен тариф %s — этот код к нему не применить.\n"+
				"Код можно будет использовать, когда текущий тариф закончится.", cur.Title), kb)
		return
	}

	if err := b.promoRepo.RedeemGrant(ctx, promo.ID, user.ID, promo.Plan, expiresAt); err != nil {
		switch {
		case errors.Is(err, domain.ErrPromoAlreadyRedeemed):
			b.send(ctx, vkID, "🎟 Этот промокод ты уже активировал.", kb)
		case errors.Is(err, domain.ErrPromoExhausted):
			b.send(ctx, vkID, "🎟 Увы, лимит активаций этого промокода исчерпан.", kb)
		default:
			b.log.Error("vk: promo redeem", "err", err, "code", promo.Code)
			b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		}
		return
	}

	plan := domain.Plans[promo.Plan]
	b.restorePausedAfterUpgrade(ctx, user.ID, plan)

	b.send(ctx, vkID, fmt.Sprintf(
		"🎉 Промокод активирован!\n\nТариф: %s\n📦 Товаров: %d · 🔎 Поисков: %d\nДействует до %s.",
		plan.Title, plan.MaxProduct, plan.MaxSearch, expiresAt.Format(dateLayout)), kb)
}

// ── Рефералка ─────────────────────────────────────────────────────────────────

// sendRef — код приглашения и статистика.
func (b *Bot) sendRef(ctx context.Context, vkID int64, user *domain.User) {
	kb := menuKeyboard(user.TelegramID != 0)

	stats, err := b.referralRepo.Stats(ctx, user.ID)
	if err != nil {
		b.log.Error("vk: referral stats", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
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
	b.send(ctx, vkID, text, kb)
}

// handleRefCode — «друг <КОД>» от приглашённого: атрибуция «кто привёл».
// Защита в SetReferrerByID: только свежий аккаунт (окно атрибуции), один раз,
// не на себя.
func (b *Bot) handleRefCode(ctx context.Context, vkID int64, user *domain.User, arg string) {
	kb := menuKeyboard(user.TelegramID != 0)

	refID, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || refID <= 0 {
		b.send(ctx, vkID, "Код приглашения — это число. Отправь сообщением:\nдруг КОД", kb)
		return
	}

	ok, err := b.referralRepo.SetReferrerByID(ctx, user.ID, refID, domain.ReferralAttributionWindow)
	if err != nil {
		b.log.Error("vk: set referrer", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if !ok {
		b.send(ctx, vkID, "Не получилось засчитать приглашение 😕\n\n"+
			"Код работает только для новых аккаунтов (первые минуты после знакомства с ботом), "+
			"один раз и не на свой собственный код.", kb)
		return
	}

	trialDays := int(domain.ReferralTrialDuration.Hours() / 24)
	b.send(ctx, vkID, fmt.Sprintf(
		"🎉 Приглашение засчитано!\n\n"+
			"Тебе доступен расширенный триал — %d %s вместо %d. Жми «Триал» внизу, когда "+
			"захочешь попробовать поиск по ссылке.\n\n"+
			"А начать можно просто: отправь мне ссылку на любой товар WB 👇",
		trialDays, domain.DaysWord(trialDays), int(domain.TrialDuration.Hours()/24)), kb)
}
