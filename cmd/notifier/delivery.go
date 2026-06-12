package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
	"gitlab.com/KosovAndrey/tryberrybot/internal/vk"
)

// deliverer — маршрутизация уведомлений по каналам (users.notify_channel):
// Telegram и/или VK. Если VK не сконфигурирован (vk == nil), всё идёт в
// Telegram (а юзерам без TG ничего — с warn-логом, не зацикливая kafka-retry).
// Успех = доставлено хотя бы в один канал (ошибка второго только логируется,
// иначе kafka-retry задублирует сообщение в доставленный канал).
type deliverer struct {
	log   *slog.Logger
	tg    *telegram.Notifier
	vk    *vk.Client
	users *postgres.UserRepo
}

// targets — куда слать. userID — основной ключ (users.id), telegramID — фолбэк
// для старых kafka-событий без user_id. Возвращает (слать в TG, peer VK или 0).
func (d *deliverer) targets(ctx context.Context, userID, telegramID int64) (sendTG bool, vkPeer int64) {
	if d.vk == nil && telegramID != 0 {
		// VK выключен, TG-чат известен — без лишнего запроса к БД (как раньше).
		return true, 0
	}
	var u *domain.User
	var err error
	if userID != 0 {
		u, err = d.users.GetByID(ctx, userID)
	} else {
		u, err = d.users.GetByTelegramID(ctx, telegramID)
	}
	if err != nil {
		// Не нашли/ошибка — ведём себя как раньше (в TG), уведомление важнее роутинга.
		return telegramID != 0, 0
	}
	if d.vk == nil {
		return u.TelegramID != 0, 0
	}
	hasVK := u.VKID != nil
	tg, vkOn := domain.ResolveNotifyTargets(u.NotifyChannel, u.TelegramID != 0, hasVK)
	if vkOn && hasVK {
		vkPeer = *u.VKID
	}
	return tg, vkPeer
}

// deliver — общий хвост: TG и/или VK, успех при любой доставке.
func (d *deliverer) deliver(ctx context.Context, userID, telegramID int64, sendTG func(context.Context) error, vkText string) error {
	tg, vkPeer := d.targets(ctx, userID, telegramID)

	if !tg && vkPeer == 0 {
		// Некуда доставлять (например, VK-only юзер при выключенном VK).
		// Возвращаем nil: retry не поможет, кафку зацикливать нельзя.
		d.log.Warn("deliver: no channel available", "user_id", userID, "telegram_id", telegramID)
		return nil
	}

	var tgErr, vkErr error
	delivered := false

	if tg {
		if tgErr = sendTG(ctx); tgErr == nil {
			delivered = true
		}
	}
	if vkPeer != 0 {
		if vkErr = d.vk.SendMessage(ctx, vkPeer, vkText); vkErr == nil {
			delivered = true
		}
	}

	if delivered {
		if tgErr != nil {
			d.log.Error("deliver: tg failed (vk ok)", "user_id", userID, "err", tgErr)
		}
		if vkErr != nil {
			d.log.Error("deliver: vk failed (tg ok)", "user_id", userID, "err", vkErr)
		}
		return nil
	}
	if tgErr != nil {
		return tgErr
	}
	return vkErr
}

func (d *deliverer) SendPriceAlert(ctx context.Context, a telegram.PriceAlert) error {
	return d.deliver(ctx, a.UserID, a.ChatID,
		func(ctx context.Context) error { return d.tg.SendPriceAlert(ctx, a) },
		vkPriceText(a))
}

func (d *deliverer) SendSearchAlert(ctx context.Context, a telegram.SearchAlert) error {
	return d.deliver(ctx, a.UserID, a.ChatID,
		func(ctx context.Context) error { return d.tg.SendSearchAlert(ctx, a) },
		vkSearchText(a))
}

// ── Сервисные уведомления реконсайлера (по users.id) ──────────────────────────

