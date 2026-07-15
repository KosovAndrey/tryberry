package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

// trackFSM — состояние диалога настройки стратегии для ТОВАРНОЙ подписки:
// ждём число (целевую цену или процент) для уже созданной подписки SubID.
type trackFSM struct {
	SubID   int64  `json:"s"`
	Trigger string `json:"t"` // domain.TriggerBelowTarget | domain.TriggerDiscountPct
}

func trackFSMKey(tgID int64) string { return fmt.Sprintf("track_fsm:%d", tgID) }

func (b *Bot) getTrackFSM(ctx context.Context, tgID int64) (trackFSM, bool) {
	if b.rdb == nil {
		return trackFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, trackFSMKey(tgID)).Result()
	if err != nil {
		return trackFSM{}, false
	}
	var fsm trackFSM
	if err := json.Unmarshal([]byte(raw), &fsm); err != nil {
		return trackFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setTrackFSM(ctx context.Context, tgID int64, fsm trackFSM) error {
	if b.rdb == nil {
		return errors.New("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, trackFSMKey(tgID), raw, fsmTTL).Err()
}

func (b *Bot) clearTrackFSM(ctx context.Context, tgID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, trackFSMKey(tgID))
}

// trackTriggerKeyboard — клавиатура выбора стратегии под сообщением товара.
// Текущая стратегия помечается галочкой. Снизу — переходы в меню.
func trackTriggerKeyboard(subID int64, current domain.TriggerType, chartURL string) tgbotapi.InlineKeyboardMarkup {
	mark := func(label string, t domain.TriggerType) string {
		if current == t {
			return "✅ " + label
		}
		return label
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				mark("🔻 Любое снижение", domain.TriggerAnyDrop),
				fmt.Sprintf("ptrack:%d:any", subID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				mark("📉 Ниже цены", domain.TriggerBelowTarget),
				fmt.Sprintf("ptrack:%d:below", subID)),
			tgbotapi.NewInlineKeyboardButtonData(
				mark("％ Скидка %", domain.TriggerDiscountPct),
				fmt.Sprintf("ptrack:%d:disc", subID)),
		),
	}
	if row := chartButtonRow(chartURL); row != nil {
		rows = append(rows, row)
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("📋 Мои подписки", "menu:list"),
		tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
	))
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// chartButtonRow — строка с кнопкой «📈 График цены» (URL на /p/<public_id>), либо
// nil, если ссылка не сконфигурирована. Telegram отклоняет всю клавиатуру при
// битом URL, поэтому пустую ссылку не добавляем.
func chartButtonRow(chartURL string) []tgbotapi.InlineKeyboardButton {
	if chartURL == "" {
		return nil
	}
	return tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonURL("📈 График цены", chartURL),
	)
}

// trackOOSKeyboard — клавиатура для товара БЕЗ активного оффера. Три стратегии,
// как у обычного товара, но вместо «любое снижение» — «в наличии»
// (back_in_stock, выбран по умолчанию → галочка). below_target/discount_pct
// показываем только при известной last-цене (hasPrice): без опорной цены
// процент скидки считать не от чего.
func trackOOSKeyboard(subID int64, hasPrice bool, chartURL string) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData(
			"✅ 🔔 Когда появится в наличии",
			fmt.Sprintf("ptrack:%d:stock", subID))},
	}
	if hasPrice {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(
				"📉 Ниже цены", fmt.Sprintf("ptrack:%d:below", subID)),
			tgbotapi.NewInlineKeyboardButtonData(
				"％ Скидка %", fmt.Sprintf("ptrack:%d:disc", subID)),
		})
	} else {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(
				"📉 Ниже цены", fmt.Sprintf("ptrack:%d:below", subID)),
		})
	}
	if row := chartButtonRow(chartURL); row != nil {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("📋 Мои подписки", "menu:list"),
		tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
	})
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// handleTrackTriggerCallback — обработка нажатия ptrack:<subID>:<kind>.
func (b *Bot) handleTrackTriggerCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	parts := strings.Split(strings.TrimPrefix(cb.Data, "ptrack:"), ":")
	if len(parts) != 2 {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	subID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	kind := parts[1]
	chatID := cb.Message.Chat.ID

	// subID берётся из callback_data (недоверенный ввод) — резолвим владельца и
	// прокидываем user.ID в репозиторий, чтобы нельзя было править чужую подписку.
	user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}

	switch kind {
	case "any":
		b.clearTrackFSM(ctx, cb.From.ID)
		if err := b.subRepo.SetTrigger(ctx, subID, user.ID, string(domain.TriggerAnyDrop), nil, nil); err != nil {
			b.log.Error("set trigger any", "sub_id", subID, "err", err)
			b.answerCallback(cb.ID, "Ошибка, попробуй позже")
			return
		}
		b.editMenu(chatID, cb.Message.MessageID,
			"🔔 Тип уведомления: "+domain.TriggerDescription(domain.TriggerAnyDrop, nil, nil),
			trackTriggerKeyboard(subID, domain.TriggerAnyDrop, b.chartURLForSub(ctx, subID)))
		b.answerCallback(cb.ID, "Готово")

	case "stock":
		// Товар без оффера: ждать появления в наличии (back_in_stock).
		b.clearTrackFSM(ctx, cb.From.ID)
		if err := b.subRepo.SetTrigger(ctx, subID, user.ID, string(domain.TriggerBackInStock), nil, nil); err != nil {
			b.log.Error("set trigger stock", "sub_id", subID, "err", err)
			b.answerCallback(cb.ID, "Ошибка, попробуй позже")
			return
		}
		// hasPrice — есть ли опорная last-цена (для below/disc в клавиатуре).
		hasPrice := false
		if s, err := b.subRepo.GetByID(ctx, subID); err == nil {
			hasPrice = s.FirstSeenPrice > 0
		}
		b.editMenu(chatID, cb.Message.MessageID,
			"🔔 Тип уведомления: "+domain.TriggerDescription(domain.TriggerBackInStock, nil, nil),
			trackOOSKeyboard(subID, hasPrice, b.chartURLForSub(ctx, subID)))
		b.answerCallback(cb.ID, "Готово")

	case "below":
		// Подсказки целевой цены (минимум за 90 дней / −5% / −10%) убраны — теперь
		// сразу переходим к ручному вводу цены пользователем.
		b.promptManualTarget(ctx, cb.From.ID, chatID, subID)
		b.answerCallback(cb.ID, "")

	case "disc":
		if err := b.setTrackFSM(ctx, cb.From.ID, trackFSM{SubID: subID, Trigger: string(domain.TriggerDiscountPct)}); err != nil {
			b.reply(chatID, "Не получилось начать ввод (нет связи с хранилищем). Останется «Любое снижение».")
			return
		}
		b.reply(chatID, "％ Введи процент скидки от текущей цены (1–99, например <code>20</code>).")
		b.answerCallback(cb.ID, "")

	default:
		b.answerCallback(cb.ID, "Ошибка")
	}
}

