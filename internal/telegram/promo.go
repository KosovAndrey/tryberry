package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// ── /promo CODE (и /start promo_CODE) ────────────────────────────────────────

// handlePromo — точка входа /promo КОД, deep-link start=promo_КОД и кнопки «🎟
// Промокод» в меню. Без кода (голый /promo или кнопка меню) запускаем диалог ввода;
// с кодом — сразу применяем через общий applyPromoCode (grant → дни, discount →
// ожидающая скидка + переход к оплате).
func (b *Bot) handlePromo(ctx context.Context, chatID int64, user *domain.User, codeArg string) {
	if domain.NormalizePromoCode(codeArg) == "" {
		b.promptCheckoutPromo(ctx, user.TelegramID, chatID, 0, "")
		return
	}
	b.applyPromoCode(ctx, chatID, user.TelegramID, user, codeArg, "")
}

func (b *Bot) applyGrantPromo(ctx context.Context, chatID int64, user *domain.User, promo *domain.PromoCode) {
	now := time.Now()

	expiresAt, err := domain.ApplyGrantPromo(user, *promo, now)
	if err != nil {
		cur := user.EffectivePlan(now)
		b.reply(chatID, fmt.Sprintf(
			"🎟 У тебя уже активен тариф <b>%s</b> — этот код к нему не применить.\n"+
				"Код можно будет использовать, когда текущий тариф закончится.",
			cur.Title))
		return
	}

	if err := b.promoRepo.RedeemGrant(ctx, promo.ID, user.ID, promo.Plan, expiresAt); err != nil {
		switch {
		case errors.Is(err, domain.ErrPromoAlreadyRedeemed):
			b.reply(chatID, "🎟 Этот промокод ты уже активировал.")
		case errors.Is(err, domain.ErrPromoExhausted):
			b.reply(chatID, "🎟 Увы, лимит активаций этого промокода исчерпан.")
		default:
			b.log.Error("promo: redeem", "err", err, "code", promo.Code)
			b.reply(chatID, "Произошла ошибка, попробуй позже.")
		}
		return
	}

	b.restorePausedAfterUpgrade(ctx, user.TelegramID)

	plan, _ := domain.PlanByName(promo.Plan)
	b.reply(chatID, fmt.Sprintf(
		"🎉 <b>Промокод активирован!</b>\n\n"+
			"Тариф: <b>%s</b>\n"+
			"📦 Товаров: <b>%d</b> · 🔎 Поисков: <b>%d</b>\n"+
			"Действует до <b>%s</b>.\n\n"+
			"Подробнее — /myplan",
		plan.Title, plan.MaxProduct, plan.MaxSearch, expiresAt.Format(dateLayout)))
}

// ── Админка ──────────────────────────────────────────────────────────────────

