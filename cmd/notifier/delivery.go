package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
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

	// synth — редирект доставки синтетических юзеров нагрузочного теста на
	// реальные тест-аккаунты (nil = выключен, синтетики дропаются). См.
	// docs/LOAD-TEST-SYNTHETIC.md.
	synth *synthRedirect
}

// synthRedirect — конфиг доставки синтетических юзеров: их фейковые идентичности
// заменяются на реальные тест-аккаунты. Чтобы 1000 синтетиков не залили 3-5 чатов
// (в жизни алерты размазаны по 1000 чатов, у TG лимит ~1 msg/s на чат), форвардится
// только каждый sampleN-й юзер (по users.id — детерминированно, один и тот же
// синтетик всегда попадает в один и тот же тест-чат). Остальные — synthetic drop
// с метрикой, путь «решение→pending_alerts→флашер» они всё равно прогружают.
type synthRedirect struct {
	tg, vk, max []int64
	sampleN     int64 // 1 из N юзеров форвардится; <=1 — форвардить всех
}

// parseSynthRedirect — конфиг из env: SYNTH_REDIRECT_TG_IDS / _VK_IDS / _MAX_IDS
// (CSV chat_id тест-аккаунтов) + SYNTH_REDIRECT_SAMPLE_N (дефолт 10). nil, если
// ни один список не задан (штатный прод-режим).
func parseSynthRedirect() *synthRedirect {
	parse := func(key string) []int64 {
		var out []int64
		for _, p := range strings.Split(os.Getenv(key), ",") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			if id, err := strconv.ParseInt(p, 10, 64); err == nil && id != 0 {
				out = append(out, id)
			}
		}
		return out
	}
	s := &synthRedirect{
		tg:      parse("SYNTH_REDIRECT_TG_IDS"),
		vk:      parse("SYNTH_REDIRECT_VK_IDS"),
		max:     parse("SYNTH_REDIRECT_MAX_IDS"),
		sampleN: 10,
	}
	if len(s.tg) == 0 && len(s.vk) == 0 && len(s.max) == 0 {
		return nil
	}
	if v, err := strconv.ParseInt(os.Getenv("SYNTH_REDIRECT_SAMPLE_N"), 10, 64); err == nil && v > 0 {
		s.sampleN = v
	}
	return s
}

// route — куда слать алерт синтетика. Канальность синтетика сохраняем: фейковый
// TG-id → тест-TG-чат, фейковый VK → тест-VK и т.д. dropped=true — sampled out.
func (s *synthRedirect) route(u *domain.User) (tgChat, vkPeer, maxUser int64, dropped bool) {
	if s.sampleN > 1 && u.ID%s.sampleN != 0 {
		return 0, 0, 0, true
	}
	if u.TelegramID != 0 && len(s.tg) > 0 {
		tgChat = s.tg[int(u.ID)%len(s.tg)]
	}
	if u.VKID != nil && len(s.vk) > 0 {
		vkPeer = s.vk[int(u.ID)%len(s.vk)]
	}
	if u.MaxID != nil && len(s.max) > 0 {
		maxUser = s.max[int(u.ID)%len(s.max)]
	}
	return tgChat, vkPeer, maxUser, false
}

// targets — куда слать. userID — основной ключ (users.id), telegramID — фолбэк
// для старых kafka-событий без user_id. Возвращает конкретные адресаты каналов
// (0 = не слать): TG-чат, peer VK, user MAX. sampledOut=true — синтетик, срезанный
// сэмплированием (дропнуть тихо, без warn «no channel»).
func (d *deliverer) targets(ctx context.Context, userID, telegramID int64) (tgChat, vkPeer, maxUser int64, sampledOut bool) {
	if d.synth == nil && d.vk == nil && d.mx == nil && telegramID != 0 {
		// Доп. каналы и synth-режим выключены, TG-чат известен — без запроса к БД.
		return telegramID, 0, 0, false
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
		return telegramID, 0, 0, false
	}
	if u.IsSynthetic {
		if d.synth == nil {
			// Нагрузочный тест без редиректа: на фейковые id не шлём вообще.
			return 0, 0, 0, true
		}
		return d.synth.route(u)
	}
	if d.vk == nil && d.mx == nil {
		return u.TelegramID, 0, 0, false
	}
	hasVK := u.VKID != nil
	hasMax := u.MaxID != nil
	tg, vkOn, mxOn := domain.ResolveNotifyTargets(u.NotifyChannel, u.TelegramID != 0, hasVK, hasMax)
	if tg {
		tgChat = u.TelegramID
	}
	if vkOn && hasVK && d.vk != nil {
		vkPeer = *u.VKID
	}
	if mxOn && hasMax && d.mx != nil {
		maxUser = *u.MaxID
	}
	return tgChat, vkPeer, maxUser, false
}