// handleTrackThreshold — приём числа (порог/процент) для товарной подписки.
func (b *Bot) handleTrackThreshold(ctx context.Context, chatID, tgID int64, text string, fsm trackFSM) {
	// fsm.SubID создавался для этого пользователя, но подстраховываемся фильтром по
	// владельцу в репозитории — резолвим user.ID по telegram_id.
	user, err := b.userRepo.GetByTelegramID(ctx, tgID)
	if err != nil {
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	switch domain.TriggerType(fsm.Trigger) {
	case domain.TriggerBelowTarget:
		price, err := domain.ParsePrice(text)
		if err != nil {
			b.reply(chatID, "Нужно число — цена в рублях, например <code>1499</code>. Или /menu чтобы отменить.")
			return
		}
		b.clearTrackFSM(ctx, tgID)
		if err := b.subRepo.SetTrigger(ctx, fsm.SubID, user.ID, string(domain.TriggerBelowTarget), &price, nil); err != nil {
			b.log.Error("set trigger below", "sub_id", fsm.SubID, "err", err)
			b.reply(chatID, "Произошла ошибка, попробуй позже.")
			return
		}
		if b.maybeInstantBelowTargetAlert(ctx, chatID, fsm.SubID, price) {
			return
		}
		b.confirmTrackTrigger(ctx, chatID, fsm.SubID, domain.TriggerBelowTarget, &price, nil)

	case domain.TriggerDiscountPct:
		pct, err := domain.ParsePct(text)
		if err != nil {
			b.reply(chatID, "Нужно целое число от 1 до 99, например <code>20</code>. Или /menu чтобы отменить.")
			return
		}
		b.clearTrackFSM(ctx, tgID)
		if err := b.subRepo.SetTrigger(ctx, fsm.SubID, user.ID, string(domain.TriggerDiscountPct), nil, &pct); err != nil {
			b.log.Error("set trigger disc", "sub_id", fsm.SubID, "err", err)
			b.reply(chatID, "Произошла ошибка, попробуй позже.")
			return
		}
		b.confirmTrackTrigger(ctx, chatID, fsm.SubID, domain.TriggerDiscountPct, nil, &pct)

	default:
		b.clearTrackFSM(ctx, tgID)
		b.reply(chatID, "Что-то пошло не так, начни заново через /menu.")
	}
}

// maybeInstantBelowTargetAlert — мгновенный алерт, если порог below_target уже
// выполнен в момент установки (текущая известная цена ≤ target). Без этого
// пользователь получал бы «Готово» и ждал уведомления до часа (следующий цикл
// скрейпа), хотя условие срабатывания было известно сразу.
// Возвращает true, если алерт отправлен — тогда вызывающий НЕ должен слать
// обычное confirmTrackTrigger (алерт уже содержит подтверждение установки порога).
func (b *Bot) maybeInstantBelowTargetAlert(ctx context.Context, chatID, subID int64, target float64) bool {
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
		b.log.Error("get product for instant alert", "sub_id", subID, "err", err)
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
		b.log.Warn("update baseline on instant alert", "sub_id", subID, "err", err)
	}

	text := fmt.Sprintf(
		"✅ Порог <b>%.0f ₽</b> установлен.\n\n"+
			"🎯 <b>%s</b> уже стоит <b>%.0f ₽</b> — ниже твоего порога!\n"+
			"Следующее уведомление пришлю, когда цена опустится ещё ниже.",
		target, product.Name, current,
	)
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	m.ReplyMarkup = trackTriggerKeyboard(subID, domain.TriggerBelowTarget, b.chartURL(product.PublicID))
	b.send(m)
	return true
}

