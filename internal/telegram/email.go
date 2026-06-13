package telegram

import (
	"context"
	"encoding/json"
	"fmt"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Сбор email для чека 54-ФЗ. Перед первой оплатой просим адрес (один раз —
// потом сохранён в users.email). Состояние диалога — в Redis, как у порога.

type emailFSM struct {
	Plan string `json:"plan"`
}

func emailFSMKey(tgID int64) string { return fmt.Sprintf("email_fsm:%d", tgID) }

func (b *Bot) getEmailFSM(ctx context.Context, tgID int64) (emailFSM, bool) {
	if b.rdb == nil {
		return emailFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, emailFSMKey(tgID)).Result()
	if err != nil {
		return emailFSM{}, false
	}
	var fsm emailFSM
	if json.Unmarshal([]byte(raw), &fsm) != nil {
		return emailFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setEmailFSM(ctx context.Context, tgID int64, fsm emailFSM) error {
	if b.rdb == nil {
		return fmt.Errorf("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, emailFSMKey(tgID), raw, fsmTTL).Err()
}

func (b *Bot) clearEmailFSM(ctx context.Context, tgID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, emailFSMKey(tgID))
}

// emailRequestText — пояснение, зачем нужен email (чтобы не выглядело прихотью).
func emailRequestText(p domain.Plan, amountKopecks int64) string {
	return fmt.Sprintf(
		"💳 <b>Тариф %s</b> — к оплате %s ₽\n\n"+
			"Перед оплатой пришли свой <b>email</b> одним сообщением.\n\n"+
			"Зачем: по закону <b>54-ФЗ</b> на каждый платёж нужен кассовый чек. "+
			"ЮKassa сформирует чек и отправит его тебе на этот адрес. "+
			"Сохраню email, чтобы больше не спрашивать.\n\n"+
			"<i>Отправить email сообщением, или /plans — отменить.</i>",
		p.Title, domain.KopecksToRubString(amountKopecks))
}

// promptChangeEmail — запрос нового email из профиля (без привязки к оплате:
// emailFSM с пустым Plan → handleEmailInput только сохранит и подтвердит).
func (b *Bot) promptChangeEmail(ctx context.Context, tgID, chatID int64, messageID int) {
	if err := b.setEmailFSM(ctx, tgID, emailFSM{Plan: ""}); err != nil {
		b.log.Error("prompt change email: set fsm", "tg_id", tgID, "err", err)
		b.showView(chatID, messageID, "Не удалось начать смену email, попробуй позже.", backToMenuKeyboard())
		return
	}
	b.showView(chatID, messageID,
		"✉️ <b>Email для чека</b>\n\n"+
			"Пришли новый email одним сообщением — на него ЮKassa отправляет кассовый чек (54-ФЗ).\n\n"+
			"<i>Или /menu — отменить.</i>",
		backToMenuKeyboard())
}

// handleEmailInput — пользователь прислал email в режиме ожидания. Валидируем,
// сохраняем и, если ввод был частью оплаты (Plan задан) — продолжаем оплату,
// иначе (смена из профиля) — просто подтверждаем.
func (b *Bot) handleEmailInput(ctx context.Context, chatID, tgID int64, text string, user *domain.User, fsm emailFSM) {
	email, ok := domain.ValidEmail(text)
	if !ok {
		b.reply(chatID, "Хм, это не похоже на email. Пришли адрес вида <code>name@example.com</code> "+
			"(или /menu — отменить).")
		return
	}
	if err := b.userRepo.SetEmail(ctx, user.ID, email); err != nil {
		b.log.Error("email input: save email", "user_id", user.ID, "err", err)
		b.reply(chatID, "Не удалось сохранить email, попробуй ещё раз.")
		return
	}
	b.clearEmailFSM(ctx, tgID)

	if fsm.Plan == "" {
		b.reply(chatID, fmt.Sprintf("✅ Email обновлён: <b>%s</b>", htmlEscape(email)))
		return
	}
	b.startCheckout(ctx, user, chatID, 0, fsm.Plan, email)
}
