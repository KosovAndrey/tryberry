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

const dateLayout = "02.01.2006 15:04"

func (b *Bot) isAdmin(tgID int64) bool {
	return b.adminIDs[tgID]
}

// ── Триал (пользовательская команда/кнопка) ──────────────────────────────────

func (b *Bot) handleTrial(ctx context.Context, chatID int64, user *domain.User) {
	now := time.Now()

	if user.TrialUsed {
		b.reply(chatID, "🎁 Триал уже был активирован ранее.\n\nПоиск по ссылке доступен на тарифе <b>Pro</b> — по вопросам пиши @kosov_andrey.")
		return
	}

	exp := now.Add(domain.TrialDuration)
	ok, err := b.userRepo.ActivateTrial(ctx, user.TelegramID, exp)
	if err != nil {
		b.log.Error("activate trial", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}
	if !ok {
		b.reply(chatID, "🎁 Триал уже был активирован ранее.")
		return
	}

	b.restorePausedAfterUpgrade(ctx, user.TelegramID)

	p := domain.Plans["trial"]
	b.reply(chatID, fmt.Sprintf(
		"🎁 <b>Триал активирован на 3 дня!</b>\n\n"+
			"Доступно поиск-подписок: <b>%d</b>.\n"+
			"Действует до <b>%s</b>.\n\n"+
			"Отправь ссылку на поисковую выдачу Wildberries, чтобы попробовать 🔎",
		p.MaxSearch, exp.Format(dateLayout)))
}

// ── Мой тариф ────────────────────────────────────────────────────────────────

func (b *Bot) handleMyPlan(ctx context.Context, chatID int64, user *domain.User) {
	now := time.Now()
	plan := user.EffectivePlan(now)

	prod, _ := b.subRepo.CountActiveByUserID(ctx, user.ID)
	srch, _ := b.searchSubRepo.CountActiveByUserID(ctx, user.ID)

	var sb strings.Builder
	fmt.Fprintf(&sb, "ℹ️ <b>Твой тариф: %s</b>\n\n", plan.Title)
	fmt.Fprintf(&sb, "📦 Товары: <b>%d из %d</b>\n", prod, plan.MaxProduct)
	fmt.Fprintf(&sb, "🔎 Поиски: <b>%d из %d</b>\n", srch, plan.MaxSearch)

	if user.PlanExpiresAt != nil && !user.PlanExpired(now) {
		fmt.Fprintf(&sb, "\n⏳ Действует до <b>%s</b>\n", user.PlanExpiresAt.Format(dateLayout))
	}
	if plan.MaxSearch == 0 && !user.TrialUsed {
		sb.WriteString("\n🎁 Доступен бесплатный триал поиска на 3 дня — /trial")
	}

	b.reply(chatID, sb.String())
}

// ── Админка ──────────────────────────────────────────────────────────────────

// /grant <telegram_id> <план> [дней]
func (b *Bot) handleGrant(ctx context.Context, msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		b.reply(msg.Chat.ID, "Неизвестная команда. Напиши /menu")
		return
	}
	args := strings.Fields(msg.CommandArguments())
	if len(args) < 2 {
		b.reply(msg.Chat.ID, "Использование: <code>/grant &lt;telegram_id&gt; &lt;план&gt; [дней]</code>\nПланы: free, trial, basic, pro, unlimited")
		return
	}
	tgID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.reply(msg.Chat.ID, "telegram_id должен быть числом.")
		return
	}
	planName := strings.ToLower(args[1])
	plan, ok := domain.PlanByName(planName)
	if !ok {
		b.reply(msg.Chat.ID, "Неизвестный план. Доступно: free, trial, basic, pro, unlimited.")
		return
	}

	var exp *time.Time
	if len(args) >= 3 {
		days, err := strconv.Atoi(args[2])
		if err != nil || days <= 0 {
			b.reply(msg.Chat.ID, "Срок (дней) должен быть положительным числом.")
			return
		}
		t := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		exp = &t
	}

	if err := b.userRepo.SetPlan(ctx, tgID, planName, exp); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.reply(msg.Chat.ID, "Пользователь не найден. Он должен хотя бы раз написать боту (/start).")
			return
		}
		b.log.Error("grant plan", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}

	b.restorePausedAfterUpgrade(ctx, tgID)

	txt := fmt.Sprintf("✅ Выдан тариф <b>%s</b> пользователю <code>%d</code>", plan.Title, tgID)
	if exp != nil {
		txt += fmt.Sprintf(" до <b>%s</b>", exp.Format(dateLayout))
	}
	b.reply(msg.Chat.ID, txt)
}

