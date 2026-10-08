package max

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

// Админ-команды MAX — зеркало telegram/vk admin. Оператор (max_id из
// MAX_ADMIN_IDS) управляет тарифами, пользователями и промокодами. Целевой
// пользователь адресуется telegram_id (как в TG/VK). Plain-text.

func (b *Bot) notAdmin(ctx context.Context, maxID int64) {
	b.send(ctx, maxID, "Неизвестная команда. Напиши «помощь».", nil)
}

func (b *Bot) handleSlashCommand(ctx context.Context, maxID int64, user *domain.User, text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return false
	}
	cmd := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	args := fields[1:]
	arg := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))

	known := map[string]bool{
		"grant": true, "revoke": true, "extend": true, "users": true, "whois": true,
		"promo_create": true, "promo_off": true, "promo_list": true, "myplan": true,
		"start": true, "menu": true, "help": true, "list": true, "plans": true,
		"trial": true, "promo": true, "ref": true, "profile": true,
		"track": true, "track_search": true, "list_search": true,
	}
	if !known[cmd] {
		return false
	}
	b.clearSearchFSM(ctx, maxID)
	b.clearTrackFSM(ctx, maxID)
	b.clearEmailFSM(ctx, maxID)
	b.clearPromoFSM(ctx, maxID)
	metrics.MaxMessages.WithLabelValues("slash_" + cmd).Inc()

	kb := menuKeyboard(user)
	switch cmd {
	// ── Админ ──
	case "grant":
		b.handleGrant(ctx, maxID, args)
	case "revoke":
		b.handleRevoke(ctx, maxID, args)
	case "extend":
		b.handleExtend(ctx, maxID, args)
	case "users":
		b.handleUsers(ctx, maxID)
	case "whois":
		b.handleWhois(ctx, maxID, args)
	case "promo_create":
		b.handlePromoCreate(ctx, maxID, args)
	case "promo_off":
		b.handlePromoOff(ctx, maxID, args)
	case "promo_list":
		b.handlePromoList(ctx, maxID)
	// ── Пользовательские ──
	case "myplan":
		b.sendMyPlan(ctx, maxID, user)
	case "start", "menu":
		b.send(ctx, maxID, b.welcomeText(user), kb)
	case "help":
		b.send(ctx, maxID, b.helpText(user), kb)
	case "list":
		b.handleList(ctx, maxID, user, "")
	case "list_search":
		b.handleListSearch(ctx, maxID, user, "")
	case "plans":
		b.sendPlans(ctx, maxID, user)
	case "trial":
		b.handleTrial(ctx, maxID, user)
	case "ref":
		b.sendRef(ctx, maxID, user)
	case "profile":
		b.sendProfile(ctx, maxID, user)
	case "promo":
		if arg != "" {
			b.handlePromoCode(ctx, maxID, user, arg)
		} else {
			b.send(ctx, maxID, "🎟 Отправь код сообщением: промокод КОД", kb)
		}
	case "track":
		if arg != "" {
			b.handleTrack(ctx, maxID, user, arg)
		} else {
			b.send(ctx, maxID, "➕ Отправь ссылку на товар Wildberries — начну отслеживать цену.", kb)
		}
	case "track_search":
		if arg != "" {
			b.startSearchTrack(ctx, maxID, user, arg)
		} else {
			b.send(ctx, maxID, "🔎 Отправь ссылку на поисковую выдачу Wildberries.", kb)
		}
	}
	return true
}

// ── Мой тариф ─────────────────────────────────────────────────────────────────

func (b *Bot) sendMyPlan(ctx context.Context, maxID int64, user *domain.User) {
	now := time.Now()
	plan := user.EffectivePlan(now)

	prod, _ := b.subRepo.CountActiveByUserID(ctx, user.ID)
	srch, _ := b.searchSubRepo.CountActiveByUserID(ctx, user.ID)

	var sb strings.Builder
	fmt.Fprintf(&sb, "ℹ️ Твой тариф: %s\n\n", plan.Title)
	fmt.Fprintf(&sb, "📦 Товары: %d из %d\n", prod, plan.MaxProduct)
	fmt.Fprintf(&sb, "🔎 Поиски: %d из %d", srch, plan.MaxSearch)
	if si := plan.EffectiveSearchInterval(plan.Interval); plan.MaxSearch > 0 && si != plan.Interval {
		fmt.Fprintf(&sb, " · проверка %s", domain.IntervalPhrase(si))
	}
	fmt.Fprintf(&sb, "\n🕒 Интервал проверки: %d мин\n", int(plan.Interval.Minutes()))
	if plan.PriceRub > 0 {
		fmt.Fprintf(&sb, "💳 Цена: %d ₽/мес\n", plan.PriceRub)
	}
	if user.PlanExpiresAt != nil && !user.PlanExpired(now) {
		fmt.Fprintf(&sb, "\n⏳ Действует до %s\n", user.PlanExpiresAt.Format(dateLayout))
	}

	subLine, hasSub := b.subscriptionLine(ctx, user.ID)
	sb.WriteString(subLine)

	var rows [][]Button
	if hasSub {
		rows = append(rows, []Button{TextButton("🚫 Отменить автопродление", buttonPayload(cmdSubCancel), ColorSecondary)})
	}
	if plan.ShowTrialOffer() && !user.TrialUsed {
		sb.WriteString("\n🎁 Тебе доступен бесплатный триал поиска — кнопка ниже.")
		rows = append(rows, []Button{TextButton("🎁 Активировать триал", buttonPayload(cmdTrial), ColorPrimary)})
	}
	rows = append(rows, []Button{TextButton("💳 Тарифы", buttonPayload(cmdPlans), ColorSecondary)})
	rows = append(rows, []Button{
		TextButton("👥 Пригласить друга", buttonPayload(cmdRef), ColorSecondary),
		TextButton("🎟 Промокод", buttonPayload(cmdPromo), ColorSecondary),
	})
	b.send(ctx, maxID, sb.String(), &Keyboard{Buttons: rows})
}