func (b *Bot) confirmTrackTrigger(ctx context.Context, chatID, subID int64, t domain.TriggerType, target *float64, pct *int16) {
	m := tgbotapi.NewMessage(chatID, "✅ <b>Готово!</b> "+domain.TriggerDescription(t, target, pct))
	m.ParseMode = "HTML"
	kb := trackTriggerKeyboard(subID, t, b.chartURLForSub(ctx, subID))
	m.ReplyMarkup = kb
	b.send(m)
}

// promptManualTarget — запросить ручной ввод целевой цены (фолбэк, как было).
func (b *Bot) promptManualTarget(ctx context.Context, tgID, chatID, subID int64) {
	if err := b.setTrackFSM(ctx, tgID, trackFSM{SubID: subID, Trigger: string(domain.TriggerBelowTarget)}); err != nil {
		b.reply(chatID, "Не получилось начать ввод (нет связи с хранилищем). Останется «Любое снижение».")
		return
	}
	b.reply(chatID, "💰 Введи целевую цену в рублях (например <code>1499</code>).\nУведомлю, когда цена опустится до неё или ниже.")
}

// handleTrackTargetCallback — выбор целевой цены: ptgt:<subID>:<rub> ставит триггер
// сразу, ptgt:<subID>:manual → ручной ввод. Владельца резолвим по telegram_id.
// Кнопки с подсказками больше не создаются (см. "below" в handleTrackTriggerCallback),
// но обработчик оставлен для callback'ов ptgt из старых сообщений, которые уже
// разосланы пользователям.
func (b *Bot) handleTrackTargetCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	parts := strings.Split(strings.TrimPrefix(cb.Data, "ptgt:"), ":")
	if len(parts) != 2 {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	subID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	chatID := cb.Message.Chat.ID

	if parts[1] == "manual" {
		b.promptManualTarget(ctx, cb.From.ID, chatID, subID)
		b.answerCallback(cb.ID, "")
		return
	}

	price, err := domain.ParsePrice(parts[1])
	if err != nil || price <= 0 {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	b.clearTrackFSM(ctx, cb.From.ID)
	if err := b.subRepo.SetTrigger(ctx, subID, user.ID, string(domain.TriggerBelowTarget), &price, nil); err != nil {
		b.log.Error("set trigger below (suggested)", "sub_id", subID, "err", err)
		b.answerCallback(cb.ID, "Ошибка, попробуй позже")
		return
	}
	if b.maybeInstantBelowTargetAlert(ctx, chatID, subID, price) {
		b.answerCallback(cb.ID, "Готово")
		return
	}
	b.confirmTrackTrigger(ctx, chatID, subID, domain.TriggerBelowTarget, &price, nil)
	b.answerCallback(cb.ID, "Готово")
}

// ── Подписки ──────────────────────────────────────────────────────────────────
func (b *Bot) handleList(ctx context.Context, chatID int64, user *domain.User) {
	subs, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("get subscriptions", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	if len(subs) == 0 {
		m := tgbotapi.NewMessage(chatID,
			"📋 У тебя пока нет активных подписок.\n\n"+
				"Отправь ссылку на товар Wildberries прямо в чат — я начну отслеживать цену.",
		)
		m.ParseMode = "HTML"
		m.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
			),
		)
		b.send(m)
		return
	}

	text, keyboard := b.buildListView(subs)
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	m.ReplyMarkup = keyboard
	b.send(m)
}

