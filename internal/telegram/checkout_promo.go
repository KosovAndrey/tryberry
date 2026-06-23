package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Ввод промокода в платёжном флоу — единый FSM для трёх точек входа: кнопка «🎟
// Промокод» в меню, кнопка на карточке тарифа и команда /promo без аргумента.
// Скидочный код запоминается как ожидающая скидка и юзер уезжает на оплату уже со
// скидкой; grant-код выдаёт дни сразу. Состояние диалога — в Redis, как email/порог.

type checkoutPromoFSM struct {
	// Plan — куда вернуть после применения скидки: "" → список тарифов (вход из
	// меню/команды), имя плана → его карточка (вход с карточки).
	Plan string `json:"plan"`
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

// promptCheckoutPromo — попросить прислать промокод. plan: "" → вход из меню/команды
// (после скидки вернём в список тарифов), имя плана → вход с карточки.
func (b *Bot) promptCheckoutPromo(ctx context.Context, tgID, chatID int64, messageID int, plan string) {
	if b.rdb == nil {
		b.showView(chatID, messageID,
			"🎟 Отправь промокод командой <code>/promo КОД</code> — скидка применится к оплате.",
			b.promoBackKeyboard(plan))
		return
	}
	if err := b.setCheckoutPromoFSM(ctx, tgID, checkoutPromoFSM{Plan: plan}); err != nil {
		b.log.Error("checkout promo: set fsm", "tg_id", tgID, "err", err)
		b.showView(chatID, messageID, "Не удалось открыть ввод промокода, попробуй позже.", b.promoBackKeyboard(plan))
		return
	}
	b.showView(chatID, messageID,
		"🎟 <b>Промокод</b>\n\nПришли код одним сообщением.\n\n"+
			"• код на скидку — применю к оплате тарифа;\n"+
			"• подарочный код — выдам дни тарифа сразу.\n\n"+
			"<i>Или ◀️ Назад — без кода.</i>",
		b.promoBackKeyboard(plan))
}

// handleCheckoutPromoInput — юзер прислал промокод в режиме ожидания (FSM). Код
// распознан (grant выдан / скидка применена / серверная ошибка) → сбрасываем FSM;
// не распознан (нет такого / уже использован) → держим FSM, чтобы ввёл другой.
func (b *Bot) handleCheckoutPromoInput(ctx context.Context, chatID, tgID int64, text string, user *domain.User, fsm checkoutPromoFSM) {
	if b.applyPromoCode(ctx, chatID, tgID, user, text, fsm.Plan) {
		b.clearCheckoutPromoFSM(ctx, tgID)
	}
}

// applyPromoCode — единая обработка введённого кода (из меню, карточки или /promo).
// returnPlan: "" → после скидки показать список тарифов, иначе карточку плана.
// Возвращает recognized: true, если код разобран (grant/скидка/серверная ошибка)
// и FSM можно сбросить; false — дать ввести другой код.
func (b *Bot) applyPromoCode(ctx context.Context, chatID, tgID int64, user *domain.User, codeArg, returnPlan string) (recognized bool) {
	code := domain.NormalizePromoCode(codeArg)
	if code == "" {
		b.reply(chatID, "Пришли промокод одним сообщением (или /plans — отменить).")
		return false
	}

	promo, err := b.promoRepo.GetActiveByCode(ctx, code)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.reply(chatID, "🎟 Такого промокода нет, либо он уже не действует.\nПришли другой код (или /plans — отменить).")
			return false
		}
		b.log.Error("promo: get code", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return true
	}

	switch promo.Kind {
	case domain.PromoKindGrant:
		// Подарочный код выдаёт дни тарифа сразу (как /promo). RedeemGrant внутри
		// держит «один раз на юзера», так что отдельная проверка не нужна.
		b.applyGrantPromo(ctx, chatID, user, promo)
		return true

	case domain.PromoKindDiscount:
		if b.discounts == nil {
			b.reply(chatID, "🎟 Скидки временно недоступны, попробуй позже.")
			return true
		}
		// Пред-проверка «доступен этому юзеру» (свободные активации + он его ещё не
		// гасил) — чтобы дать обратную связь сразу, а не молчаливо потерять скидку на
		// оплате. Сбой проверки не блокирует (best-effort): финальный гейт — на checkout.
		if ok, err := b.promoRepo.Redeemable(ctx, promo.ID, user.ID); err == nil && !ok {
			b.reply(chatID, "🎟 Этот код уже использован тобой или исчерпан.\nПришли другой код (или /plans — отменить).")
			return false
		}
		if err := b.discounts.Put(ctx, user.ID, promo.ID, promo.DiscountPct); err != nil {
			b.log.Error("promo: store pending discount", "user_id", user.ID, "err", err)
			b.reply(chatID, "Не удалось применить промокод, попробуй позже.")
			return true
		}
		b.reply(chatID, fmt.Sprintf("✅ Промокод <b>%s</b> применён: скидка <b>%d%%</b> на оплату.\nВыбери тариф — цена уже со скидкой.",
			htmlEscape(promo.Code), promo.DiscountPct))
		// Уводим на оплату: на карточку плана (если зашли с неё) или в список тарифов.
		if returnPlan != "" {
			b.sendPlanCard(ctx, tgID, chatID, 0, returnPlan)
		} else {
			b.sendPlansMenu(ctx, tgID, chatID, 0)
		}
		return true

	default:
		b.log.Error("promo: unknown kind", "kind", promo.Kind, "code", promo.Code)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return true
	}
}

// promoBackKeyboard — «Назад» туда, откуда зашли вводить код: список тарифов
// (plan=="") или карточка конкретного тарифа.
func (b *Bot) promoBackKeyboard(plan string) tgbotapi.InlineKeyboardMarkup {
	if plan == "" {
		return backToPlansKeyboard()
	}
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Назад", "plan:view:"+plan),
		),
	)
}
