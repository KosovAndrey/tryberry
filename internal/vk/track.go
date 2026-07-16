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

const listPageSize = 8 // подписок на страницу списка (VK лимитирует inline-кнопки)

// pageBounds — число страниц и срез [start,end) для page по n элементам.
func pageBounds(n, page int) (pages, start, end int) {
	pages = (n + listPageSize - 1) / listPageSize
	if pages < 1 {
		pages = 1
	}
	page = clampPage(page, pages)
	start = page * listPageSize
	end = start + listPageSize
	if end > n {
		end = n
	}
	return
}

func clampPage(page, pages int) int {
	if page < 0 {
		return 0
	}
	if page >= pages {
		return pages - 1
	}
	return page
}

// pageNavRow — строка навигации ◀️/▶️ для списка (cmd несёт целевую страницу в id).
// nil, если страница одна.
func pageNavRow(cmd string, page, pages int) []Button {
	if pages <= 1 {
		return nil
	}
	var nav []Button
	if page > 0 {
		nav = append(nav, TextButton("◀️ Назад", fmt.Sprintf(`{"cmd":%q,"id":%d}`, cmd, page-1), ColorSecondary))
	}
	if page < pages-1 {
		nav = append(nav, TextButton("Вперёд ▶️", fmt.Sprintf(`{"cmd":%q,"id":%d}`, cmd, page+1), ColorSecondary))
	}
	return nav
}

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

	product, err := b.prodRepo.Upsert(ctx, scraper.CanonicalProductURL(s.Marketplace(), rawURL), result.Name, result.ImageURL, string(s.Marketplace()))
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
				"Отпишись от ненужного («Мои товары») или оформи тариф повыше кнопкой «Тарифы» 👇",
			plan.Title, len(active), plan.MaxProduct), menuKeyboard(user.TelegramID != 0))
		return
	}

	// OOS: товара нет в наличии (нет активного оффера) — «слежу за снижением»
	// неприменимо. Заводим back_in_stock и фиксируем in_stock=false (как TG doTrack).
	if !result.InStock {
		if err := b.prodRepo.SetInStock(ctx, product.ID, false); err != nil {
			b.log.Warn("vk: set product out of stock", "product_id", product.ID, "err", err)
		}
		// Последняя известная цена — из НАШЕЙ истории (внутри UpsertOutOfStock),
		// а не из result.Price: там «справочная» цена из стейта МП, и она врёт.
		oos, _, err := b.subRepo.UpsertOutOfStock(ctx, user.ID, product.ID)
		if err != nil {
			b.log.Error("vk: upsert oos subscription", "err", err)
			b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
			return
		}
		priceLine := "Цена появится, когда товар вернётся в продажу."
		if oos.BaselinePrice > 0 {
			priceLine = fmt.Sprintf("💰 Последняя цена: %.0f ₽", oos.BaselinePrice)
		}
		b.send(ctx, vkID, fmt.Sprintf(
			"✅ Добавил в отслеживание!\n\n%s\n🚫 Сейчас товара нет в наличии (нет активного предложения).\n%s\n\n"+
				"По умолчанию уведомлю, как только он появится в наличии. Сменить тип — кнопками ниже 👇",
			result.Name, priceLine),
			vkTrackOOSKeyboard(oos.ID, oos.BaselinePrice > 0, b.chartURL(product.PublicID)))
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
		// Подсказки целевой цены убраны — сразу ручной ввод.
		b.promptManualTarget(ctx, vkID, p.ID)
	case "disc":
		if err := b.setTrackFSM(ctx, vkID, vkTrackFSM{SubID: p.ID, Trigger: string(domain.TriggerDiscountPct)}); err != nil {
			b.send(ctx, vkID, "Не получилось начать ввод (нет связи с хранилищем). Останется «Любое снижение».", nil)
			return
		}
		b.send(ctx, vkID, "％ Введи процент скидки от текущей цены (1–99, например 20).", nil)
	}
}