// restorePausedAfterUpgrade мгновенно возвращает паузные подписки пользователя
// (в пределах grace) до лимитов действующего плана — после выдачи/покупки тарифа.
// Reconciler в notifier сделал бы это и сам на ближайшем тике; хук убирает лаг.
// Ошибки только логируем: возврат не критичен для ответа пользователю.
func (b *Bot) restorePausedAfterUpgrade(ctx context.Context, telegramID int64) {
	user, err := b.userRepo.GetByTelegramID(ctx, telegramID)
	if err != nil {
		b.log.Error("restore after upgrade: get user", "err", err)
		return
	}
	now := time.Now()
	plan := user.EffectivePlan(now)
	cutoff := now.Add(-domain.PlanGracePeriod)

	activeS, _ := b.searchSubRepo.CountActiveByUserID(ctx, user.ID)
	if n, err := b.searchSubRepo.RestorePausedForUser(ctx, user.ID, plan.MaxSearch-activeS, cutoff); err != nil {
		b.log.Error("restore after upgrade: search", "err", err)
	} else if n > 0 {
		b.log.Info("restored paused search subs after upgrade", "user", user.ID, "count", n)
	}

	activeP, _ := b.subRepo.CountActiveByUserID(ctx, user.ID)
	if n, err := b.subRepo.RestorePausedForUser(ctx, user.ID, plan.MaxProduct-activeP, cutoff); err != nil {
		b.log.Error("restore after upgrade: products", "err", err)
	} else if n > 0 {
		b.log.Info("restored paused product subs after upgrade", "user", user.ID, "count", n)
	}
}

// /revoke <telegram_id>
func (b *Bot) handleRevoke(ctx context.Context, msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		b.reply(msg.Chat.ID, "Неизвестная команда. Напиши /menu")
		return
	}
	args := strings.Fields(msg.CommandArguments())
	if len(args) < 1 {
		b.reply(msg.Chat.ID, "Использование: <code>/revoke &lt;telegram_id&gt;</code>")
		return
	}
	tgID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.reply(msg.Chat.ID, "telegram_id должен быть числом.")
		return
	}
	if err := b.userRepo.SetPlan(ctx, tgID, "free", nil); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.reply(msg.Chat.ID, "Пользователь не найден.")
			return
		}
		b.log.Error("revoke plan", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}
	b.reply(msg.Chat.ID, fmt.Sprintf("✅ Тариф пользователя <code>%d</code> сброшен на <b>Free</b>.", tgID))
}

// /users — список с занятостью лимитов
func (b *Bot) handleUsers(ctx context.Context, msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		b.reply(msg.Chat.ID, "Неизвестная команда. Напиши /menu")
		return
	}
	list, err := b.userRepo.ListWithUsage(ctx, 50)
	if err != nil {
		b.log.Error("list users", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}
	if len(list) == 0 {
		b.reply(msg.Chat.ID, "Пользователей пока нет.")
		return
	}

	now := time.Now()
	var sb strings.Builder
	fmt.Fprintf(&sb, "👥 <b>Пользователи — %d</b> (по активности):\n\n", len(list))
	for _, u := range list {
		uname := u.Username
		if uname == "" {
			uname = "—"
		} else {
			uname = "@" + uname
		}
		plan := u.Plan
		if u.PlanExpiresAt != nil && now.After(*u.PlanExpiresAt) {
			plan = "free*" // срок истёк → фактически free
		}
		fmt.Fprintf(&sb, "<code>%d</code> %s · %s · 📦%d 🔎%d\n",
			u.TelegramID, htmlEscape(uname), plan, u.Products, u.Searches)
	}
	sb.WriteString("\n* срок плана истёк, действует Free")
	b.reply(msg.Chat.ID, sb.String())
}

// /whois <telegram_id>
func (b *Bot) handleWhois(ctx context.Context, msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		b.reply(msg.Chat.ID, "Неизвестная команда. Напиши /menu")
		return
	}
	args := strings.Fields(msg.CommandArguments())
	if len(args) < 1 {
		b.reply(msg.Chat.ID, "Использование: <code>/whois &lt;telegram_id&gt;</code>")
		return
	}
	tgID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.reply(msg.Chat.ID, "telegram_id должен быть числом.")
		return
	}
	u, err := b.userRepo.GetByTelegramID(ctx, tgID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.reply(msg.Chat.ID, "Пользователь не найден.")
			return
		}
		b.log.Error("whois", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}

	now := time.Now()
	plan := u.EffectivePlan(now)
	prod, _ := b.subRepo.CountActiveByUserID(ctx, u.ID)
	srch, _ := b.searchSubRepo.CountActiveByUserID(ctx, u.ID)

	uname := "—"
	if u.Username != "" {
		uname = "@" + u.Username
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "👤 <code>%d</code> %s\n\n", u.TelegramID, htmlEscape(uname))
	fmt.Fprintf(&sb, "Тариф: <b>%s</b> (в БД: %s)\n", plan.Title, u.Plan)
	fmt.Fprintf(&sb, "📦 Товары: %d из %d\n", prod, plan.MaxProduct)
	fmt.Fprintf(&sb, "🔎 Поиски: %d из %d\n", srch, plan.MaxSearch)
	fmt.Fprintf(&sb, "Триал использован: %v\n", u.TrialUsed)
	if u.PlanExpiresAt != nil {
		status := "активен"
		if now.After(*u.PlanExpiresAt) {
			status = "истёк"
		}
		fmt.Fprintf(&sb, "Срок: %s (%s)\n", u.PlanExpiresAt.Format(dateLayout), status)
	}
	b.reply(msg.Chat.ID, sb.String())
}