// /promo_create <код> grant <план> <дней> <макс_активаций> [срок_кода_дней]
// /promo_create <код> discount <процент> <макс_активаций> [срок_кода_дней]
func (b *Bot) handlePromoCreate(ctx context.Context, msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		b.reply(msg.Chat.ID, "Неизвестная команда. Напиши /menu")
		return
	}
	usage := "Использование:\n" +
		"<code>/promo_create КОД grant план дней макс_активаций [срок_кода_дней]</code>\n" +
		"<code>/promo_create КОД discount процент макс_активаций [срок_кода_дней]</code>\n\n" +
		"Примеры:\n<code>/promo_create LAUNCH7 grant pro 7 100</code>\n" +
		"<code>/promo_create SALE20 discount 20 50 30</code>"

	args := strings.Fields(msg.CommandArguments())
	if len(args) < 4 {
		b.reply(msg.Chat.ID, usage)
		return
	}

	p := domain.PromoCode{Code: domain.NormalizePromoCode(args[0]), Kind: strings.ToLower(args[1])}
	var rest []string

	switch p.Kind {
	case domain.PromoKindGrant:
		if len(args) < 5 {
			b.reply(msg.Chat.ID, usage)
			return
		}
		planName := strings.ToLower(args[2])
		plan, ok := domain.PlanByName(planName)
		if !ok || planName == "free" {
			b.reply(msg.Chat.ID, "Неизвестный план. Доступно: trial, lite, pro, reseller_start, reseller_pro, unlimited.")
			return
		}
		days, err := strconv.Atoi(args[3])
		if err != nil || days <= 0 {
			b.reply(msg.Chat.ID, "Дней должно быть положительным числом.")
			return
		}
		p.Plan, p.Days = plan.Name, days
		rest = args[4:]
	case domain.PromoKindDiscount:
		pct, err := strconv.Atoi(args[2])
		if err != nil || pct < 1 || pct > 99 {
			b.reply(msg.Chat.ID, "Процент скидки — число от 1 до 99.")
			return
		}
		p.DiscountPct = pct
		rest = args[3:]
	default:
		b.reply(msg.Chat.ID, usage)
		return
	}

	maxUses, err := strconv.Atoi(rest[0])
	if err != nil || maxUses <= 0 {
		b.reply(msg.Chat.ID, "Макс. активаций должно быть положительным числом.")
		return
	}
	p.MaxUses = maxUses

	if len(rest) >= 2 {
		days, err := strconv.Atoi(rest[1])
		if err != nil || days <= 0 {
			b.reply(msg.Chat.ID, "Срок кода (дней) должен быть положительным числом.")
			return
		}
		t := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		p.ExpiresAt = &t
	}

	if _, err := b.promoRepo.Create(ctx, p); err != nil {
		b.log.Error("promo create", "err", err, "code", p.Code)
		b.reply(msg.Chat.ID, "Не удалось создать код — возможно, такой уже существует.")
		return
	}

	txt := fmt.Sprintf("✅ Промокод <code>%s</code> создан: %s", p.Code, describePromo(p))
	b.reply(msg.Chat.ID, txt)
}

// /promo_off <код>
func (b *Bot) handlePromoOff(ctx context.Context, msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		b.reply(msg.Chat.ID, "Неизвестная команда. Напиши /menu")
		return
	}
	args := strings.Fields(msg.CommandArguments())
	if len(args) < 1 {
		b.reply(msg.Chat.ID, "Использование: <code>/promo_off КОД</code>")
		return
	}
	if err := b.promoRepo.SetActive(ctx, args[0], false); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.reply(msg.Chat.ID, "Код не найден.")
			return
		}
		b.log.Error("promo off", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}
	b.reply(msg.Chat.ID, fmt.Sprintf("✅ Код <code>%s</code> выключен.", domain.NormalizePromoCode(args[0])))
}

// /promo_list
func (b *Bot) handlePromoList(ctx context.Context, msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		b.reply(msg.Chat.ID, "Неизвестная команда. Напиши /menu")
		return
	}
	list, err := b.promoRepo.List(ctx, 30)
	if err != nil {
		b.log.Error("promo list", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}
	if len(list) == 0 {
		b.reply(msg.Chat.ID, "Промокодов пока нет. Создай: /promo_create")
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🎟 <b>Промокоды — %d</b> (новые сверху):\n\n", len(list))
	for _, p := range list {
		status := ""
		if !p.Active {
			status = " · ⛔️ выключен"
		} else if p.ExpiresAt != nil && time.Now().After(*p.ExpiresAt) {
			status = " · ⌛️ истёк"
		}
		fmt.Fprintf(&sb, "<code>%s</code> — %s · %d/%d%s\n",
			p.Code, describePromo(p), p.UsedCount, p.MaxUses, status)
	}
	b.reply(msg.Chat.ID, sb.String())
}

func describePromo(p domain.PromoCode) string {
	switch p.Kind {
	case domain.PromoKindGrant:
		title := p.Plan
		if plan, ok := domain.PlanByName(p.Plan); ok {
			title = plan.Title
		}
		return fmt.Sprintf("%s на %d дн.", title, p.Days)
	case domain.PromoKindDiscount:
		return fmt.Sprintf("скидка %d%%", p.DiscountPct)
	default:
		return p.Kind
	}
}
