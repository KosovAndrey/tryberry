package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/max"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
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
	log          *slog.Logger
	tg           *telegram.Notifier
	vk           *vk.Client
	mx           *max.Client
	users        *postgres.UserRepo
	chartBaseURL string // PUBLIC_BASE_URL для ссылки «📈 График цены» в VK/MAX-пуше; "" — без неё
}

// targets — куда слать. userID — основной ключ (users.id), telegramID — фолбэк
// для старых kafka-событий без user_id. Возвращает (слать в TG, peer VK или 0,
// user MAX или 0).
func (d *deliverer) targets(ctx context.Context, userID, telegramID int64) (sendTG bool, vkPeer, maxUser int64) {
	if d.vk == nil && d.mx == nil && telegramID != 0 {
		// Доп. каналы выключены, TG-чат известен — без лишнего запроса к БД.
		return true, 0, 0
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
		return telegramID != 0, 0, 0
	}
	if d.vk == nil && d.mx == nil {
		return u.TelegramID != 0, 0, 0
	}
	hasVK := u.VKID != nil
	hasMax := u.MaxID != nil
	tg, vkOn, mxOn := domain.ResolveNotifyTargets(u.NotifyChannel, u.TelegramID != 0, hasVK, hasMax)
	if vkOn && hasVK && d.vk != nil {
		vkPeer = *u.VKID
	}
	if mxOn && hasMax && d.mx != nil {
		maxUser = *u.MaxID
	}
	return tg, vkPeer, maxUser
}

// deliver — общий хвост: TG и/или VK и/или MAX, успех при любой доставке.
// imageURL — фото для VK (пусто → без картинки; MAX рендерит превью ссылки).
func (d *deliverer) deliver(ctx context.Context, userID, telegramID int64, sendTG func(context.Context) error, vkText, vkImageURL string) error {
	tg, vkPeer, maxUser := d.targets(ctx, userID, telegramID)

	if !tg && vkPeer == 0 && maxUser == 0 {
		// Некуда доставлять. Возвращаем nil: retry не поможет, кафку зацикливать нельзя.
		d.log.Warn("deliver: no channel available", "user_id", userID, "telegram_id", telegramID)
		metrics.NotificationsDelivered.WithLabelValues("none", "skipped").Inc()
		return nil
	}

	var tgErr, vkErr, mxErr error
	delivered := false

	if tg {
		if tgErr = sendTG(ctx); tgErr == nil {
			delivered = true
		}
		metrics.NotificationsDelivered.WithLabelValues("tg", statusLabel(tgErr)).Inc()
	}
	if vkPeer != 0 {
		if vkImageURL != "" {
			vkErr = d.vk.SendMessagePhoto(ctx, vkPeer, vkText, vkImageURL)
		} else {
			vkErr = d.vk.SendMessage(ctx, vkPeer, vkText)
		}
		if vkErr == nil {
			delivered = true
		}
		metrics.NotificationsDelivered.WithLabelValues("vk", statusLabel(vkErr)).Inc()
	}
	if maxUser != 0 {
		mxErr = d.mx.SendMessage(ctx, maxUser, vkText)
		if mxErr == nil {
			delivered = true
		}
		metrics.NotificationsDelivered.WithLabelValues("max", statusLabel(mxErr)).Inc()
	}

	if delivered {
		if tgErr != nil {
			d.log.Error("deliver: tg failed (other ok)", "user_id", userID, "err", tgErr)
		}
		if vkErr != nil {
			d.log.Error("deliver: vk failed (other ok)", "user_id", userID, "err", vkErr)
		}
		if mxErr != nil {
			d.log.Error("deliver: max failed (other ok)", "user_id", userID, "err", mxErr)
		}
		return nil
	}
	// Не доставлено. Перманентную ошибку TG (битая картинка, бан, чат не найден)
	// НЕ ретраим — иначе одно сообщение зацикливает kafka-консьюмер и копит лаг.
	// Транзиентные (429/сеть, любые VK) — отдаём наверх для ретрая.
	if errors.Is(tgErr, telegram.ErrTelegramPermanent) && vkErr == nil && mxErr == nil {
		d.log.Error("deliver: tg permanent, skipping (no kafka retry)", "user_id", userID, "err", tgErr)
		metrics.NotificationsDelivered.WithLabelValues("tg", "skipped").Inc()
		return nil
	}
	if tgErr != nil {
		return tgErr
	}
	if vkErr != nil {
		return vkErr
	}
	return mxErr
}