func (b *Bot) buildListView(subs []*domain.Subscription) (string, tgbotapi.InlineKeyboardMarkup) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 <b>Твои подписки — %d активных</b>\n\n", len(subs))

	for i, sub := range subs {
		priceEmoji := ""
		if sub.CurrentPrice > 0 && sub.CurrentPrice < sub.FirstSeenPrice {
			priceEmoji = "📉 "
		}
		currentPriceStr := "нет данных"
		if sub.CurrentPrice > 0 {
			currentPriceStr = fmt.Sprintf("%s%.0f ₽", priceEmoji, sub.CurrentPrice)
		}

		fmt.Fprintf(&sb, "%d. %s <b>%s</b>\n   сейчас %s  |  при подписке %.0f ₽\n   %s\n\n",
			i+1, marketplaceIcon(sub.ProductMarketplace), sub.ProductName, currentPriceStr, sub.FirstSeenPrice,
			domain.TriggerDescription(sub.TriggerType, sub.TargetPrice, sub.DiscountPct),
		)
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for i, sub := range subs {
		var row []tgbotapi.InlineKeyboardButton
		// URL-кнопку добавляем ТОЛЬКО при валидной ссылке: Telegram отклоняет ВСЮ
		// клавиатуру, если хоть один URL битый (в БД встречаются записи вида
		// «Название\nссылка»). Битый URL → строка только с кнопкой отмены.
		if u := safeButtonURL(sub.ProductURL); u != "" {
			row = append(row, tgbotapi.NewInlineKeyboardButtonURL(
				fmt.Sprintf("🔗 #%d %s", i+1, truncate(sub.ProductName, 16)), u))
		}
		if cu := b.chartURL(sub.ProductPublicID); cu != "" {
			row = append(row, tgbotapi.NewInlineKeyboardButtonURL(fmt.Sprintf("📈 #%d", i+1), cu))
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("❌ Отменить #%d", i+1),
			fmt.Sprintf("untrack:%d", sub.ID),
		))
		rows = append(rows, row)
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ В меню", "menu:main"),
	))

	return sb.String(), tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// safeButtonURL возвращает валидный http(s)-URL для inline-кнопки или "" если
// ссылка битая. В БД встречаются записи, где в url попал весь текст сообщения
// («Название\nhttps://…») — Telegram отклоняет ВСЮ клавиатуру из-за одной такой
// кнопки, и весь список перестаёт открываться. Из строки с пробелами/переводами
// вытаскиваем первую http(s)-ссылку; если её нет или схема не http(s) — "".
func safeButtonURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		raw = bulkURLRe.FindString(raw)
		if raw == "" {
			return ""
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return raw
}

func (b *Bot) handleUntrack(ctx context.Context, msg *tgbotapi.Message) {
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		b.reply(msg.Chat.ID, "Укажи номер подписки из /list.\nПример: /untrack 3")
		return
	}
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		b.reply(msg.Chat.ID, "Номер подписки должен быть числом.")
		return
	}
	user, err := b.userRepo.GetByTelegramID(ctx, msg.From.ID)
	if err != nil {
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}
	if err := b.subRepo.Deactivate(ctx, id, user.ID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.reply(msg.Chat.ID, "Подписка не найдена.")
			return
		}
		b.log.Error("deactivate subscription", "err", err)
		b.reply(msg.Chat.ID, "Произошла ошибка, попробуй позже.")
		return
	}
	b.reply(msg.Chat.ID, "✅ Отслеживание отменено.")
}