// ── Тарифы пользователей ─────────────────────────────────────────────────────

func (b *Bot) handleGrant(ctx context.Context, maxID int64, args []string) {
	if !b.isAdmin(maxID) {
		b.notAdmin(ctx, maxID)
		return
	}
	if len(args) < 2 {
		b.send(ctx, maxID, "Использование: /grant <telegram_id> <план> [дней]\nПланы: free, trial, lite, pro, reseller_start, reseller_pro, unlimited, pro_plus_s<поиски>_p<товары>, reseller_pro_plus_s<поиски>_p<товары>", nil)
		return
	}
	tgID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.send(ctx, maxID, "telegram_id должен быть числом.", nil)
		return
	}
	planName := strings.ToLower(args[1])
	plan, ok := domain.PlanByName(planName)
	if !ok {
		b.send(ctx, maxID, "Неизвестный план. Доступно: free, trial, lite, pro, reseller_start, reseller_pro, unlimited, pro_plus_s<поиски>_p<товары>, reseller_pro_plus_s<поиски>_p<товары>.", nil)
		return
	}

	var exp *time.Time
	if len(args) >= 3 {
		days, err := strconv.Atoi(args[2])
		if err != nil || days <= 0 {
			b.send(ctx, maxID, "Срок (дней) должен быть положительным числом.", nil)
			return
		}
		t := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		exp = &t
	}

	if err := b.userRepo.SetPlan(ctx, tgID, planName, exp); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.send(ctx, maxID, "Пользователь не найден. Он должен хотя бы раз написать боту.", nil)
			return
		}
		b.log.Error("max grant plan", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	b.restorePausedForTelegram(ctx, tgID)

	txt := fmt.Sprintf("✅ Выдан тариф %s пользователю %d", plan.Title, tgID)
	if exp != nil {
		txt += fmt.Sprintf(" до %s", exp.Format(dateLayout))
	}
	b.send(ctx, maxID, txt, nil)
}

func (b *Bot) handleRevoke(ctx context.Context, maxID int64, args []string) {
	if !b.isAdmin(maxID) {
		b.notAdmin(ctx, maxID)
		return
	}
	if len(args) < 1 {
		b.send(ctx, maxID, "Использование: /revoke <telegram_id>", nil)
		return
	}
	tgID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.send(ctx, maxID, "telegram_id должен быть числом.", nil)
		return
	}
	if err := b.userRepo.SetPlan(ctx, tgID, "free", nil); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.send(ctx, maxID, "Пользователь не найден.", nil)
			return
		}
		b.log.Error("max revoke plan", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	subNote := ""
	if b.billing != nil {
		if u, err := b.userRepo.GetByTelegramID(ctx, tgID); err == nil {
			if canceled, err := b.billing.Cancel(ctx, u.ID); err != nil {
				b.log.Error("max revoke: cancel subscription", "err", err)
			} else if canceled {
				subNote = " Автопродление подписки отменено."
			}
		}
	}
	b.send(ctx, maxID, fmt.Sprintf("✅ Тариф пользователя %d сброшен на Free.%s", tgID, subNote), nil)
}

