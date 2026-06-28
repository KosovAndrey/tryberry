package max

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Слияние аккаунтов из MAX: код выдан в TG/VK, оба аккаунта непустые. Зеркало
// vk/merge.go: состояние в Redis, кнопки — payload cmd=merge.

func mergeKey(maxID int64) string { return fmt.Sprintf("max_merge_fsm:%d", maxID) }

func (b *Bot) getMergePending(ctx context.Context, maxID int64) (domain.MergePending, bool) {
	if b.rdb == nil {
		return domain.MergePending{}, false
	}
	raw, err := b.rdb.Get(ctx, mergeKey(maxID)).Result()
	if err != nil {
		return domain.MergePending{}, false
	}
	var p domain.MergePending
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return domain.MergePending{}, false
	}
	return p, true
}

func (b *Bot) setMergePending(ctx context.Context, maxID int64, p domain.MergePending) error {
	if b.rdb == nil {
		return errors.New("redis unavailable")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, mergeKey(maxID), raw, fsmTTL).Err()
}

func (b *Bot) clearMergePending(ctx context.Context, maxID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, mergeKey(maxID))
}

func mergeButton(label, action string) Button {
	return TextButton(label, fmt.Sprintf(`{"cmd":"merge","k":%q}`, action), ColorSecondary)
}

func (b *Bot) menuFor(ctx context.Context, maxID int64, fallback *domain.User) *Keyboard {
	if u, err := b.userRepo.GetByMaxID(ctx, maxID); err == nil && u != nil {
		return menuKeyboard(u)
	}
	return menuKeyboard(fallback)
}

