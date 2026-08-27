package max

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/searchsub"
)

// Поиск-подписки из MAX: тот же флоу, что в telegram/vk — ссылка → выбор типа
// триггера → ввод числа через FSM в Redis (ключ max_search_fsm).

const fsmTTL = 10 * time.Minute

type searchFSM struct {
	QueryID   int64  `json:"q"`
	Trigger   string `json:"t"`
	SellerURL string `json:"u,omitempty"`
}

func fsmKey(maxID int64) string { return fmt.Sprintf("max_search_fsm:%d", maxID) }

func (b *Bot) getSearchFSM(ctx context.Context, maxID int64) (searchFSM, bool) {
	if b.rdb == nil {
		return searchFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, fsmKey(maxID)).Result()
	if err != nil {
		return searchFSM{}, false
	}
	var fsm searchFSM
	if err := json.Unmarshal([]byte(raw), &fsm); err != nil {
		return searchFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setSearchFSM(ctx context.Context, maxID int64, fsm searchFSM) error {
	if b.rdb == nil {
		return errors.New("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, fsmKey(maxID), raw, fsmTTL).Err()
}

func (b *Bot) clearSearchFSM(ctx context.Context, maxID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, fsmKey(maxID))
}

// ── Старт подписки: ссылка → выбор типа триггера ──────────────────────────────

func (b *Bot) startSearchTrack(ctx context.Context, maxID int64, user *domain.User, rawURL string) {
	kb := menuKeyboard(user)

	plan := user.EffectivePlan(time.Now())
	cnt, err := b.searchSubRepo.CountActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("max: count search subs", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if cnt >= plan.MaxSearch {
		b.send(ctx, maxID, b.searchLimitText(plan, cnt), kb)
		return
	}

	ss, err := b.registry.FindSearchByURL(rawURL)
	if err != nil {
		b.send(ctx, maxID, "Это не похоже на поисковую ссылку Wildberries. Нужна ссылка с параметром поиска.", kb)
		return
	}

	if !b.checkSellerCap(ctx, maxID, kb, ss, rawURL) {
		return
	}

	if _, isSeller := ss.(sellerSizer); isSeller && !domain.HasTextFilter(rawURL) {
		b.promptSellerTextFilter(ctx, maxID, user, rawURL)
		return
	}

	b.proceedSearchTrack(ctx, maxID, user, rawURL)
}

func (b *Bot) proceedSearchTrack(ctx context.Context, maxID int64, user *domain.User, rawURL string) {
	ss, err := b.registry.FindSearchByURL(rawURL)
	if err != nil {
		b.send(ctx, maxID, "Это не похоже на поисковую ссылку Wildberries. Нужна ссылка с параметром поиска.", menuKeyboard(user))
		return
	}
	normalized, err := ss.NormalizeSearchURL(rawURL)
	if err != nil {
		b.send(ctx, maxID, "Не получилось разобрать поисковый запрос из ссылки. Проверь, что в ней есть текст поиска.", menuKeyboard(user))
		return
	}

	queryText := domain.QueryTextFromNormalized(normalized)
	if sz, ok := ss.(sellerSizer); ok {
		if name, err := sz.SellerName(ctx, rawURL); err == nil && name != "" {
			queryText = domain.SellerLabel(name, rawURL)
		}
	}

	sq, _, err := b.searchQueryRepo.Upsert(ctx, string(ss.Marketplace()), normalized, queryText, nil)
	if err != nil {
		b.log.Error("max: upsert search query", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	triggerKB := &Keyboard{Buttons: [][]Button{
		{TextButton("📉 Ниже цены", searchTriggerPayload(sq.ID, "below"), ColorPrimary)},
		{TextButton("🔻 Любое снижение", searchTriggerPayload(sq.ID, "any"), ColorPrimary)},
		{TextButton("％ Скидка от %", searchTriggerPayload(sq.ID, "disc"), ColorPrimary)},
	}}
	b.send(ctx, maxID, fmt.Sprintf("🔎 Запрос: «%s»\n\nКак уведомлять о снижении цены?", sq.QueryText), triggerKB)
}

func (b *Bot) promptSellerTextFilter(ctx context.Context, maxID int64, user *domain.User, rawURL string) {
	if err := b.setSearchFSM(ctx, maxID, searchFSM{SellerURL: rawURL}); err != nil {
		b.log.Error("max: set seller text fsm", "err", err)
		b.proceedSearchTrack(ctx, maxID, user, rawURL)
		return
	}
	kb := &Keyboard{Buttons: [][]Button{
		{TextButton("⏭ Без фильтра", fmt.Sprintf(`{"cmd":%q}`, cmdSFSkip), ColorSecondary)},
	}}
	b.send(ctx, maxID, "🏬 Магазин распознан.\n\n"+
		"Следить за всеми товарами или только за частью? Пришли слово — оставлю карточки, "+
		"в названии которых оно есть (например «iphone 17»).\n\n"+
		"Или нажми «Без фильтра», чтобы следить за всей выдачей.", kb)
}

func (b *Bot) handleSellerTextFilter(ctx context.Context, maxID int64, user *domain.User, text string, fsm searchFSM) {
	b.clearSearchFSM(ctx, maxID)

	if _, err := b.registry.FindSearchByURL(text); err == nil {
		b.startSearchTrack(ctx, maxID, user, text)
		return
	}

	rawURL := fsm.SellerURL
	if t := strings.TrimSpace(text); t != "" {
		rawURL = domain.AppendTextFilter(rawURL, t)
	}
	b.proceedSearchTrack(ctx, maxID, user, rawURL)
}

func (b *Bot) handleSellerSkipFilter(ctx context.Context, maxID int64, user *domain.User) {
	fsm, ok := b.getSearchFSM(ctx, maxID)
	if !ok || fsm.SellerURL == "" {
		return
	}
	b.clearSearchFSM(ctx, maxID)
	b.proceedSearchTrack(ctx, maxID, user, fsm.SellerURL)
}

func searchTriggerPayload(queryID int64, kind string) string {
	return fmt.Sprintf(`{"cmd":%q,"id":%d,"k":%q}`, cmdSTrack, queryID, kind)
}

// sellerSizer — скрейпер витрины продавца WB: размер выдачи + потолок (CAP).
type sellerSizer interface {
	SellerTotal(ctx context.Context, rawURL string) (int, error)
	SellerName(ctx context.Context, rawURL string) (string, error)
	MaxItems() int
}

func (b *Bot) checkSellerCap(ctx context.Context, maxID int64, kb *Keyboard, ss interface{}, rawURL string) bool {
	sz, ok := ss.(sellerSizer)
	if !ok {
		return true
	}
	total, err := sz.SellerTotal(ctx, rawURL)
	if err != nil {
		b.log.Error("max: seller total", "err", err)
		return true
	}
	if total == 0 {
		b.send(ctx, maxID, "🏬 В этом магазине по такой ссылке нет товаров. Проверь ссылку или ослабь фильтры.", kb)
		return false
	}
	if limit := sz.MaxItems(); total > limit {
		b.send(ctx, maxID, fmt.Sprintf(
			"🏬 В выдаче %d товаров — это больше лимита (%d).\n\n"+
				"Сузь выбор фильтрами на сайте (категория, бренд, модель) и пришли ссылку снова. "+
				"Можно добавить в конец ссылки &tb_q=текст — оставлю только карточки с этим текстом в названии.",
			total, limit), kb)
		return false
	}
	return true
}

// ── Выбор типа триггера (кнопка strack) ───────────────────────────────────────

func (b *Bot) handleSearchTrigger(ctx context.Context, maxID int64, user *domain.User, p payloadData) {
	switch p.Kind {
	case "any":
		b.createSearchSub(ctx, maxID, user, p.ID, domain.TriggerAnyDrop, nil, nil)
	case "below":
		if err := b.setSearchFSM(ctx, maxID, searchFSM{QueryID: p.ID, Trigger: string(domain.TriggerBelowTarget)}); err != nil {
			b.send(ctx, maxID, "Не получилось начать ввод (нет связи с хранилищем). Попробуй «Любое снижение».", nil)
			return
		}
		b.send(ctx, maxID, "💰 Введи целевую цену в рублях (например 59990).\nУведомлю, когда найдётся товар дешевле.", nil)
	case "disc":
		if err := b.setSearchFSM(ctx, maxID, searchFSM{QueryID: p.ID, Trigger: string(domain.TriggerDiscountPct)}); err != nil {
			b.send(ctx, maxID, "Не получилось начать ввод (нет связи с хранилищем). Попробуй «Любое снижение».", nil)
			return
		}
		b.send(ctx, maxID, "％ Введи процент скидки от стартовой цены (1–99, например 20).", nil)
	}
}

// ── Ввод числа (порог/процент) ────────────────────────────────────────────────

func (b *Bot) handleSearchThreshold(ctx context.Context, maxID int64, user *domain.User, text string, fsm searchFSM) {
	switch domain.TriggerType(fsm.Trigger) {
	case domain.TriggerBelowTarget:
		price, err := domain.ParsePrice(text)
		if err != nil {
			b.send(ctx, maxID, "Нужно число — цена в рублях, например 59990. Любая кнопка внизу отменит ввод.", nil)
			return
		}
		b.clearSearchFSM(ctx, maxID)
		b.createSearchSub(ctx, maxID, user, fsm.QueryID, domain.TriggerBelowTarget, &price, nil)

	case domain.TriggerDiscountPct:
		pct, err := domain.ParsePct(text)
		if err != nil {
			b.send(ctx, maxID, "Нужно целое число от 1 до 99, например 20. Любая кнопка внизу отменит ввод.", nil)
			return
		}
		b.clearSearchFSM(ctx, maxID)
		b.createSearchSub(ctx, maxID, user, fsm.QueryID, domain.TriggerDiscountPct, nil, &pct)

	default:
		b.clearSearchFSM(ctx, maxID)
		b.send(ctx, maxID, "Что-то пошло не так, отправь поисковую ссылку заново.", menuKeyboard(user))
	}
}

func (b *Bot) createSearchSub(ctx context.Context, maxID int64, user *domain.User, queryID int64, trigger domain.TriggerType, target *float64, pct *int16) {
	plan := user.EffectivePlan(time.Now())
	if cnt, err := b.searchSubRepo.CountActiveByUserID(ctx, user.ID); err == nil && cnt >= plan.MaxSearch {
		b.send(ctx, maxID, b.searchLimitText(plan, cnt), menuKeyboard(user))
		return
	}

	sub, err := b.searchSubRepo.Create(ctx, &domain.SearchSubscription{
		UserID:        user.ID,
		SearchQueryID: queryID,
		TriggerType:   trigger,
		TargetPrice:   target,
		DiscountPct:   pct,
	})
	if err != nil {
		b.log.Error("max: create search subscription", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	if _, err := b.searchSubRepo.BackfillBaselines(ctx, sub.ID, queryID); err != nil {
		b.log.Warn("max: backfill baselines", "sub_id", sub.ID, "err", err)
	}

	// Мгновенная первая оценка below_target по сохранённой выдаче — паритет с TG.
	if b.searchResults != nil && b.searchEvents != nil {
		if q, err := b.searchQueryRepo.GetByID(ctx, queryID); err == nil {
			n, err := searchsub.SendInstantBelowTarget(ctx, b.searchResults, b.searchSubRepo, b.searchEvents, sub, q, user.TelegramID)
			if err != nil {
				b.log.Warn("max: instant search eval", "sub_id", sub.ID, "err", err)
			} else if n > 0 {
				b.log.Info("max: instant search hits queued", "sub_id", sub.ID, "items", n)
			}
		}
	}

	b.send(ctx, maxID, "✅ Готово! "+domain.TriggerDescription(trigger, target, pct)+
		"\n\nПроверяю выдачу регулярно и пришлю, когда товары подешевеют 🔔",
		menuKeyboard(user))
}

// ── Список поиск-подписок ─────────────────────────────────────────────────────

func (b *Bot) handleListSearch(ctx context.Context, maxID int64, user *domain.User, prefix string) {
	b.showSearchList(ctx, maxID, user, prefix, 0)
}

func (b *Bot) showSearchList(ctx context.Context, maxID int64, user *domain.User, prefix string, page int) {
	subs, err := b.searchSubRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("max: get search subscriptions", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if len(subs) == 0 {
		text := "📡 У тебя пока нет поиск-подписок.\n\nОтправь ссылку на поисковую выдачу Wildberries — буду следить за всей выдачей."
		if prefix != "" {
			text = prefix + "\n\n" + text
		}
		b.send(ctx, maxID, text, menuKeyboard(user))
		return
	}
	pages, start, end := pageBounds(len(subs), page)
	page = clampPage(page, pages)

	var sb strings.Builder
	if prefix != "" {
		sb.WriteString(prefix + "\n\n")
	}
	fmt.Fprintf(&sb, "📡 Поиск-подписки — %d активных", len(subs))
	if pages > 1 {
		fmt.Fprintf(&sb, " (стр. %d/%d)", page+1, pages)
	}
	sb.WriteString("\n\n")
	interval := user.EffectivePlan(time.Now()).SearchInterval
	now := time.Now()
	for i := start; i < end; i++ {
		s := subs[i]
		fmt.Fprintf(&sb, "%d. %s\n   %s\n   %s\n",
			i+1, s.QueryText, domain.TriggerDescription(s.TriggerType, s.TargetPrice, s.DiscountPct), s.NormalizedURL)
		// Молчание подписки под блоком площадки неотличимо от «цены не падали» —
		// поэтому застоявшуюся выдачу проговариваем прямо в списке.
		if note := domain.StaleSearchNote(s.LastScrapedAt, s.CreatedAt, interval, now); note != "" {
			fmt.Fprintf(&sb, "   %s\n", note)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Отписаться — кнопки «❌ номер» под сообщением 👇")

	var rows [][]Button
	var row []Button
	for i := start; i < end; i++ {
		row = append(row, TextButton(
			fmt.Sprintf("❌ %d", i+1),
			fmt.Sprintf(`{"cmd":%q,"id":%d}`, cmdSUntrack, subs[i].ID),
			ColorSecondary))
		if len(row) == 4 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	if nav := pageNavRow(cmdLSearchPage, page, pages); nav != nil {
		rows = append(rows, nav)
	}
	b.send(ctx, maxID, sb.String(), &Keyboard{Buttons: rows})
}

func (b *Bot) handleUntrackSearch(ctx context.Context, maxID int64, user *domain.User, subID int64) {
	if subID == 0 {
		b.send(ctx, maxID, b.welcomeText(user), menuKeyboard(user))
		return
	}
	if err := b.searchSubRepo.Deactivate(ctx, subID, user.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		b.log.Error("max: deactivate search sub", "err", err)
		b.send(ctx, maxID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	b.handleListSearch(ctx, maxID, user, "✅ Поиск-подписка отменена.")
}

func (b *Bot) searchLimitText(plan domain.Plan, used int) string {
	if plan.MaxSearch == 0 {
		return "🔎 Поиск-подписки на тарифе " + plan.Title + " недоступны.\n\n" +
			"Они есть на тарифах Lite и выше — кнопка «Тарифы». А ещё можно попробовать бесплатный триал — кнопка «Триал»."
	}
	if plan.Name == "free" {
		metrics.SearchUpsellShown.WithLabelValues("max").Inc()
		return fmt.Sprintf(
			"🚫 На тарифе Free доступна одна поиск-подписка (%d из %d занято), проверка — %s.\n\n"+
				"Больше поисков и проверка чаще — на тарифах Lite и Pro (кнопка «Тарифы»). Новым пользователям — бесплатный триал на 10 дней (кнопка «Триал»).\n\n"+
				"Отменить ненужное — «Мои поиски».",
			used, plan.MaxSearch, domain.IntervalPhrase(plan.EffectiveSearchInterval(plan.Interval)))
	}
	return fmt.Sprintf(
		"🚫 Достигнут лимит поиск-подписок тарифа %s: %d из %d.\n\n"+
			"Отпишись от ненужного («Мои поиски») или оформи тариф повыше («Тарифы»).",
		plan.Title, used, plan.MaxSearch)
}
