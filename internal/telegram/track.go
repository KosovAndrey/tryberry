package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// trackFSM — состояние диалога настройки стратегии для ТОВАРНОЙ подписки:
// ждём число (целевую цену или процент) для уже созданной подписки SubID.
type trackFSM struct {
	SubID   int64  `json:"s"`
	Trigger string `json:"t"` // domain.TriggerBelowTarget | domain.TriggerDiscountPct
}

func trackFSMKey(tgID int64) string { return fmt.Sprintf("track_fsm:%d", tgID) }

func (b *Bot) getTrackFSM(ctx context.Context, tgID int64) (trackFSM, bool) {
	if b.rdb == nil {
		return trackFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, trackFSMKey(tgID)).Result()
	if err != nil {
		return trackFSM{}, false
	}
	var fsm trackFSM
	if err := json.Unmarshal([]byte(raw), &fsm); err != nil {
		return trackFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setTrackFSM(ctx context.Context, tgID int64, fsm trackFSM) error {
	if b.rdb == nil {
		return errors.New("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, trackFSMKey(tgID), raw, fsmTTL).Err()
}

func (b *Bot) clearTrackFSM(ctx context.Context, tgID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, trackFSMKey(tgID))
}

// trackTriggerKeyboard — клавиатура выбора стратегии под сообщением товара.
// Текущая стратегия помечается галочкой. Снизу — переходы в меню.
func trackTriggerKeyboard(subID int64, current domain.TriggerType) tgbotapi.InlineKeyboardMarkup {
	mark := func(label string, t domain.TriggerType) string {
		if current == t {
			return "✅ " + label
		}
		return label
	}
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				mark("🔻 Любое снижение", domain.TriggerAnyDrop),
				fmt.Sprintf("ptrack:%d:any", subID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				mark("📉 Ниже цены", domain.TriggerBelowTarget),
				fmt.Sprintf("ptrack:%d:below", subID)),
			tgbotapi.NewInlineKeyboardButtonData(
				mark("％ Скидка %", domain.TriggerDiscountPct),
				fmt.Sprintf("ptrack:%d:disc", subID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Мои подписки", "menu:list"),
			tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
		),
	)
}

// handleTrackTriggerCallback — обработка нажатия ptrack:<subID>:<kind>.
func (b *Bot) handleTrackTriggerCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	parts := strings.Split(strings.TrimPrefix(cb.Data, "ptrack:"), ":")
	if len(parts) != 2 {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	subID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	kind := parts[1]
	chatID := cb.Message.Chat.ID

	// subID берётся из callback_data (недоверенный ввод) — резолвим владельца и
	// прокидываем user.ID в репозиторий, чтобы нельзя было править чужую подписку.
	user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}

	switch kind {
	case "any":
		b.clearTrackFSM(ctx, cb.From.ID)
		if err := b.subRepo.SetTrigger(ctx, subID, user.ID, string(domain.TriggerAnyDrop), nil, nil); err != nil {
			b.log.Error("set trigger any", "sub_id", subID, "err", err)
			b.answerCallback(cb.ID, "Ошибка, попробуй позже")
			return
		}
		b.editMenu(chatID, cb.Message.MessageID,
			"🔔 Тип уведомления: "+triggerDescription(domain.TriggerAnyDrop, nil, nil),
			trackTriggerKeyboard(subID, domain.TriggerAnyDrop))
		b.answerCallback(cb.ID, "Готово")

	case "below":
		if err := b.setTrackFSM(ctx, cb.From.ID, trackFSM{SubID: subID, Trigger: string(domain.TriggerBelowTarget)}); err != nil {
			b.reply(chatID, "Не получилось начать ввод (нет связи с хранилищем). Останется «Любое снижение».")
			return
		}
		b.reply(chatID, "💰 Введи целевую цену в рублях (например <code>1499</code>).\nУведомлю, когда цена опустится до неё или ниже.")
		b.answerCallback(cb.ID, "")

	case "disc":
		if err := b.setTrackFSM(ctx, cb.From.ID, trackFSM{SubID: subID, Trigger: string(domain.TriggerDiscountPct)}); err != nil {
			b.reply(chatID, "Не получилось начать ввод (нет связи с хранилищем). Останется «Любое снижение».")
			return
		}
		b.reply(chatID, "％ Введи процент скидки от текущей цены (1–99, например <code>20</code>).")
		b.answerCallback(cb.ID, "")

	default:
		b.answerCallback(cb.ID, "Ошибка")
	}
}

// handleTrackThreshold — приём числа (порог/процент) для товарной подписки.
func (b *Bot) handleTrackThreshold(ctx context.Context, chatID, tgID int64, text string, fsm trackFSM) {
	// fsm.SubID создавался для этого пользователя, но подстраховываемся фильтром по
	// владельцу в репозитории — резолвим user.ID по telegram_id.
	user, err := b.userRepo.GetByTelegramID(ctx, tgID)
	if err != nil {
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	switch domain.TriggerType(fsm.Trigger) {
	case domain.TriggerBelowTarget:
		price, err := parsePrice(text)
		if err != nil {
			b.reply(chatID, "Нужно число — цена в рублях, например <code>1499</code>. Или /menu чтобы отменить.")
			return
		}
		b.clearTrackFSM(ctx, tgID)
		if err := b.subRepo.SetTrigger(ctx, fsm.SubID, user.ID, string(domain.TriggerBelowTarget), &price, nil); err != nil {
			b.log.Error("set trigger below", "sub_id", fsm.SubID, "err", err)
			b.reply(chatID, "Произошла ошибка, попробуй позже.")
			return
		}
		b.confirmTrackTrigger(chatID, fsm.SubID, domain.TriggerBelowTarget, &price, nil)

	case domain.TriggerDiscountPct:
		pct, err := parsePct(text)
		if err != nil {
			b.reply(chatID, "Нужно целое число от 1 до 99, например <code>20</code>. Или /menu чтобы отменить.")
			return
		}
		b.clearTrackFSM(ctx, tgID)
		if err := b.subRepo.SetTrigger(ctx, fsm.SubID, user.ID, string(domain.TriggerDiscountPct), nil, &pct); err != nil {
			b.log.Error("set trigger disc", "sub_id", fsm.SubID, "err", err)
			b.reply(chatID, "Произошла ошибка, попробуй позже.")
			return
		}
		b.confirmTrackTrigger(chatID, fsm.SubID, domain.TriggerDiscountPct, nil, &pct)

	default:
		b.clearTrackFSM(ctx, tgID)
		b.reply(chatID, "Что-то пошло не так, начни заново через /menu.")
	}
}

func (b *Bot) confirmTrackTrigger(chatID, subID int64, t domain.TriggerType, target *float64, pct *int16) {
	m := tgbotapi.NewMessage(chatID, "✅ <b>Готово!</b> "+triggerDescription(t, target, pct))
	m.ParseMode = "HTML"
	kb := trackTriggerKeyboard(subID, t)
	m.ReplyMarkup = kb
	b.send(m)
}