// startMergeFlow — оба аккаунта непустые: предлагаем объединение. kept — аккаунт,
// выдавший код (TG/VK-сторона), absorbed — текущий MAX-аккаунт.
func (b *Bot) startMergeFlow(ctx context.Context, maxID int64, actor *domain.User, keptID int64) {
	kept, err := b.userRepo.GetByID(ctx, keptID)
	if err != nil {
		b.log.Error("max: merge get kept user", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	dec := domain.ComputeMerge(kept, actor, time.Now())
	pending := domain.MergePending{KeptID: keptID, AbsorbedID: actor.ID, Decision: dec}
	if err := b.setMergePending(ctx, maxID, pending); err != nil {
		b.send(ctx, maxID, "Не получилось начать объединение (нет связи с хранилищем). Попробуй позже.", nil)
		return
	}

	intro := "⚠️ На обоих аккаунтах уже есть данные (подписки или тариф), простая привязка невозможна.\n\n" +
		"Можно объединить аккаунты: подписки сложатся (дубли уберутся), аккаунты станут одним. " +
		"Объединение нельзя отменить.\n\n"

	if !dec.NeedChoice {
		opt := dec.Options[0]
		kb := &Keyboard{Buttons: [][]Button{
			{mergeButton("✅ Объединить", "confirm:0")},
			{mergeButton("❌ Отмена", "cancel")},
		}}
		b.send(ctx, maxID, intro+"Итоговый тариф: "+domain.MergeOptionLabel(opt), kb)
		return
	}
	b.sendMergeChoice(ctx, maxID, intro, dec)
}

func (b *Bot) sendMergeChoice(ctx context.Context, maxID int64, intro string, dec domain.MergeDecision) {
	text := intro + "На аккаунтах разные платные тарифы — выбери, какой оставить " +
		"(остаток второго конвертируется в дни по соотношению цен):\n\n" +
		"1️⃣ " + domain.MergeOptionLabel(dec.Options[0]) + "\n" +
		"2️⃣ " + domain.MergeOptionLabel(dec.Options[1])
	kb := &Keyboard{Buttons: [][]Button{
		{mergeButton("1️⃣ "+domain.MergeOptionLabel(dec.Options[0]), "opt:0")},
		{mergeButton("2️⃣ "+domain.MergeOptionLabel(dec.Options[1]), "opt:1")},
		{mergeButton("❌ Отмена", "cancel")},
	}}
	b.send(ctx, maxID, text, kb)
}

func (b *Bot) handleMergeAction(ctx context.Context, maxID int64, user *domain.User, action string) {
	if action == "cancel" {
		b.clearMergePending(ctx, maxID)
		b.send(ctx, maxID, "Объединение отменено. Аккаунты остались как были.", menuKeyboard(user))
		return
	}

	pending, ok := b.getMergePending(ctx, maxID)
	if !ok {
		b.send(ctx, maxID, "⏳ Сессия объединения истекла. Получи новый код в Telegram/VK-боте и отправь его ещё раз.",
			menuKeyboard(user))
		return
	}

	switch {
	case action == "back":
		b.sendMergeChoice(ctx, maxID, "", pending.Decision)

	case strings.HasPrefix(action, "opt:"):
		i, err := strconv.Atoi(strings.TrimPrefix(action, "opt:"))
		if err != nil || i < 0 || i >= len(pending.Decision.Options) {
			return
		}
		label := domain.MergeOptionLabel(pending.Decision.Options[i])
		kb := &Keyboard{Buttons: [][]Button{
			{mergeButton("✅ Подтверждаю", fmt.Sprintf("confirm:%d", i))},
			{mergeButton("◀️ Назад", "back"), mergeButton("❌ Отмена", "cancel")},
		}}
		b.send(ctx, maxID, "Проверь ещё раз 👇\n\nИтоговый тариф: "+label+"\n\n"+
			"Подписки обоих аккаунтов объединятся, объединение нельзя отменить.", kb)

	case strings.HasPrefix(action, "confirm:"):
		i, err := strconv.Atoi(strings.TrimPrefix(action, "confirm:"))
		if err != nil || i < 0 || i >= len(pending.Decision.Options) {
			return
		}
		opt := pending.Decision.Options[i]
		detail := b.mergeDetail(ctx, pending, i)
		if err := b.userRepo.MergeAccounts(ctx, pending.KeptID, pending.AbsorbedID, opt.Plan, opt.ExpiresAt, detail); err != nil {
			b.log.Error("max: merge execute", "kept", pending.KeptID, "absorbed", pending.AbsorbedID, "err", err)
			b.send(ctx, maxID, "Произошла ошибка, объединение не выполнено. Попробуй позже.", nil)
			return
		}
		b.clearMergePending(ctx, maxID)
		b.log.Info("accounts merged", "kept", pending.KeptID, "absorbed", pending.AbsorbedID,
			"plan", opt.Plan, "chosen", i, "initiated_in", "max")
		b.send(ctx, maxID, "🎉 Аккаунты объединены!\n\n"+
			"Тариф: "+domain.MergeOptionLabel(opt)+"\n"+
			"Аккаунты теперь общие: подписки общие, уведомления настраиваются в Профиле.",
			b.menuFor(ctx, maxID, user))
	}
}

func (b *Bot) mergeDetail(ctx context.Context, pending domain.MergePending, chosen int) []byte {
	d := map[string]any{
		"initiated_in": "max",
		"chosen":       chosen,
		"options":      pending.Decision.Options,
		"need_choice":  pending.Decision.NeedChoice,
	}
	ident := func(u *domain.User) map[string]any {
		m := map[string]any{}
		if u.TelegramID != 0 {
			m["tg"] = u.TelegramID
		}
		if u.VKID != nil {
			m["vk"] = *u.VKID
		}
		if u.MaxID != nil {
			m["max"] = *u.MaxID
		}
		return m
	}
	if kept, err := b.userRepo.GetByID(ctx, pending.KeptID); err == nil {
		d["kept"] = ident(kept)
	}
	if absorbed, err := b.userRepo.GetByID(ctx, pending.AbsorbedID); err == nil {
		d["absorbed"] = ident(absorbed)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return []byte("{}")
	}
	return raw
}