// deliver — общий хвост: TG и/или VK и/или MAX, успех при любой доставке.
// sendTG получает конкретный chat_id (может быть переопределён synth-редиректом).
// imageURL — фото для VK (пусто → без картинки; MAX рендерит превью ссылки).
func (d *deliverer) deliver(ctx context.Context, userID, telegramID int64, sendTG func(context.Context, int64) error, vkText, vkImageURL string) error {
	tgChat, vkPeer, maxUser, sampledOut := d.targets(ctx, userID, telegramID)

	if sampledOut {
		// Синтетик нагрузочного теста, срезанный сэмплированием/без редиректа:
		// путь до доставки прогружен, сам send намеренно не выполняем.
		metrics.NotificationsDelivered.WithLabelValues("synth", "skipped").Inc()
		return nil
	}
	if tgChat == 0 && vkPeer == 0 && maxUser == 0 {
		// Некуда доставлять. Возвращаем nil: retry не поможет, кафку зацикливать нельзя.
		d.log.Warn("deliver: no channel available", "user_id", userID, "telegram_id", telegramID)
		metrics.NotificationsDelivered.WithLabelValues("none", "skipped").Inc()
		return nil
	}

	var tgErr, vkErr, mxErr error
	delivered := false

	if tgChat != 0 {
		if tgErr = sendTG(ctx, tgChat); tgErr == nil {
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
		func(ctx context.Context, chat int64) error {
			a := a
			a.ChatID = chat
			return d.tg.SendPriceAlert(ctx, a)
		},
		vkPriceText(a, d.chartBaseURL), a.ImageURL)
}

func (d *deliverer) SendSearchAlert(ctx context.Context, a telegram.SearchAlert) error {
	return d.deliver(ctx, a.UserID, a.ChatID,
		func(ctx context.Context, chat int64) error {
			a := a
			a.ChatID = chat
			return d.tg.SendSearchAlert(ctx, a)
		},
		vkSearchText(a), "")
}

// SendBundledAlert — пачка товарных алертов одного юзера одним сообщением (без
// фото: несколько картинок в один текст не вложить). Роутинг TG/VK как обычно.
func (d *deliverer) SendBundledAlert(ctx context.Context, a telegram.BundledAlert) error {
	return d.deliver(ctx, a.UserID, a.ChatID,
		func(ctx context.Context, chat int64) error {
			a := a
			a.ChatID = chat
			return d.tg.SendBundledAlert(ctx, a)
		},
		vkBundledText(a), "")
}

// SendDigest — персональный дайджест (один и тот же текст в TG и VK), роутинг по
// notify_channel. Текст уже отрендерен билдером (HTML годится и для VK — теги VK
// игнорирует/не критично).
func (d *deliverer) SendDigest(ctx context.Context, userID, telegramID int64, text string) error {
	return d.deliver(ctx, userID, telegramID,
		func(ctx context.Context, chat int64) error { return d.tg.SendDigest(ctx, chat, text) },
		text, "")
}

// ── Сервисные уведомления реконсайлера (по users.id) ──────────────────────────

func (d *deliverer) SendPlanPausedNotice(ctx context.Context, userID int64) error {
	return d.deliver(ctx, userID, 0,
		func(ctx context.Context, chat int64) error {
			return d.tg.SendPlanPausedNotice(ctx, chat)
		},
		"⏳ Тариф закончился\n\n"+
			"Часть твоих подписок приостановлена (вышли за лимит бесплатного тарифа). "+
			"Я сохраню их настройки ещё 7 дней — оформи подписку за это время, и я верну их "+
			"и продолжу следить за ценами. Потом они удалятся.\n\nТарифы — кнопка «💳 Тарифы» внизу.", "")
}

func (d *deliverer) SendPlanExpiringReminder(ctx context.Context, userID int64) error {
	return d.deliver(ctx, userID, 0,
		func(ctx context.Context, chat int64) error {
			return d.tg.SendPlanExpiringReminder(ctx, chat)
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
		func(ctx context.Context, chat int64) error {
			return d.tg.SendReferralRewardNotice(ctx, chat, friendName, days, granted)
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