func (b *Bot) doTrack(ctx context.Context, chatID int64, rawURL string, user *domain.User) {
	tracer := otel.Tracer("bot")
	ctx, span := tracer.Start(ctx, "bot.handleTrack",
		trace.WithAttributes(
			attribute.String("url", rawURL),
			attribute.Int64("user.id", user.ID),
			attribute.Int64("chat.id", chatID),
		),
	)
	defer span.End()

	s, err := b.registry.FindByURL(rawURL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		supported := b.registry.SupportedMarketplaces()
		b.reply(chatID, fmt.Sprintf(
			"Не могу распознать ссылку.\n\nПоддерживаемые маркетплейсы: %v\n\n"+
				"Пример ссылки:\n<code>https://www.wildberries.ru/catalog/123456789/detail.aspx</code>",
			supported,
		))
		return
	}

	span.SetAttributes(attribute.String("marketplace", string(s.Marketplace())))

	wait := tgbotapi.NewMessage(chatID, "⏳ Получаю данные о товаре...")
	wait.ParseMode = "HTML"
	sent, _ := b.api.Send(wait)

	// Через registry.Scrape (а не s.Scrape напрямую), чтобы инкрементить
	// tryberrybot_scrape_requests_total. Иначе ручные /track-скрейпы невидимы
	// метрике, и success-rate/алерт HighScrapeErrorRate считаются только по
	// фоновому воркеру — success-rate смещён. Маркетплейс уже знаем из s.
	result, _, err := b.registry.Scrape(ctx, rawURL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		b.log.Error("scrape on track", "url", rawURL, "marketplace", s.Marketplace(), "err", err)

		msg := "❌ Не удалось получить данные о товаре. Попробуй позже."
		switch {
		case errors.Is(err, scraper.ErrAgeRestricted):
			// 18+ товар: цена скрыта за возрастным гейтом Ozon — понятное сообщение
			// вместо «не удалось».
			msg = "🔞 Это товар <b>18+</b>. Ozon прячет его цену за подтверждением возраста — пока не могу отслеживать такие товары."
		case s.Marketplace() == scraper.MarketplaceOzon &&
			(errors.Is(err, scraper.ErrNotImplemented) || errors.Is(err, scraper.ErrMarketplaceBlocked)):
			// Заглушка: Ozon ещё в разработке (антибот/прокси). Не пугаем «ошибкой» —
			// показываем понятное «скоро будет». Когда Ozon заработает стабильно,
			// сюда дойдёт обычный успешный путь, и заглушка не сработает.
			msg = ozonComingSoonMsg
		case errors.Is(err, scraper.ErrNotImplemented):
			msg = fmt.Sprintf("⚠️ Маркетплейс <b>%s</b> пока не поддерживается. Сейчас доступен только Wildberries.", s.Marketplace())
		}

		edit := tgbotapi.NewEditMessageText(chatID, sent.MessageID, msg)
		edit.ParseMode = "HTML"
		b.api.Send(edit) //nolint:errcheck
		return
	}

	span.SetAttributes(
		attribute.String("product.name", result.Name),
		attribute.Float64("product.price", result.Price),
	)

	product, err := b.prodRepo.Upsert(ctx, rawURL, result.Name, result.ImageURL, string(s.Marketplace()))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		b.log.Error("upsert product", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	// Лимит тарифа на товарные подписки. Повторная ссылка на уже
	// отслеживаемый товар лимит не расходует (это обновление, не новая).
	plan := user.EffectivePlan(time.Now())
	active, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		span.RecordError(err)
		b.log.Error("count active subs", "err", err)
		edit := tgbotapi.NewEditMessageText(chatID, sent.MessageID, "Произошла ошибка, попробуй позже.")
		edit.ParseMode = "HTML"
		b.api.Send(edit) //nolint:errcheck
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
		metrics.TrackCommands.WithLabelValues("limit").Inc()
		edit := tgbotapi.NewEditMessageText(chatID, sent.MessageID, productLimitText(plan, len(active)))
		edit.ParseMode = "HTML"
		b.api.Send(edit) //nolint:errcheck
		return
	}

	// Карточка без активного оффера («нет в продаже»): цены нет, обычное «слежу за
	// снижением» неприменимо. Заводим подписку с триггером back_in_stock и
	// предлагаем выбор — ждать наличие или указать целевую цену. in_stock=false
	// фиксируем явно, чтобы последующий скрейп с ценой дал переход false→true.
	if !result.InStock {
		if err := b.prodRepo.SetInStock(ctx, product.ID, false); err != nil {
			b.log.Warn("set product out of stock", "product_id", product.ID, "err", err)
		}
		// Последнюю известную цену берём из НАШЕЙ истории (внутри UpsertOutOfStock),
		// а не из result.Price: там «справочная» цена из стейта МП, и она врёт.
		sub, _, err := b.subRepo.UpsertOutOfStock(ctx, user.ID, product.ID)
		if err != nil {
			span.RecordError(err)
			metrics.TrackCommands.WithLabelValues("error").Inc()
			b.log.Error("upsert oos subscription", "err", err)
			b.reply(chatID, "Произошла ошибка, попробуй позже.")
			return
		}
		metrics.TrackCommands.WithLabelValues("success").Inc()

		priceLine := "Цена появится, когда товар вернётся в продажу."
		if sub.BaselinePrice > 0 {
			priceLine = fmt.Sprintf("💰 Последняя цена: <b>%.0f ₽</b>", sub.BaselinePrice)
		}
		text := fmt.Sprintf(
			"✅ <b>Добавил в отслеживание!</b>\n\n"+
				"<b>%s</b>\n"+
				"🚫 Сейчас товара <b>нет в наличии</b> (нет активного предложения).\n"+
				"%s\n\n"+
				"По умолчанию уведомлю, как только он <b>появится в наличии</b>. "+
				"Можно сменить тип уведомления кнопками ниже 👇",
			result.Name, priceLine,
		)
		// 3 стратегии как у обычного товара, но «любое снижение» → «в наличии».
		// below_target/discount_pct показываем только при известной last-цене
		// (есть опора): без неё процент скидки считать не от чего.
		kb := trackOOSKeyboard(sub.ID, sub.BaselinePrice > 0, b.chartURL(product.PublicID))
		edit := tgbotapi.NewEditMessageText(chatID, sent.MessageID, text)
		edit.ParseMode = "HTML"
		edit.ReplyMarkup = &kb
		b.api.Send(edit) //nolint:errcheck
		return
	}

	sub, created, err := b.subRepo.Upsert(ctx, user.ID, product.ID, result.Price)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		metrics.TrackCommands.WithLabelValues("error").Inc()
		b.log.Error("upsert subscription", "err", err)
		b.reply(chatID, "Произошла ошибка, попробуй позже.")
		return
	}

	span.SetAttributes(attribute.Bool("subscription.created", created))

	var responseText string
	if created {
		metrics.TrackCommands.WithLabelValues("success").Inc()
		responseText = fmt.Sprintf(
			"✅ <b>Добавил в отслеживание!</b>\n\n"+
				"<b>%s</b>\n"+
				"💰 Текущая цена: <b>%.0f ₽</b>\n\n"+
				"🔔 Сейчас уведомлю при <b>любом снижении</b>. Можно сменить тип уведомления кнопками ниже 👇",
			result.Name, result.Price,
		)
	} else {
		metrics.TrackCommands.WithLabelValues("reactivated").Inc()
		responseText = fmt.Sprintf(
			"🔄 <b>Отслеживание возобновлено!</b>\n\n"+
				"<b>%s</b>\n"+
				"💰 Текущая цена: <b>%.0f ₽</b>\n\n"+
				"🔔 Тип уведомления: <b>любое снижение</b>. Сменить — кнопками ниже 👇",
			result.Name, result.Price,
		)
	}

	keyboard := trackTriggerKeyboard(sub.ID, domain.TriggerAnyDrop, b.chartURL(product.PublicID))

	edit := tgbotapi.NewEditMessageText(chatID, sent.MessageID, responseText)
	edit.ParseMode = "HTML"
	edit.ReplyMarkup = &keyboard
	b.api.Send(edit) //nolint:errcheck
}

func (b *Bot) callbackUntrack(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	idStr := strings.TrimPrefix(cb.Data, "untrack:")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}

	// id — из callback_data; гасим только если подписка принадлежит этому юзеру.
	user, err := b.userRepo.GetByTelegramID(ctx, cb.From.ID)
	if err != nil {
		b.answerCallback(cb.ID, "Ошибка")
		return
	}
	if err := b.subRepo.Deactivate(ctx, id, user.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		b.log.Error("deactivate via callback", "err", err)
		b.answerCallback(cb.ID, "Ошибка, попробуй позже")
		return
	}

	subs, err := b.subRepo.GetActiveByUserID(ctx, user.ID)
	if err == nil && len(subs) > 0 {
		text, keyboard := b.buildListView(subs)
		b.editMenu(cb.Message.Chat.ID, cb.Message.MessageID, text, keyboard)
		b.answerCallback(cb.ID, "✅ Отслеживание отменено")
		return
	}

	b.sendMainMenu(ctx, cb.Message.Chat.ID, cb.Message.MessageID, true)
	b.answerCallback(cb.ID, "✅ Отслеживание отменено")
}
