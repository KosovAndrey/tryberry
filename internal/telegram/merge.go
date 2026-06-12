package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Слияние аккаунтов из TG: код выдан в VK (vk2tg), оба аккаунта непустые.
// Состояние между нажатиями кнопок живёт в Redis (как FSM-ы), коллбэки merge:*.

func mergeKey(tgID int64) string { return fmt.Sprintf("merge_fsm:%d", tgID) }

func (b *Bot) getMergePending(ctx context.Context, tgID int64) (domain.MergePending, bool) {
	if b.rdb == nil {
		return domain.MergePending{}, false
	}
	raw, err := b.rdb.Get(ctx, mergeKey(tgID)).Result()
	if err != nil {
		return domain.MergePending{}, false
	}
	var p domain.MergePending
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return domain.MergePending{}, false
	}
	return p, true
}

func (b *Bot) setMergePending(ctx context.Context, tgID int64, p domain.MergePending) error {
	if b.rdb == nil {
		return errors.New("redis unavailable")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, mergeKey(tgID), raw, fsmTTL).Err()
}

func (b *Bot) clearMergePending(ctx context.Context, tgID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, mergeKey(tgID))
}

// startMergeFlow — оба аккаунта непустые: предлагаем объединение.
// kept — аккаунт, выдавший код (VK-сторона), absorbed — текущий TG-аккаунт.
func (b *Bot) startMergeFlow(ctx context.Context, chatID int64, actor *domain.User, keptID int64) {
	kept, err := b.userRepo.GetByID(ctx, keptID)
	if err != nil {
		b.log.Error("merge: get kept user", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	dec := domain.ComputeMerge(kept, actor, time.Now())
	pending := domain.MergePending{KeptID: keptID, AbsorbedID: actor.ID, Decision: dec}
	if err := b.setMergePending(ctx, actor.TelegramID, pending); err != nil {
		b.reply(chatID, "Не получилось начать объединение (нет связи с хранилищем). Попробуй позже.")
		return
	}

	intro := "⚠️ <b>На обоих аккаунтах уже есть данные</b> (подписки или тариф), простая привязка невозможна.\n\n" +
		"Можно <b>объединить</b> аккаунты: подписки сложатся (дубли уберутся), Telegram и VK станут одним аккаунтом. " +
		"Объединение нельзя отменить.\n\n"

	if !dec.NeedChoice {
		opt := dec.Options[0]
		text := intro + "Итоговый тариф: <b>" + domain.MergeOptionLabel(opt) + "</b>"
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ Объединить", "merge:confirm:0"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "merge:cancel"),
			),
		)
		m := tgbotapi.NewMessage(chatID, text)
		m.ParseMode = "HTML"
		m.ReplyMarkup = keyboard
		b.send(m)
		return
	}

	b.sendMergeChoice(chatID, 0, intro, dec)
}

// sendMergeChoice — экран выбора тарифа (оба платных, разные планы).
func (b *Bot) sendMergeChoice(chatID int64, messageID int, intro string, dec domain.MergeDecision) {
	text := intro + "На аккаунтах разные платные тарифы — выбери, какой оставить " +
		"(остаток второго конвертируется в дни по соотношению цен):\n\n" +
		"1️⃣ <b>" + domain.MergeOptionLabel(dec.Options[0]) + "</b>\n" +
		"2️⃣ <b>" + domain.MergeOptionLabel(dec.Options[1]) + "</b>"
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("1️⃣ "+domain.MergeOptionLabel(dec.Options[0]), "merge:opt:0"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("2️⃣ "+domain.MergeOptionLabel(dec.Options[1]), "merge:opt:1"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "merge:cancel"),
		),
	)
	b.showView(chatID, messageID, text, keyboard)
}