func (d *deliverer) SendPlanPausedNotice(ctx context.Context, userID int64) error {
	return d.deliver(ctx, userID, 0,
		func(ctx context.Context) error {
			u, err := d.users.GetByID(ctx, userID)
			if err != nil {
				return err
			}
			return d.tg.SendPlanPausedNotice(ctx, u.TelegramID)
		},
		"⏳ Тариф закончился\n\n"+
			"Часть твоих подписок приостановлена (вышли за лимит бесплатного тарифа). "+
			"Я сохраню их настройки ещё 7 дней — оформи подписку за это время, и я верну их "+
			"и продолжу следить за ценами. Потом они удалятся.\n\nТарифы — кнопка «💳 Тарифы» внизу.")
}

func (d *deliverer) SendPlanExpiringReminder(ctx context.Context, userID int64) error {
	return d.deliver(ctx, userID, 0,
		func(ctx context.Context) error {
			u, err := d.users.GetByID(ctx, userID)
			if err != nil {
				return err
			}
			return d.tg.SendPlanExpiringReminder(ctx, u.TelegramID)
		},
		"⏳ Тариф скоро закончится\n\n"+
			"Завтра истекает срок твоего тарифа. Продли, чтобы не потерять подписки и лимиты — "+
			"иначе часть из них будет приостановлена.\n\nТарифы — кнопка «💳 Тарифы» внизу.")
}

func (d *deliverer) SendReferralRewardNotice(ctx context.Context, userID int64, friendName string, days int, granted bool) error {
	who := "Твой друг"
	if friendName != "" {
		who = "Твой друг @" + friendName
	}
	vkText := fmt.Sprintf("🎉 %s освоился в боте!\n\n", who)
	if granted {
		vkText += fmt.Sprintf("За это тебе начислено +%d дн. тарифа — спасибо, что зовёшь друзей!", days)
	} else {
		vkText += "Награда записана — спасибо, что зовёшь друзей!"
	}
	return d.deliver(ctx, userID, 0,
		func(ctx context.Context) error {
			u, err := d.users.GetByID(ctx, userID)
			if err != nil {
				return err
			}
			return d.tg.SendReferralRewardNotice(ctx, u.TelegramID, friendName, days, granted)
		},
		vkText)
}

// ── Plain-text рендер для VK (HTML там не работает) ──────────────────────────

func vkPriceText(a telegram.PriceAlert) string {
	diff := a.OldPrice - a.NewPrice
	percent := math.Round(diff / a.OldPrice * 100)
	return fmt.Sprintf(
		"📉 Цена снизилась!\n\n%s\n\nБыло: %.0f ₽ → Стало: %.0f ₽\nСкидка: %.0f ₽ (%.0f%%)\n\n%s",
		a.ProductName, a.OldPrice, a.NewPrice, diff, percent, a.ProductURL)
}

func vkSearchText(a telegram.SearchAlert) string {
	var sb strings.Builder
	if a.TotalHits > len(a.Items) {
		fmt.Fprintf(&sb, "🔎 По запросу «%s» подешевело %d товаров — показываю лучшие %d:\n\n",
			a.QueryText, a.TotalHits, len(a.Items))
	} else {
		fmt.Fprintf(&sb, "🔎 По запросу «%s» подешевело %d товаров:\n\n", a.QueryText, a.TotalHits)
	}
	for _, it := range a.Items {
		fmt.Fprintf(&sb, "📉 %s\n", it.Name)
		if it.PrevRub > it.EffectiveRub && it.PrevRub > 0 {
			fmt.Fprintf(&sb, "%.0f ₽ (было %.0f ₽)", it.EffectiveRub, it.PrevRub)
		} else {
			fmt.Fprintf(&sb, "%.0f ₽", it.EffectiveRub)
		}
		if it.PointsRub > 0 {
			fmt.Fprintf(&sb, " +%.0f баллов", it.PointsRub)
		}
		fmt.Fprintf(&sb, "\n%s\n\n", it.URL)
	}
	if a.SearchURL != "" {
		fmt.Fprintf(&sb, "Вся выдача: %s", a.SearchURL)
	}
	return sb.String()
}