func (b *Bot) handleExtend(ctx context.Context, maxID int64, args []string) {
	if !b.isAdmin(maxID) {
		b.notAdmin(ctx, maxID)
		return
	}
	if len(args) < 2 {
		b.send(ctx, maxID, "Использование: /extend <telegram_id> <дней>\nПродлевает текущий тариф на N дней (можно отрицательное). Новый тариф — через /grant.", nil)
		return
	}
	tgID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.send(ctx, maxID, "telegram_id должен быть числом.", nil)
		return
	}
	days, err := strconv.Atoi(args[1])
	if err != nil || days == 0 {
		b.send(ctx, maxID, "Число дней должно быть ненулевым целым.", nil)
		return
	}

	u, err := b.userRepo.GetByTelegramID(ctx, tgID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.send(ctx, maxID, "Пользователь не найден.", nil)
			return
		}
		b.log.Error("max extend: get user", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if u.Plan == "free" {
		b.send(ctx, maxID, "У пользователя нет платного тарифа — продлевать нечего. Выдай тариф через /grant.", nil)
		return
	}

	now := time.Now()
	base := now
	if u.PlanExpiresAt != nil && u.PlanExpiresAt.After(now) {
		base = *u.PlanExpiresAt
	}
	newExp := base.Add(time.Duration(days) * 24 * time.Hour)
	if !newExp.After(now) {
		b.send(ctx, maxID, "После сокращения срок оказался бы в прошлом. Чтобы снять тариф — /revoke.", nil)
		return
	}

	if err := b.userRepo.SetPlan(ctx, tgID, u.Plan, &newExp); err != nil {
		b.log.Error("max extend plan", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	b.restorePausedForTelegram(ctx, tgID)

	plan, _ := domain.PlanByName(u.Plan)
	verb, d := "продлён на", days
	if days < 0 {
		verb, d = "сокращён на", -days
	}
	b.send(ctx, maxID, fmt.Sprintf("✅ Тариф %s пользователя %d %s %d %s — теперь до %s.",
		plan.Title, tgID, verb, d, domain.DaysWord(d), newExp.Format(dateLayout)), nil)
}

func (b *Bot) handleUsers(ctx context.Context, maxID int64) {
	if !b.isAdmin(maxID) {
		b.notAdmin(ctx, maxID)
		return
	}
	list, err := b.userRepo.ListWithUsage(ctx, 50)
	if err != nil {
		b.log.Error("max list users", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if len(list) == 0 {
		b.send(ctx, maxID, "Пользователей пока нет.", nil)
		return
	}

	now := time.Now()
	var sb strings.Builder
	fmt.Fprintf(&sb, "👥 Пользователи — %d (по активности):\n\n", len(list))
	for _, u := range list {
		uname := "—"
		if u.Username != "" {
			uname = "@" + u.Username
		}
		plan := u.Plan
		if u.PlanExpiresAt != nil && now.After(*u.PlanExpiresAt) {
			plan = "free*"
		}
		fmt.Fprintf(&sb, "%d %s · %s · 📦%d 🔎%d\n", u.TelegramID, uname, plan, u.Products, u.Searches)
	}
	sb.WriteString("\n* срок плана истёк, действует Free")
	b.send(ctx, maxID, sb.String(), nil)
}

func (b *Bot) handleWhois(ctx context.Context, maxID int64, args []string) {
	if !b.isAdmin(maxID) {
		b.notAdmin(ctx, maxID)
		return
	}
	if len(args) < 1 {
		b.send(ctx, maxID, "Использование: /whois <telegram_id>", nil)
		return
	}
	tgID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.send(ctx, maxID, "telegram_id должен быть числом.", nil)
		return
	}
	u, err := b.userRepo.GetByTelegramID(ctx, tgID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.send(ctx, maxID, "Пользователь не найден.", nil)
			return
		}
		b.log.Error("max whois", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
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
	fmt.Fprintf(&sb, "👤 %d %s\n\n", u.TelegramID, uname)
	fmt.Fprintf(&sb, "Тариф: %s (в БД: %s)\n", plan.Title, u.Plan)
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
	if b.billing != nil {
		sub, err := b.billing.GetActiveByUserID(ctx, u.ID)
		switch {
		case err == nil:
			sp, _ := domain.PlanByName(sub.Plan)
			st := "активна"
			if sub.Status == domain.SubStatusPastDue {
				st = "просрочено списание"
			}
			fmt.Fprintf(&sb, "\n🔁 Автоподписка: %s — %s, %s ₽, след. списание %s\n",
				st, sp.Title, domain.KopecksToRubString(sub.AmountKopecks), sub.NextChargeAt.Format(dateLayout))
			if sub.FailCount > 0 {
				fmt.Fprintf(&sb, "Неудачных списаний подряд: %d\n", sub.FailCount)
			}
		case errors.Is(err, domain.ErrNotFound):
			sb.WriteString("\n🔁 Автоподписка: нет активной\n")
		default:
			b.log.Error("max whois: billing sub", "err", err)
		}
	}
	b.send(ctx, maxID, sb.String(), nil)
}

func (b *Bot) restorePausedForTelegram(ctx context.Context, tgID int64) {
	u, err := b.userRepo.GetByTelegramID(ctx, tgID)
	if err != nil {
		b.log.Error("max restore after upgrade: get user", "err", err)
		return
	}
	b.restorePausedAfterUpgrade(ctx, u.ID, u.EffectivePlan(time.Now()))
}

// ── Промокоды (менеджмент) ───────────────────────────────────────────────────

func (b *Bot) handlePromoCreate(ctx context.Context, maxID int64, args []string) {
	if !b.isAdmin(maxID) {
		b.notAdmin(ctx, maxID)
		return
	}
	usage := "Использование:\n" +
		"/promo_create КОД grant план дней макс [срок_дней]\n" +
		"/promo_create КОД discount процент макс [срок_дней]\n\n" +
		"Примеры:\n/promo_create LAUNCH7 grant pro 7 100\n/promo_create SALE20 discount 20 50 30"
	if len(args) < 4 {
		b.send(ctx, maxID, usage, nil)
		return
	}

	p := domain.PromoCode{Code: domain.NormalizePromoCode(args[0]), Kind: strings.ToLower(args[1])}
	var rest []string

	switch p.Kind {
	case domain.PromoKindGrant:
		if len(args) < 5 {
			b.send(ctx, maxID, usage, nil)
			return
		}
		planName := strings.ToLower(args[2])
		plan, ok := domain.PlanByName(planName)
		if !ok || planName == "free" {
			b.send(ctx, maxID, "Неизвестный план. Доступно: trial, lite, pro, reseller_start, reseller_pro, unlimited, pro_plus_s<поиски>_p<товары>, reseller_pro_plus_s<поиски>_p<товары>.", nil)
			return
		}
		days, err := strconv.Atoi(args[3])
		if err != nil || days <= 0 {
			b.send(ctx, maxID, "Дней должно быть положительным числом.", nil)
			return
		}
		p.Plan, p.Days = plan.Name, days
		rest = args[4:]
	case domain.PromoKindDiscount:
		pct, err := strconv.Atoi(args[2])
		if err != nil || pct < 1 || pct > 99 {
			b.send(ctx, maxID, "Процент скидки — число от 1 до 99.", nil)
			return
		}
		p.DiscountPct = pct
		rest = args[3:]
	default:
		b.send(ctx, maxID, usage, nil)
		return
	}

	maxUses, err := strconv.Atoi(rest[0])
	if err != nil || maxUses <= 0 {
		b.send(ctx, maxID, "Макс. активаций должно быть положительным числом.", nil)
		return
	}
	p.MaxUses = maxUses

	if len(rest) >= 2 {
		days, err := strconv.Atoi(rest[1])
		if err != nil || days <= 0 {
			b.send(ctx, maxID, "Срок кода (дней) должен быть положительным числом.", nil)
			return
		}
		t := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		p.ExpiresAt = &t
	}

	if _, err := b.promoRepo.Create(ctx, p); err != nil {
		b.log.Error("max promo create", "err", err, "code", p.Code)
		b.send(ctx, maxID, "Не удалось создать код — возможно, такой уже существует.", nil)
		return
	}
	b.send(ctx, maxID, fmt.Sprintf("✅ Промокод %s создан: %s", p.Code, describePromo(p)), nil)
}

func (b *Bot) handlePromoOff(ctx context.Context, maxID int64, args []string) {
	if !b.isAdmin(maxID) {
		b.notAdmin(ctx, maxID)
		return
	}
	if len(args) < 1 {
		b.send(ctx, maxID, "Использование: /promo_off КОД", nil)
		return
	}
	if err := b.promoRepo.SetActive(ctx, args[0], false); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.send(ctx, maxID, "Код не найден.", nil)
			return
		}
		b.log.Error("max promo off", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	b.send(ctx, maxID, fmt.Sprintf("✅ Код %s выключен.", domain.NormalizePromoCode(args[0])), nil)
}

func (b *Bot) handlePromoList(ctx context.Context, maxID int64) {
	if !b.isAdmin(maxID) {
		b.notAdmin(ctx, maxID)
		return
	}
	list, err := b.promoRepo.List(ctx, 30)
	if err != nil {
		b.log.Error("max promo list", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if len(list) == 0 {
		b.send(ctx, maxID, "Промокодов пока нет. Создай: /promo_create", nil)
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🎟 Промокоды — %d (новые сверху):\n\n", len(list))
	for _, p := range list {
		status := ""
		if !p.Active {
			status = " · ⛔️ выключен"
		} else if p.ExpiresAt != nil && time.Now().After(*p.ExpiresAt) {
			status = " · ⌛️ истёк"
		}
		fmt.Fprintf(&sb, "%s — %s · %d/%d%s\n", p.Code, describePromo(p), p.UsedCount, p.MaxUses, status)
	}
	b.send(ctx, maxID, sb.String(), nil)
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