// maybeInstantBelowTargetAlert — мгновенный алерт, если порог below_target уже
// выполнен в момент установки (текущая известная цена ≤ target). Без этого
// пользователь получал бы «Готово» и ждал уведомления до часа (следующий цикл
// скрейпа), хотя условие срабатывания было известно сразу.
// Возвращает true, если алерт отправлен — тогда вызывающий НЕ должен слать
// обычное «Готово!» (алерт уже содержит подтверждение установки порога).
func (b *Bot) maybeInstantBelowTargetAlert(ctx context.Context, vkID, subID int64, target float64) bool {
	sub, err := b.subRepo.GetByID(ctx, subID)
	if err != nil {
		return false
	}
	current := sub.FirstSeenPrice
	if b.priceRepo != nil {
		if p, _, err := b.priceRepo.GetLatest(ctx, sub.ProductID); err == nil && p > 0 {
			current = p
		}
	}
	if current <= 0 || current > target {
		return false
	}

	product, err := b.prodRepo.GetByID(ctx, sub.ProductID)
	if err != nil {
		b.log.Error("vk: get product for instant alert", "sub_id", subID, "err", err)
		return false
	}
	// Товар не в наличии: last-цена есть, но «уже стоит X ₽» ввёл бы в
	// заблуждение. Ждём штатного цикла — notifier при OOS ценовые триггеры
	// не оценивает, алерт придёт после возврата в продажу.
	if !product.InStock {
		return false
	}

	// UpdateBaseline фиксирует baseline_price=current и notified=TRUE — это
	// «гасит» дубль от notifier (Decide шлёт повторные только при цене НИЖЕ
	// baseline), а следующее уведомление придёт при дальнейшем падении цены.
	if err := b.subRepo.UpdateBaseline(ctx, subID, current); err != nil {
		b.log.Warn("vk: update baseline on instant alert", "sub_id", subID, "err", err)
	}

	text := fmt.Sprintf(
		"✅ Порог %.0f ₽ установлен.\n\n"+
			"🎯 %s уже стоит %.0f ₽ — ниже твоего порога!\n"+
			"Следующее уведомление пришлю, когда цена опустится ещё ниже.",
		target, product.Name, current,
	)
	b.send(ctx, vkID, text, vkTriggerKeyboard(subID, domain.TriggerBelowTarget, b.chartURL(product.PublicID)))
	return true
}

// promptManualTarget — фолбэк-ввод целевой цены вручную (FSM).
func (b *Bot) promptManualTarget(ctx context.Context, vkID, subID int64) {
	if err := b.setTrackFSM(ctx, vkID, vkTrackFSM{SubID: subID, Trigger: string(domain.TriggerBelowTarget)}); err != nil {
		b.send(ctx, vkID, "Не получилось начать ввод (нет связи с хранилищем). Останется «Любое снижение».", nil)
		return
	}
	b.send(ctx, vkID, "💰 Введи целевую цену в рублях (например 1499).\nУведомлю, когда цена опустится до неё или ниже.", nil)
}

// handleProductTarget — выбор подсказанной цены: k=manual → ручной ввод, k=<rub> →
// ставим below_target сразу. Владельца резолвим по user (фильтр в репозитории).
// Кнопки с подсказками больше не создаются, обработчик оставлен для callback'ов
// cmdPTarget из старых сообщений, которые уже разосланы пользователям.
func (b *Bot) handleProductTarget(ctx context.Context, vkID int64, user *domain.User, p payloadData) {
	if p.Kind == "manual" {
		b.promptManualTarget(ctx, vkID, p.ID)
		return
	}
	price, err := domain.ParsePrice(p.Kind)
	if err != nil || price <= 0 {
		b.promptManualTarget(ctx, vkID, p.ID)
		return
	}
	b.clearTrackFSM(ctx, vkID)
	if err := b.subRepo.SetTrigger(ctx, p.ID, user.ID, string(domain.TriggerBelowTarget), &price, nil); err != nil {
		b.log.Error("vk: set trigger below (suggested)", "sub_id", p.ID, "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if b.maybeInstantBelowTargetAlert(ctx, vkID, p.ID, price) {
		return
	}
	b.send(ctx, vkID, "✅ Готово! "+domain.TriggerDescription(domain.TriggerBelowTarget, &price, nil),
		vkTriggerKeyboard(p.ID, domain.TriggerBelowTarget, b.chartURLForSub(ctx, p.ID)))
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
		if b.maybeInstantBelowTargetAlert(ctx, vkID, fsm.SubID, price) {
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

// handleList — список подписок (страница 0). prefix — строка над списком.
func (b *Bot) handleList(ctx context.Context, vkID int64, user *domain.User, prefix string) {
	b.showProductList(ctx, vkID, user, prefix, 0)
}

// showProductList — постраничный список товарных подписок. VK ограничивает inline-
// клавиатуру, поэтому листаем по listPageSize с кнопками ◀️/▶️ — так VK-only юзер
// может управлять ВСЕМИ подписками (паритет с TG, где список не обрезается).
func (b *Bot) showProductList(ctx context.Context, vkID int64, user *domain.User, prefix string, page int) {
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
	pages, start, end := pageBounds(len(subs), page)
	page = clampPage(page, pages)

	var sb strings.Builder
	if prefix != "" {
		sb.WriteString(prefix + "\n\n")
	}
	fmt.Fprintf(&sb, "📋 Твои подписки — %d активных", len(subs))
	if pages > 1 {
		fmt.Fprintf(&sb, " (стр. %d/%d)", page+1, pages)
	}
	sb.WriteString("\n\n")
	for i := start; i < end; i++ {
		sub := subs[i]
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

	var rows [][]Button
	var row []Button
	for i := start; i < end; i++ {
		row = append(row, TextButton(
			fmt.Sprintf("❌ %d", i+1),
			fmt.Sprintf(`{"cmd":%q,"id":%d}`, cmdUntrack, subs[i].ID),
			ColorSecondary))
		if len(row) == 4 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	if nav := pageNavRow(cmdListPage, page, pages); nav != nil {
		rows = append(rows, nav)
	}
	b.send(ctx, vkID, sb.String(), &Keyboard{Inline: true, Buttons: rows})
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