// handleMergeCallback — merge:opt:<i> | merge:confirm:<i> | merge:back | merge:cancel.
func (b *Bot) handleMergeCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	chatID := cb.Message.Chat.ID
	messageID := cb.Message.MessageID

	if cb.Data == "merge:cancel" {
		b.clearMergePending(ctx, cb.From.ID)
		b.editMenu(chatID, messageID, "Объединение отменено. Аккаунты остались как были.", backToMenuKeyboard())
		b.answerCallback(cb.ID, "Отменено")
		return
	}

	pending, ok := b.getMergePending(ctx, cb.From.ID)
	if !ok {
		b.editMenu(chatID, messageID,
			"⏳ Сессия объединения истекла. Получи новый код в VK-боте и отправь его ещё раз.",
			backToMenuKeyboard())
		b.answerCallback(cb.ID, "")
		return
	}

	switch {
	case cb.Data == "merge:back":
		b.sendMergeChoice(chatID, messageID, "", pending.Decision)
		b.answerCallback(cb.ID, "")

	case strings.HasPrefix(cb.Data, "merge:opt:"):
		i, err := strconv.Atoi(strings.TrimPrefix(cb.Data, "merge:opt:"))
		if err != nil || i < 0 || i >= len(pending.Decision.Options) {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		label := domain.MergeOptionLabel(pending.Decision.Options[i])
		text := "Проверь ещё раз 👇\n\nИтоговый тариф: <b>" + label + "</b>\n\n" +
			"Подписки обоих аккаунтов объединятся, объединение нельзя отменить."
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ Подтверждаю", fmt.Sprintf("merge:confirm:%d", i)),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("◀️ Назад", "merge:back"),
				tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "merge:cancel"),
			),
		)
		b.editMenu(chatID, messageID, text, keyboard)
		b.answerCallback(cb.ID, "")

	case strings.HasPrefix(cb.Data, "merge:confirm:"):
		i, err := strconv.Atoi(strings.TrimPrefix(cb.Data, "merge:confirm:"))
		if err != nil || i < 0 || i >= len(pending.Decision.Options) {
			b.answerCallback(cb.ID, "Ошибка")
			return
		}
		b.executeMerge(ctx, chatID, messageID, cb.From.ID, pending, i, "tg")
		b.answerCallback(cb.ID, "")

	default:
		b.answerCallback(cb.ID, "Ошибка")
	}
}

// executeMerge — финальное слияние после подтверждения.
func (b *Bot) executeMerge(ctx context.Context, chatID int64, messageID int, tgID int64, pending domain.MergePending, chosen int, platform string) {
	opt := pending.Decision.Options[chosen]

	detail := mergeDetail(ctx, b.userRepo, pending, chosen, platform)
	if err := b.userRepo.MergeAccounts(ctx, pending.KeptID, pending.AbsorbedID, opt.Plan, opt.ExpiresAt, detail); err != nil {
		b.log.Error("merge: execute", "kept", pending.KeptID, "absorbed", pending.AbsorbedID, "err", err)
		b.editMenu(chatID, messageID, "Произошла ошибка, объединение не выполнено. Попробуй позже.", backToMenuKeyboard())
		return
	}
	b.clearMergePending(ctx, tgID)
	b.log.Info("accounts merged", "kept", pending.KeptID, "absorbed", pending.AbsorbedID,
		"plan", opt.Plan, "chosen", chosen, "initiated_in", platform)

	b.editMenu(chatID, messageID,
		"🎉 <b>Аккаунты объединены!</b>\n\n"+
			"Тариф: <b>"+domain.MergeOptionLabel(opt)+"</b>\n"+
			"Telegram и VK теперь один аккаунт: подписки общие, уведомления настраиваются в Профиле.",
		backToMenuKeyboard())
}

// mergeDetail — JSON-снапшот для аудита (account_merges.detail): идентичности
// сторон до слияния, варианты и выбор. Ошибки чтения не блокируют слияние.
func mergeDetail(ctx context.Context, users interface {
	GetByID(ctx context.Context, id int64) (*domain.User, error)
}, pending domain.MergePending, chosen int, platform string) []byte {
	d := map[string]any{
		"initiated_in": platform,
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
		return m
	}
	if kept, err := users.GetByID(ctx, pending.KeptID); err == nil {
		d["kept"] = ident(kept)
	}
	if absorbed, err := users.GetByID(ctx, pending.AbsorbedID); err == nil {
		d["absorbed"] = ident(absorbed)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return []byte("{}")
	}
	return raw
}
