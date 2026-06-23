package vk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Ввод промокода в платёжном флоу (паритет с telegram/checkout_promo.go): кнопка
// «🎟 Промокод» в меню, кнопка на карточке тарифа и «промокод <КОД>» сводятся в
// один FSM и общий applyPromoCode. Скидочный код → ожидающая скидка + переход к
// оплате уже со скидкой; подарочный — выдаёт дни сразу. Состояние — в Redis.

type promoFSM struct {
	// Plan — куда вернуть после применения скидки: "" → список тарифов (вход из
	// меню / «промокод»), имя плана → его карточка (вход с карточки).
	Plan string `json:"plan"`
}

func promoFSMKey(vkID int64) string { return fmt.Sprintf("vk_promo_fsm:%d", vkID) }

func (b *Bot) getPromoFSM(ctx context.Context, vkID int64) (promoFSM, bool) {
	if b.rdb == nil {
		return promoFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, promoFSMKey(vkID)).Result()
	if err != nil {
		return promoFSM{}, false
	}
	var fsm promoFSM
	if json.Unmarshal([]byte(raw), &fsm) != nil {
		return promoFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setPromoFSM(ctx context.Context, vkID int64, fsm promoFSM) error {
	if b.rdb == nil {
		return fmt.Errorf("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, promoFSMKey(vkID), raw, fsmTTL).Err()
}

func (b *Bot) clearPromoFSM(ctx context.Context, vkID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, promoFSMKey(vkID))
}

// promptPromo — попросить прислать промокод. plan: "" → вход из меню/«промокод»
// (после скидки вернём в список тарифов), имя плана → вход с карточки.
func (b *Bot) promptPromo(ctx context.Context, vkID int64, user *domain.User, plan string) {
	kb := menuKeyboard(user.TelegramID != 0)
	if b.rdb == nil {
		b.send(ctx, vkID, "🎟 Отправь промокод сообщением:\nпромокод КОД", kb)
		return
	}
	if err := b.setPromoFSM(ctx, vkID, promoFSM{Plan: plan}); err != nil {
		b.log.Error("vk: set promo fsm", "vk_id", vkID, "err", err)
		b.send(ctx, vkID, "Не удалось открыть ввод промокода, попробуй позже.", kb)
		return
	}
	b.send(ctx, vkID,
		"🎟 Промокод\n\nПришли код одним сообщением.\n"+
			"• код на скидку — применю к оплате тарифа;\n"+
			"• подарочный код — выдам дни тарифа сразу.\n\n"+
			"Или «Тарифы» — без кода.", kb)
}

// handlePromoInput — юзер прислал промокод в режиме ожидания (FSM). Распознан →
// сбрасываем FSM; не распознан (нет такого / уже использован) → держим, чтобы
// ввёл другой.
func (b *Bot) handlePromoInput(ctx context.Context, vkID int64, user *domain.User, text string, fsm promoFSM) {
	if b.applyPromoCode(ctx, vkID, user, text, fsm.Plan) {
		b.clearPromoFSM(ctx, vkID)
	}
}

// applyPromoCode — единая обработка введённого кода (меню / карточка / «промокод
// КОД»). returnPlan: "" → после скидки показать список тарифов, иначе карточку.
// Возвращает recognized: true, если код разобран и FSM можно сбросить.
func (b *Bot) applyPromoCode(ctx context.Context, vkID int64, user *domain.User, codeArg, returnPlan string) (recognized bool) {
	kb := menuKeyboard(user.TelegramID != 0)

	code := domain.NormalizePromoCode(codeArg)
	if code == "" {
		b.send(ctx, vkID, "Пришли промокод одним сообщением (или «Тарифы» — отменить).", kb)
		return false
	}

	promo, err := b.promoRepo.GetActiveByCode(ctx, code)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.send(ctx, vkID, "🎟 Такого промокода нет, либо он уже не действует.\nПришли другой код (или «Тарифы» — отменить).", kb)
			return false
		}
		b.log.Error("vk: promo get code", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return true
	}

	switch promo.Kind {
	case domain.PromoKindGrant:
		b.applyGrantPromo(ctx, vkID, user, promo, kb)
		return true

	case domain.PromoKindDiscount:
		if b.discounts == nil {
			b.send(ctx, vkID, "🎟 Скидки временно недоступны, попробуй позже.", kb)
			return true
		}
		// Пред-проверка «доступен этому юзеру» (свободные активации + он его ещё не
		// гасил) — тот же гейт, что на оплате. Сбой проверки не блокирует (best-effort).
		if ok, err := b.promoRepo.Redeemable(ctx, promo.ID, user.ID); err == nil && !ok {
			b.send(ctx, vkID, "🎟 Этот код уже использован тобой или исчерпан.\nПришли другой код (или «Тарифы» — отменить).", kb)
			return false
		}
		if err := b.discounts.Put(ctx, user.ID, promo.ID, promo.DiscountPct); err != nil {
			b.log.Error("vk: store pending discount", "user_id", user.ID, "err", err)
			b.send(ctx, vkID, "Не удалось применить промокод, попробуй позже.", kb)
			return true
		}
		b.send(ctx, vkID, fmt.Sprintf(
			"✅ Промокод %s применён: скидка %d%% на оплату.\nВыбери тариф — цена уже со скидкой.",
			promo.Code, promo.DiscountPct), nil)
		if returnPlan != "" {
			b.sendPlanCard(ctx, vkID, user, returnPlan)
		} else {
			b.sendPlans(ctx, vkID, user)
		}
		return true

	default:
		b.log.Error("vk: promo unknown kind", "kind", promo.Kind, "code", promo.Code)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return true
	}
}

// usablePendingDiscount — ожидающая скидка юзера, ПРИГОДНАЯ к оплате прямо сейчас
// (тот же гейт Redeemable, что на checkout: код жив, не исчерпан, юзер не гасил).
// 0, если скидки нет или она уже не сработает — чтобы превью не обещало скидку,
// которой не будет на оплате. Best-effort: при ошибке БД показываем.
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

// discountedRub — цена в рублях со скидкой pct%, готовая строка ("399.20").
func discountedRub(fullRub, pct int) string {
	return domain.KopecksToRubString(domain.DiscountedKopecks(int64(fullRub)*100, pct))
}
