package vk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

// Трекинг товаров из VK: та же логика, что в telegram.doTrack (скрейп → upsert
// товара → лимит тарифа → upsert подписки), но plain-text. Тип уведомления тоже
// выбирается (vkTriggerKeyboard: любое снижение / ниже цены / скидка %).

const (
	listMaxShown   = 20 // VK режет сообщения ~4096 символов
	listMaxButtons = 10 // лимит inline-клавиатуры VK
)

func (b *Bot) handleTrack(ctx context.Context, vkID int64, user *domain.User, rawURL string) {
	s, err := b.registry.FindByURL(rawURL)
	if err != nil {
		b.send(ctx, vkID, "Не могу распознать ссылку. Отправь ссылку на товар Wildberries.", nil)
		return
	}

	b.send(ctx, vkID, "⏳ Получаю данные о товаре...", nil)

	// Через registry.Scrape (а не s.Scrape напрямую), чтобы инкрементить
	// tryberrybot_scrape_requests_total — иначе ручные /track-скрейпы из VK
	// не попадают в success-rate/алерт (см. telegram.doTrack). Маркетплейс
	// уже знаем из s.
	result, _, err := b.registry.Scrape(ctx, rawURL)
	if err != nil {
		b.log.Error("vk: scrape on track", "url", rawURL, "err", err)
		msg := "❌ Не удалось получить данные о товаре. Попробуй позже."
		switch {
		case errors.Is(err, scraper.ErrAgeRestricted):
			msg = "🔞 Это товар 18+. Ozon прячет его цену за подтверждением возраста — пока не могу отслеживать такие товары."
		case s.Marketplace() == scraper.MarketplaceOzon &&
			(errors.Is(err, scraper.ErrNotImplemented) || errors.Is(err, scraper.ErrMarketplaceBlocked)):
			// Заглушка: Ozon не сконфигурён/заблокирован — понятное «скоро будет».
			msg = "🔵 Ozon скоро будет — отслеживание этого маркетплейса ещё в разработке.\n\n" +
				"Пока отслеживаю Wildberries 🟣 — пришли ссылку на товар оттуда."
		}
		b.send(ctx, vkID, msg, nil)
		return
	}

	product, err := b.prodRepo.Upsert(ctx, rawURL, result.Name, result.ImageURL, string(s.Marketplace()))
	if err != nil {
		b.log.Error("vk: upsert product", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	// Лимит тарифа: повторная ссылка на уже отслеживаемый товар лимит не расходует.
	plan := user.EffectivePlan(time.Now())
	active, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("vk: count active subs", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	alreadyTracked := false
	for _, sub := range active {
		if sub.ProductID == product.ID {
			alreadyTracked = true
			break
		}
	}
	if !alreadyTracked && len(active) >= plan.MaxProduct {
		b.send(ctx, vkID, fmt.Sprintf(
			"🚫 Достигнут лимит тарифа %s: товаров %d из %d.\n\n"+
				"Отпишись от ненужного («Мои товары») или оформи тариф повыше — "+
				"тарифы пока в Telegram-боте @TryBerryBot, команда /plans.",
			plan.Title, len(active), plan.MaxProduct), menuKeyboard(user.TelegramID != 0))
		return
	}

	// OOS: товара нет в наличии (нет активного оффера) — «слежу за снижением»
	// неприменимо. Заводим back_in_stock и фиксируем in_stock=false (как TG doTrack).
	if !result.InStock {
		if err := b.prodRepo.SetInStock(ctx, product.ID, false); err != nil {
			b.log.Warn("vk: set product out of stock", "product_id", product.ID, "err", err)
		}
		oos, _, err := b.subRepo.UpsertOutOfStock(ctx, user.ID, product.ID, result.Price)
		if err != nil {
			b.log.Error("vk: upsert oos subscription", "err", err)
			b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
			return
		}
		priceLine := "Цена появится, когда товар вернётся в продажу."
		if result.Price > 0 {
			priceLine = fmt.Sprintf("💰 Последняя цена: %.0f ₽", result.Price)
		}
		b.send(ctx, vkID, fmt.Sprintf(
			"✅ Добавил в отслеживание!\n\n%s\n🚫 Сейчас товара нет в наличии (нет активного предложения).\n%s\n\n"+
				"По умолчанию уведомлю, как только он появится в наличии. Сменить тип — кнопками ниже 👇",
			result.Name, priceLine),
			vkTrackOOSKeyboard(oos.ID, result.Price > 0, b.chartURL(product.PublicID)))
		return
	}

	sub, created, err := b.subRepo.Upsert(ctx, user.ID, product.ID, result.Price)
	if err != nil {
		b.log.Error("vk: upsert subscription", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	head := "✅ Добавил в отслеживание!"
	if !created {
		head = "🔄 Отслеживание возобновлено!"
	}
	b.send(ctx, vkID, fmt.Sprintf(
		"%s\n\n%s\n💰 Текущая цена: %.0f ₽\n\n"+
			"🔔 Сейчас уведомлю при любом снижении. Сменить тип уведомления — кнопками ниже 👇",
		head, result.Name, result.Price), vkTriggerKeyboard(sub.ID, domain.TriggerAnyDrop, b.chartURL(product.PublicID)))
}

// ── Тип триггера товарной подписки ────────────────────────────────────────────

// vkTrackFSM — ждём число (цену/процент) для уже созданной подписки.
type vkTrackFSM struct {
	SubID   int64  `json:"s"`
	Trigger string `json:"t"`
}

func trackFSMKey(vkID int64) string { return fmt.Sprintf("vk_track_fsm:%d", vkID) }

func (b *Bot) getTrackFSM(ctx context.Context, vkID int64) (vkTrackFSM, bool) {
	if b.rdb == nil {
		return vkTrackFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, trackFSMKey(vkID)).Result()
	if err != nil {
		return vkTrackFSM{}, false
	}
	var fsm vkTrackFSM
	if err := json.Unmarshal([]byte(raw), &fsm); err != nil {
		return vkTrackFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setTrackFSM(ctx context.Context, vkID int64, fsm vkTrackFSM) error {
	if b.rdb == nil {
		return errors.New("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, trackFSMKey(vkID), raw, fsmTTL).Err()
}

func (b *Bot) clearTrackFSM(ctx context.Context, vkID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, trackFSMKey(vkID))
}

// vkTriggerKeyboard — inline-выбор стратегии под сообщением товара,
// текущая помечена галочкой (как в TG). chartURL непустой → добавляем link-кнопку
// «📈 График цены» (open_link) отдельной строкой; "" → без неё.
func vkTriggerKeyboard(subID int64, current domain.TriggerType, chartURL string) *Keyboard {
	mark := func(label string, t domain.TriggerType) string {
		if current == t {
			return "✅ " + label
		}
		return label
	}
	pl := func(kind string) string {
		return fmt.Sprintf(`{"cmd":%q,"id":%d,"k":%q}`, cmdPTrack, subID, kind)
	}
	rows := [][]Button{
		{TextButton(mark("🔻 Любое снижение", domain.TriggerAnyDrop), pl("any"), ColorPrimary)},
		{
			TextButton(mark("📉 Ниже цены", domain.TriggerBelowTarget), pl("below"), ColorSecondary),
			TextButton(mark("％ Скидка %", domain.TriggerDiscountPct), pl("disc"), ColorSecondary),
		},
	}
	if chartURL != "" {
		rows = append(rows, []Button{LinkButton("📈 График цены", chartURL)})
	}
	return &Keyboard{Inline: true, Buttons: rows}
}

// vkTrackOOSKeyboard — клавиатура для товара БЕЗ активного оффера: по умолчанию
// «когда появится в наличии» (back_in_stock), а «ниже цены»/«скидка %» — только при
// известной last-цене (hasPrice). Зеркало telegram.trackOOSKeyboard.
func vkTrackOOSKeyboard(subID int64, hasPrice bool, chartURL string) *Keyboard {
	pl := func(kind string) string {
		return fmt.Sprintf(`{"cmd":%q,"id":%d,"k":%q}`, cmdPTrack, subID, kind)
	}
	rows := [][]Button{
		{TextButton("✅ 🔔 Когда появится в наличии", pl("stock"), ColorPrimary)},
	}
	if hasPrice {
		rows = append(rows, []Button{
			TextButton("📉 Ниже цены", pl("below"), ColorSecondary),
			TextButton("％ Скидка %", pl("disc"), ColorSecondary),
		})
	} else {
		rows = append(rows, []Button{TextButton("📉 Ниже цены", pl("below"), ColorSecondary)})
	}
	if chartURL != "" {
		rows = append(rows, []Button{LinkButton("📈 График цены", chartURL)})
	}
	return &Keyboard{Inline: true, Buttons: rows}
}

// handleProductTrigger — нажатие кнопки типа триггера (cmd=ptrack).
func (b *Bot) handleProductTrigger(ctx context.Context, vkID int64, user *domain.User, p payloadData) {
	switch p.Kind {
	case "any":
		// id — из payload (недоверенный ввод); SetTrigger фильтрует по владельцу.
		if err := b.subRepo.SetTrigger(ctx, p.ID, user.ID, string(domain.TriggerAnyDrop), nil, nil); err != nil {
			b.log.Error("vk: set trigger any", "sub_id", p.ID, "err", err)
			b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
			return
		}
		b.send(ctx, vkID, "🔔 Тип уведомления: "+domain.TriggerDescription(domain.TriggerAnyDrop, nil, nil),
			vkTriggerKeyboard(p.ID, domain.TriggerAnyDrop, b.chartURLForSub(ctx, p.ID)))
	case "stock":
		// Товар без оффера: ждать появления в наличии (back_in_stock).
		if err := b.subRepo.SetTrigger(ctx, p.ID, user.ID, string(domain.TriggerBackInStock), nil, nil); err != nil {
			b.log.Error("vk: set trigger stock", "sub_id", p.ID, "err", err)
			b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
			return
		}
		hasPrice := false
		if s, err := b.subRepo.GetByID(ctx, p.ID); err == nil {
			hasPrice = s.FirstSeenPrice > 0
		}
		b.send(ctx, vkID, "🔔 Тип уведомления: "+domain.TriggerDescription(domain.TriggerBackInStock, nil, nil),
			vkTrackOOSKeyboard(p.ID, hasPrice, b.chartURLForSub(ctx, p.ID)))
	case "below":
		if err := b.setTrackFSM(ctx, vkID, vkTrackFSM{SubID: p.ID, Trigger: string(domain.TriggerBelowTarget)}); err != nil {
			b.send(ctx, vkID, "Не получилось начать ввод (нет связи с хранилищем). Останется «Любое снижение».", nil)
			return
		}
		b.send(ctx, vkID, "💰 Введи целевую цену в рублях (например 1499).\nУведомлю, когда цена опустится до неё или ниже.", nil)
	case "disc":
		if err := b.setTrackFSM(ctx, vkID, vkTrackFSM{SubID: p.ID, Trigger: string(domain.TriggerDiscountPct)}); err != nil {
			b.send(ctx, vkID, "Не получилось начать ввод (нет связи с хранилищем). Останется «Любое снижение».", nil)
			return
		}
		b.send(ctx, vkID, "％ Введи процент скидки от текущей цены (1–99, например 20).", nil)
	}
}

// handleTrackThreshold — приём числа (порог/процент) для товарной подписки.
func (b *Bot) handleTrackThreshold(ctx context.Context, vkID int64, user *domain.User, text string, fsm vkTrackFSM) {
	switch domain.TriggerType(fsm.Trigger) {
	case domain.TriggerBelowTarget:
		price, err := domain.ParsePrice(text)
		if err != nil {
			b.send(ctx, vkID, "Нужно число — цена в рублях, например 1499. Любая кнопка внизу отменит ввод.", nil)
			return
		}
		b.clearTrackFSM(ctx, vkID)
		if err := b.subRepo.SetTrigger(ctx, fsm.SubID, user.ID, string(domain.TriggerBelowTarget), &price, nil); err != nil {
			b.log.Error("vk: set trigger below", "sub_id", fsm.SubID, "err", err)
			b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
			return
		}
		b.send(ctx, vkID, "✅ Готово! "+domain.TriggerDescription(domain.TriggerBelowTarget, &price, nil),
			vkTriggerKeyboard(fsm.SubID, domain.TriggerBelowTarget, b.chartURLForSub(ctx, fsm.SubID)))

	case domain.TriggerDiscountPct:
		pct, err := domain.ParsePct(text)
		if err != nil {
			b.send(ctx, vkID, "Нужно целое число от 1 до 99, например 20. Любая кнопка внизу отменит ввод.", nil)
			return
		}
		b.clearTrackFSM(ctx, vkID)
		if err := b.subRepo.SetTrigger(ctx, fsm.SubID, user.ID, string(domain.TriggerDiscountPct), nil, &pct); err != nil {
			b.log.Error("vk: set trigger disc", "sub_id", fsm.SubID, "err", err)
			b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
			return
		}
		b.send(ctx, vkID, "✅ Готово! "+domain.TriggerDescription(domain.TriggerDiscountPct, nil, &pct),
			vkTriggerKeyboard(fsm.SubID, domain.TriggerDiscountPct, b.chartURLForSub(ctx, fsm.SubID)))

	default:
		b.clearTrackFSM(ctx, vkID)
		b.send(ctx, vkID, "Что-то пошло не так, отправь ссылку на товар заново.", menuKeyboard(user.TelegramID != 0))
	}
}

// handleList — список подписок + inline-кнопки отписки. prefix — строка над
// списком (например, подтверждение отписки).
func (b *Bot) handleList(ctx context.Context, vkID int64, user *domain.User, prefix string) {
	subs, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("vk: get subscriptions", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if len(subs) == 0 {
		text := "📋 У тебя пока нет активных подписок.\n\nОтправь ссылку на товар Wildberries — начну отслеживать цену."
		if prefix != "" {
			text = prefix + "\n\n" + text
		}
		b.send(ctx, vkID, text, menuKeyboard(user.TelegramID != 0))
		return
	}

	var sb strings.Builder
	if prefix != "" {
		sb.WriteString(prefix + "\n\n")
	}
	fmt.Fprintf(&sb, "📋 Твои подписки — %d активных\n\n", len(subs))
	for i, sub := range subs {
		if i == listMaxShown {
			fmt.Fprintf(&sb, "… и ещё %d. Полный список — в Telegram-боте (/list).\n", len(subs)-listMaxShown)
			break
		}
		current := "нет данных"
		if sub.CurrentPrice > 0 {
			emoji := ""
			if sub.CurrentPrice < sub.FirstSeenPrice {
				emoji = "📉 "
			}
			current = fmt.Sprintf("%s%.0f ₽", emoji, sub.CurrentPrice)
		}
		fmt.Fprintf(&sb, "%d. %s\n   сейчас %s | при подписке %.0f ₽\n   %s\n",
			i+1, sub.ProductName, current, sub.FirstSeenPrice, sub.ProductURL)
		if cu := b.chartURL(sub.ProductPublicID); cu != "" {
			fmt.Fprintf(&sb, "   📈 График: %s\n", cu)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Отписаться — кнопки «❌ номер» под сообщением 👇")

	// Inline-клавиатура отписки; постоянное меню при этом остаётся на месте.
	b.send(ctx, vkID, sb.String(), untrackKeyboard(subs))
}

// untrackKeyboard — inline-кнопки «❌ N» (VK: максимум 10 кнопок в inline).
func untrackKeyboard(subs []*domain.Subscription) *Keyboard {
	var rows [][]Button
	var row []Button
	for i, sub := range subs {
		if i == listMaxButtons {
			break
		}
		row = append(row, TextButton(
			fmt.Sprintf("❌ %d", i+1),
			fmt.Sprintf(`{"cmd":%q,"id":%d}`, cmdUntrack, sub.ID),
			ColorSecondary,
		))
		if len(row) == 5 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return &Keyboard{Inline: true, Buttons: rows}
}

func (b *Bot) handleUntrack(ctx context.Context, vkID int64, user *domain.User, subID int64) {
	if subID == 0 {
		b.send(ctx, vkID, b.welcomeText(user), menuKeyboard(user.TelegramID != 0))
		return
	}
	// id — из payload кнопки; гасим только подписку этого юзера.
	if err := b.subRepo.Deactivate(ctx, subID, user.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		b.log.Error("vk: deactivate", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	b.handleList(ctx, vkID, user, "✅ Отслеживание отменено.")
}
