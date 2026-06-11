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
// Telegram (как раньше) и/или VK. Если VK не сконфигурирован (vk == nil),
// поведение идентично прежнему — всё в Telegram, без лишних запросов к БД.
// Успех = доставлено хотя бы в один канал (ошибка второго только логируется,
// иначе kafka-retry задублирует сообщение в доставленный канал).
type deliverer struct {
	log   *slog.Logger
	tg    *telegram.Notifier
	vk    *vk.Client
	users *postgres.UserRepo
}

// targets — куда слать для юзера с данным telegram_id.
func (d *deliverer) targets(ctx context.Context, telegramID int64) (sendTG bool, vkPeer int64) {
	if d.vk == nil {
		return true, 0
	}
	u, err := d.users.GetByTelegramID(ctx, telegramID)
	if err != nil {
		// Не нашли/ошибка — ведём себя как раньше (в TG), уведомление важнее роутинга.
		return true, 0
	}
	hasVK := u.VKID != nil
	tg, vkOn := domain.ResolveNotifyTargets(u.NotifyChannel, u.TelegramID != 0, hasVK)
	if vkOn && hasVK {
		vkPeer = *u.VKID
	}
	return tg, vkPeer
}

// deliver — общий хвост: TG и/или VK, успех при любой доставке.
func (d *deliverer) deliver(ctx context.Context, telegramID int64, sendTG func(context.Context) error, vkText string) error {
	tg, vkPeer := d.targets(ctx, telegramID)

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
			d.log.Error("deliver: tg failed (vk ok)", "telegram_id", telegramID, "err", tgErr)
		}
		if vkErr != nil {
			d.log.Error("deliver: vk failed (tg ok)", "telegram_id", telegramID, "err", vkErr)
		}
		return nil
	}
	if tgErr != nil {
		return tgErr
	}
	return vkErr
}

func (d *deliverer) SendPriceAlert(ctx context.Context, a telegram.PriceAlert) error {
	return d.deliver(ctx, a.ChatID,
		func(ctx context.Context) error { return d.tg.SendPriceAlert(ctx, a) },
		vkPriceText(a))
}

func (d *deliverer) SendSearchAlert(ctx context.Context, a telegram.SearchAlert) error {
	return d.deliver(ctx, a.ChatID,
		func(ctx context.Context) error { return d.tg.SendSearchAlert(ctx, a) },
		vkSearchText(a))
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
