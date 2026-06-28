package max

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Ввод промокода в платёжном флоу (паритет с telegram/vk): кнопка «🎟 Промокод»,
// кнопка на карточке тарифа и «промокод <КОД>» сводятся в один FSM и общий
// applyPromoCode. Состояние — в Redis (max_promo_fsm).

type promoFSM struct {
	Plan string `json:"plan"`
}

func promoFSMKey(maxID int64) string { return fmt.Sprintf("max_promo_fsm:%d", maxID) }

func (b *Bot) getPromoFSM(ctx context.Context, maxID int64) (promoFSM, bool) {
	if b.rdb == nil {
		return promoFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, promoFSMKey(maxID)).Result()
	if err != nil {
		return promoFSM{}, false
	}
	var fsm promoFSM
	if json.Unmarshal([]byte(raw), &fsm) != nil {
		return promoFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setPromoFSM(ctx context.Context, maxID int64, fsm promoFSM) error {
	if b.rdb == nil {
		return fmt.Errorf("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, promoFSMKey(maxID), raw, fsmTTL).Err()
}

func (b *Bot) clearPromoFSM(ctx context.Context, maxID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, promoFSMKey(maxID))
}

func (b *Bot) promptPromo(ctx context.Context, maxID int64, user *domain.User, plan string) {
	kb := menuKeyboard(user)
	if b.rdb == nil {
		b.send(ctx, maxID, "🎟 Отправь промокод сообщением:\nпромокод КОД", kb)
		return
	}
	if err := b.setPromoFSM(ctx, maxID, promoFSM{Plan: plan}); err != nil {
		b.log.Error("max: set promo fsm", "max_id", maxID, "err", err)
		b.send(ctx, maxID, "Не удалось открыть ввод промокода, попробуй позже.", kb)
		return
	}
	b.send(ctx, maxID,
		"🎟 Промокод\n\nПришли код одним сообщением.\n"+
			"• код на скидку — применю к оплате тарифа;\n"+
			"• подарочный код — выдам дни тарифа сразу.\n\n"+
			"Или «Тарифы» — без кода.", kb)
}

func (b *Bot) handlePromoInput(ctx context.Context, maxID int64, user *domain.User, text string, fsm promoFSM) {
	if b.applyPromoCode(ctx, maxID, user, text, fsm.Plan) {
		b.clearPromoFSM(ctx, maxID)
	}
}

func (b *Bot) applyPromoCode(ctx context.Context, maxID int64, user *domain.User, codeArg, returnPlan string) (recognized bool) {
	kb := menuKeyboard(user)

	code := domain.NormalizePromoCode(codeArg)
	if code == "" {
		b.send(ctx, maxID, "Пришли промокод одним сообщением (или «Тарифы» — отменить).", kb)
		return false
	}

	promo, err := b.promoRepo.GetActiveByCode(ctx, code)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.send(ctx, maxID, "🎟 Такого промокода нет, либо он уже не действует.\nПришли другой код (или «Тарифы» — отменить).", kb)
			return false
		}
		b.log.Error("max: promo get code", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return true
	}

	switch promo.Kind {
	case domain.PromoKindGrant:
		b.applyGrantPromo(ctx, maxID, user, promo, kb)
		return true

	case domain.PromoKindDiscount:
		if b.discounts == nil {
			b.send(ctx, maxID, "🎟 Скидки временно недоступны, попробуй позже.", kb)
			return true
		}
		if ok, err := b.promoRepo.Redeemable(ctx, promo.ID, user.ID); err == nil && !ok {
			b.send(ctx, maxID, "🎟 Этот код уже использован тобой или исчерпан.\nПришли другой код (или «Тарифы» — отменить).", kb)
			return false
		}
		if err := b.discounts.Put(ctx, user.ID, promo.ID, promo.DiscountPct); err != nil {
			b.log.Error("max: store pending discount", "user_id", user.ID, "err", err)
			b.send(ctx, maxID, "Не удалось применить промокод, попробуй позже.", kb)
			return true
		}
		b.send(ctx, maxID, fmt.Sprintf(
			"✅ Промокод %s применён: скидка %d%% на оплату.\nВыбери тариф — цена уже со скидкой.",
			promo.Code, promo.DiscountPct), nil)
		if returnPlan != "" {
			b.sendPlanCard(ctx, maxID, user, returnPlan)
		} else {
			b.sendPlans(ctx, maxID, user)
		}
		return true

	default:
		b.log.Error("max: promo unknown kind", "kind", promo.Kind, "code", promo.Code)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return true
	}
}

func (b *Bot) usablePendingDiscount(ctx context.Context, userID int64) int {
	if b.discounts == nil {
		return 0
	}
	d, ok, err := b.discounts.Get(ctx, userID)
	if err != nil || !ok || d.Pct <= 0 {
		return 0
	}
	if b.promoRepo != nil {
		if usable, err := b.promoRepo.Redeemable(ctx, d.CodeID, userID); err == nil && !usable {
			return 0
		}
	}
	return d.Pct
}

func discountedRub(fullRub, pct int) string {
	return domain.KopecksToRubString(domain.DiscountedKopecks(int64(fullRub)*100, pct))
}
