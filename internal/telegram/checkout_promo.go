package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Ввод промокода прямо в платёжном флоу — кнопка «🎟 Промокод» на карточке тарифа,
// а не отдельная команда /promo «в меню». Скидочный код запоминается как ожидающая
// скидка и применяется к ближайшей оплате; grant-код активируется сразу (как /promo).
// Состояние диалога — в Redis, по образцу email/порога.

type checkoutPromoFSM struct {
	Plan string `json:"plan"` // тариф, с карточки которого зашли — чтобы вернуть на него
}

func checkoutPromoFSMKey(tgID int64) string { return fmt.Sprintf("checkout_promo_fsm:%d", tgID) }

func (b *Bot) getCheckoutPromoFSM(ctx context.Context, tgID int64) (checkoutPromoFSM, bool) {
	if b.rdb == nil {
		return checkoutPromoFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, checkoutPromoFSMKey(tgID)).Result()
	if err != nil {
		return checkoutPromoFSM{}, false
	}
	var fsm checkoutPromoFSM
	if json.Unmarshal([]byte(raw), &fsm) != nil {
		return checkoutPromoFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setCheckoutPromoFSM(ctx context.Context, tgID int64, fsm checkoutPromoFSM) error {
	if b.rdb == nil {
		return fmt.Errorf("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, checkoutPromoFSMKey(tgID), raw, fsmTTL).Err()
}

func (b *Bot) clearCheckoutPromoFSM(ctx context.Context, tgID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, checkoutPromoFSMKey(tgID))
}

// promptCheckoutPromo — кнопка «промокод» на карточке тарифа: просим прислать код.
func (b *Bot) promptCheckoutPromo(ctx context.Context, tgID, chatID int64, messageID int, plan string) {
	if b.rdb == nil {
		// Без Redis FSM не работает — отправляем на обычную команду.
		b.showView(chatID, messageID,
			"🎟 Отправь промокод командой <code>/promo КОД</code> — скидка применится к оплате.",
			backToPlanKeyboard(plan))
		return
	}
	if err := b.setCheckoutPromoFSM(ctx, tgID, checkoutPromoFSM{Plan: plan}); err != nil {
		b.log.Error("checkout promo: set fsm", "tg_id", tgID, "err", err)
		b.showView(chatID, messageID, "Не удалось открыть ввод промокода, попробуй позже.", backToPlanKeyboard(plan))
		return
	}
	b.showView(chatID, messageID,
		"🎟 <b>Промокод</b>\n\nПришли код одним сообщением — применю скидку к оплате тарифа.\n\n"+
			"<i>Или ◀️ Назад — продолжить без кода.</i>",
		backToPlanKeyboard(plan))
}

// handleCheckoutPromoInput — пользователь прислал промокод в платёжном флоу.
// Скидочный код → ожидающая скидка + возврат на карточку (уже с учётом скидки).
// Grant-код → активируем сразу, как /promo. Невалидный → просим другой, FSM держим.
func (b *Bot) handleCheckoutPromoInput(ctx context.Context, chatID, tgID int64, text string, user *domain.User, fsm checkoutPromoFSM) {
	code := domain.NormalizePromoCode(text)
	if code == "" {
		b.reply(chatID, "Пришли промокод одним сообщением (или /plans — отменить).")
		return
	}

	promo, err := b.promoRepo.GetActiveByCode(ctx, code)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Код не держим строгим — даём ввести другой, состояние не сбрасываем.
			b.reply(chatID, "🎟 Такого промокода нет, либо он уже не действует.\nПришли другой код (или /plans — отменить).")
			return
		}
		b.log.Error("checkout promo: get code", "err", err)
		b.clearCheckoutPromoFSM(ctx, tgID)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	b.clearCheckoutPromoFSM(ctx, tgID)

	switch promo.Kind {
	case domain.PromoKindGrant:
		// Grant даёт дни тарифа сразу (оплата не нужна) — ровно как /promo.
		b.applyGrantPromo(ctx, chatID, user, promo)
	case domain.PromoKindDiscount:
		if b.discounts == nil {
			b.reply(chatID, "🎟 Скидки временно недоступны, попробуй позже.")
			return
		}
		if err := b.discounts.Put(ctx, user.ID, promo.ID, promo.DiscountPct); err != nil {
			b.log.Error("checkout promo: store pending discount", "user_id", user.ID, "err", err)
			b.reply(chatID, "Не удалось применить промокод, попробуй позже.")
			return
		}
		b.reply(chatID, fmt.Sprintf("✅ Промокод <b>%s</b> применён: скидка <b>%d%%</b> на оплату.",
			htmlEscape(promo.Code), promo.DiscountPct))
		// Возвращаем на карточку тарифа новым сообщением — цена уже со скидкой.
		b.sendPlanCard(ctx, tgID, chatID, 0, fsm.Plan)
	default:
		b.log.Error("checkout promo: unknown kind", "kind", promo.Kind, "code", promo.Code)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
	}
}

// backToPlanKeyboard — «Назад» на карточку конкретного тарифа.
func backToPlanKeyboard(plan string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Назад", "plan:view:"+plan),
		),
	)
}