func statusLabel(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func (d *deliverer) SendPriceAlert(ctx context.Context, a telegram.PriceAlert) error {
	return d.deliver(ctx, a.UserID, a.ChatID,
		func(ctx context.Context) error { return d.tg.SendPriceAlert(ctx, a) },
		vkPriceText(a, d.chartBaseURL), a.ImageURL)
}

func (d *deliverer) SendSearchAlert(ctx context.Context, a telegram.SearchAlert) error {
	return d.deliver(ctx, a.UserID, a.ChatID,
		func(ctx context.Context) error { return d.tg.SendSearchAlert(ctx, a) },
		vkSearchText(a), "")
}

// SendBundledAlert — пачка товарных алертов одного юзера одним сообщением (без
// фото: несколько картинок в один текст не вложить). Роутинг TG/VK как обычно.
func (d *deliverer) SendBundledAlert(ctx context.Context, a telegram.BundledAlert) error {
	return d.deliver(ctx, a.UserID, a.ChatID,
		func(ctx context.Context) error { return d.tg.SendBundledAlert(ctx, a) },
		vkBundledText(a), "")
}

// SendDigest — персональный дайджест (один и тот же текст в TG и VK), роутинг по
// notify_channel. Текст уже отрендерен билдером (HTML годится и для VK — теги VK
// игнорирует/не критично).
func (d *deliverer) SendDigest(ctx context.Context, userID, telegramID int64, text string) error {
	return d.deliver(ctx, userID, telegramID,
		func(ctx context.Context) error { return d.tg.SendDigest(ctx, telegramID, text) },
		text, "")
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
			"и продолжу следить за ценами. Потом они удалятся.\n\nТарифы — кнопка «💳 Тарифы» внизу.", "")
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
			"иначе часть из них будет приостановлена.\n\nТарифы — кнопка «💳 Тарифы» внизу.", "")
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
		vkText, "")
}

// ── Plain-text рендер для VK (HTML там не работает) ──────────────────────────

func vkPriceText(a telegram.PriceAlert, chartBaseURL string) string {
	// chart — строка «📈 График цены: <url>» (или ""), как CTA после ссылки на товар.
	chart := ""
	if chartBaseURL != "" && a.PublicID != "" {
		chart = "\n📈 График цены: " + chartBaseURL + "/p/" + a.PublicID
	}
	if a.BackInStock {
		return fmt.Sprintf("🔔 Снова в наличии!\n\n%s\n\nЦена: %.0f ₽\nТеперь слежу за снижением цены (поменять — /list)\n\n%s%s",
			a.ProductName, a.NewPrice, a.ProductURL, chart)
	}
	diff := a.OldPrice - a.NewPrice
	percent := math.Round(diff / a.OldPrice * 100)
	honest := ""
	if a.HonestLine != "" {
		honest = "\n" + a.HonestLine
	}
	return fmt.Sprintf(
		"📉 Цена снизилась!\n\n%s\n\nБыло: %.0f ₽ → Стало: %.0f ₽\nСкидка: %.0f ₽ (%.0f%%)%s\n\n%s%s",
		a.ProductName, a.OldPrice, a.NewPrice, diff, percent, honest, a.ProductURL, chart)
}

func vkBundledText(a telegram.BundledAlert) string {
	hasBack := false
	for _, it := range a.Items {
		if it.BackInStock {
			hasBack = true
			break
		}
	}
	var sb strings.Builder
	if hasBack {
		fmt.Fprintf(&sb, "🔔 Обновления по вашим товарам (%d):\n\n", len(a.Items))
	} else {
		fmt.Fprintf(&sb, "📉 По вашим товарам снизилась цена (%d):\n\n", len(a.Items))
	}
	for _, it := range a.Items {
		if it.BackInStock {
			fmt.Fprintf(&sb, "🔔 %s\nснова в наличии — %.0f ₽\n%s\n\n", it.ProductName, it.NewPrice, it.ProductURL)
			continue
		}
		fmt.Fprintf(&sb, "📉 %s\n", it.ProductName)
		if it.OldPrice > it.NewPrice && it.OldPrice > 0 {
			pct := math.Round((it.OldPrice - it.NewPrice) / it.OldPrice * 100)
			fmt.Fprintf(&sb, "%.0f ₽ (было %.0f ₽, -%.0f%%)\n%s\n\n", it.NewPrice, it.OldPrice, pct, it.ProductURL)
		} else {
			fmt.Fprintf(&sb, "%.0f ₽\n%s\n\n", it.NewPrice, it.ProductURL)
		}
	}
	sb.WriteString("Управлять отслеживанием — /list")
	return sb.String()
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
