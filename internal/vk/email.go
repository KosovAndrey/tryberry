package vk

import (
	"context"
	"encoding/json"
	"fmt"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Сбор email для чека 54-ФЗ перед первой оплатой (как в TG). Сохраняем в
// users.email, дальше не переспрашиваем. Состояние — в Redis (vk_email_fsm).

type emailFSM struct {
	Plan string `json:"plan"`
	Sub  bool   `json:"sub,omitempty"` // true → после email оформляем подписку
}

func emailFSMKey(vkID int64) string { return fmt.Sprintf("vk_email_fsm:%d", vkID) }

func (b *Bot) getEmailFSM(ctx context.Context, vkID int64) (emailFSM, bool) {
	if b.rdb == nil {
		return emailFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, emailFSMKey(vkID)).Result()
	if err != nil {
		return emailFSM{}, false
	}
	var fsm emailFSM
	if json.Unmarshal([]byte(raw), &fsm) != nil {
		return emailFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setEmailFSM(ctx context.Context, vkID int64, fsm emailFSM) error {
	if b.rdb == nil {
		return fmt.Errorf("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, emailFSMKey(vkID), raw, fsmTTL).Err()
}

func (b *Bot) clearEmailFSM(ctx context.Context, vkID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, emailFSMKey(vkID))
}

func emailRequestText(p domain.Plan, amountKopecks int64) string {
	return fmt.Sprintf(
		"💳 Тариф %s — к оплате %s ₽\n\n"+
			"Перед оплатой отправь свой email одним сообщением.\n\n"+
			"Зачем: на каждый платёж нужен чек. "+
			"Платёжный сервис сформирует чек и отправит его тебе на этот адрес. "+
			"Сохраню email, чтобы больше не спрашивать.\n\n"+
			"Отправь email сообщением, или нажми «Тарифы» — отменить.",
		p.Title, domain.KopecksToRubString(amountKopecks))
}

// promptChangeEmail — запрос нового email из профиля (emailFSM с пустым Plan →
// handleEmailInput только сохранит и подтвердит, без оплаты).
func (b *Bot) promptChangeEmail(ctx context.Context, vkID int64, user *domain.User) {
	if err := b.setEmailFSM(ctx, vkID, emailFSM{Plan: ""}); err != nil {
		b.log.Error("vk: prompt change email", "user_id", user.ID, "err", err)
		b.send(ctx, vkID, "Не удалось начать смену email, попробуй позже.", menuKeyboard(user.TelegramID != 0))
		return
	}
	b.send(ctx, vkID,
		"✉️ Email для чека\n\n"+
			"Отправь новый email одним сообщением — на него платёжный сервис отправляет кассовый чек.",
		menuKeyboard(user.TelegramID != 0))
}

// handleEmailInput — пользователь прислал email в режиме ожидания. Если ввод был
// частью оплаты (Plan задан) — продолжаем оплату, иначе просто подтверждаем.
func (b *Bot) handleEmailInput(ctx context.Context, vkID int64, user *domain.User, text string, fsm emailFSM) {
	kb := menuKeyboard(user.TelegramID != 0)
	email, ok := domain.ValidEmail(text)
	if !ok {
		b.send(ctx, vkID, "Хм, это не похоже на email. Отправь адрес вида name@example.com "+
			"(или «Тарифы» — отменить).", kb)
		return
	}
	if err := b.userRepo.SetEmail(ctx, user.ID, email); err != nil {
		b.log.Error("vk: save email", "user_id", user.ID, "err", err)
		b.send(ctx, vkID, "Не удалось сохранить email, попробуй ещё раз.", kb)
		return
	}
	b.clearEmailFSM(ctx, vkID)

	if fsm.Plan == "" {
		b.send(ctx, vkID, fmt.Sprintf("✅ Email обновлён: %s", email), kb)
		return
	}
	if fsm.Sub {
		b.startSubCheckout(ctx, vkID, user, fsm.Plan, email)
		return
	}
	b.startCheckout(ctx, vkID, user, fsm.Plan, email)
}
