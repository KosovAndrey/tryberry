package vk

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

// Поиск-подписки из VK: тот же флоу, что в telegram/search.go — ссылка →
// выбор типа триггера (inline-кнопки) → для порога/процента ввод числа
// через FSM в Redis (ключ свой, vk_search_fsm, состояние общее между репликами).

const fsmTTL = 10 * time.Minute

type searchFSM struct {
	QueryID   int64  `json:"q"`
	Trigger   string `json:"t"`           // domain.TriggerBelowTarget | domain.TriggerDiscountPct
	SellerURL string `json:"u,omitempty"` // задан → ждём текст-фильтр для этой витрины продавца
}

func fsmKey(vkID int64) string { return fmt.Sprintf("vk_search_fsm:%d", vkID) }

func (b *Bot) getSearchFSM(ctx context.Context, vkID int64) (searchFSM, bool) {
	if b.rdb == nil {
		return searchFSM{}, false
	}
	raw, err := b.rdb.Get(ctx, fsmKey(vkID)).Result()
	if err != nil {
		return searchFSM{}, false // redis.Nil или ошибка → состояния нет
	}
	var fsm searchFSM
	if err := json.Unmarshal([]byte(raw), &fsm); err != nil {
		return searchFSM{}, false
	}
	return fsm, true
}

func (b *Bot) setSearchFSM(ctx context.Context, vkID int64, fsm searchFSM) error {
	if b.rdb == nil {
		return errors.New("redis unavailable")
	}
	raw, err := json.Marshal(fsm)
	if err != nil {
		return err
	}
	return b.rdb.Set(ctx, fsmKey(vkID), raw, fsmTTL).Err()
}

func (b *Bot) clearSearchFSM(ctx context.Context, vkID int64) {
	if b.rdb == nil {
		return
	}
	b.rdb.Del(ctx, fsmKey(vkID))
}

// ── Старт подписки: ссылка → выбор типа триггера ──────────────────────────────

func (b *Bot) startSearchTrack(ctx context.Context, vkID int64, user *domain.User, rawURL string) {
	kb := menuKeyboard(user.TelegramID != 0)

	// Фейл-фаст: лимит/недоступность поиска на тарифе — сразу объясняем.
	plan := user.EffectivePlan(time.Now())
	cnt, err := b.searchSubRepo.CountActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("vk: count search subs", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if cnt >= plan.MaxSearch {
		b.send(ctx, vkID, b.searchLimitText(plan, cnt), kb)
		return
	}

	ss, err := b.registry.FindSearchByURL(rawURL)
	if err != nil {
		b.send(ctx, vkID, "Это не похоже на поисковую ссылку Wildberries. Нужна ссылка с параметром поиска.", kb)
		return
	}

	// Гейт CAP для витрины продавца WB (для не-seller-ссылок no-op).
	if !b.checkSellerCap(ctx, vkID, kb, ss, rawURL) {
		return
	}

	// Витрина продавца без явного текст-фильтра → предложить добавить его.
	if _, isSeller := ss.(sellerSizer); isSeller && !domain.HasTextFilter(rawURL) {
		b.promptSellerTextFilter(ctx, vkID, user, rawURL)
		return
	}

	b.proceedSearchTrack(ctx, vkID, user, rawURL)
}

// proceedSearchTrack — финал: нормализуем URL, заводим запрос, показываем выбор
// триггера. Лимит/CAP уже проверены в startSearchTrack.
func (b *Bot) proceedSearchTrack(ctx context.Context, vkID int64, user *domain.User, rawURL string) {
	ss, err := b.registry.FindSearchByURL(rawURL)
	if err != nil {
		b.send(ctx, vkID, "Это не похоже на поисковую ссылку Wildberries. Нужна ссылка с параметром поиска.", menuKeyboard(user.TelegramID != 0))
		return
	}
	normalized, err := ss.NormalizeSearchURL(rawURL)
	if err != nil {
		b.send(ctx, vkID, "Не получилось разобрать поисковый запрос из ссылки. Проверь, что в ней есть текст поиска.", menuKeyboard(user.TelegramID != 0))
		return
	}

	// Витрина продавца → ярлык с именем магазина (вместо «Магазин #{id}»).
	queryText := domain.QueryTextFromNormalized(normalized)
	if sz, ok := ss.(sellerSizer); ok {
		if name, err := sz.SellerName(ctx, rawURL); err == nil && name != "" {
			queryText = domain.SellerLabel(name, rawURL)
		}
	}

	sq, _, err := b.searchQueryRepo.Upsert(ctx, string(ss.Marketplace()), normalized, queryText, nil)
	if err != nil {
		b.log.Error("vk: upsert search query", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	triggerKB := &Keyboard{Inline: true, Buttons: [][]Button{
		{TextButton("📉 Ниже цены", searchTriggerPayload(sq.ID, "below"), ColorPrimary)},
		{TextButton("🔻 Любое снижение", searchTriggerPayload(sq.ID, "any"), ColorPrimary)},
		{TextButton("％ Скидка от %", searchTriggerPayload(sq.ID, "disc"), ColorPrimary)},
	}}
	b.send(ctx, vkID, fmt.Sprintf("🔎 Запрос: «%s»\n\nКак уведомлять о снижении цены?", sq.QueryText), triggerKB)
}

// promptSellerTextFilter — спросить опциональный текст-фильтр для витрины
// продавца: FSM (ждём слово) + кнопка «Без фильтра».
func (b *Bot) promptSellerTextFilter(ctx context.Context, vkID int64, user *domain.User, rawURL string) {
	if err := b.setSearchFSM(ctx, vkID, searchFSM{SellerURL: rawURL}); err != nil {
		// Без Redis шаг недоступен — подключаем магазин целиком.
		b.log.Error("vk: set seller text fsm", "err", err)
		b.proceedSearchTrack(ctx, vkID, user, rawURL)
		return
	}
	kb := &Keyboard{Inline: true, Buttons: [][]Button{
		{TextButton("⏭ Без фильтра", fmt.Sprintf(`{"cmd":%q}`, cmdSFSkip), ColorSecondary)},
	}}
	b.send(ctx, vkID, "🏬 Магазин распознан.\n\n"+
		"Следить за всеми товарами или только за частью? Пришли слово — оставлю карточки, "+
		"в названии которых оно есть (например «iphone 17»).\n\n"+
		"Или нажми «Без фильтра», чтобы следить за всей выдачей.", kb)
}

// handleSellerTextFilter — пользователь прислал слово-фильтр для витрины (FSM в
// режиме SellerURL): дописываем tb_q и идём к выбору триггера.
func (b *Bot) handleSellerTextFilter(ctx context.Context, vkID int64, user *domain.User, text string, fsm searchFSM) {
	b.clearSearchFSM(ctx, vkID)

	// Прислали новую ссылку вместо слова → начинаем флоу заново по ней.
	if _, err := b.registry.FindSearchByURL(text); err == nil {
		b.startSearchTrack(ctx, vkID, user, text)
		return
	}

	rawURL := fsm.SellerURL
	if t := strings.TrimSpace(text); t != "" {
		rawURL = domain.AppendTextFilter(rawURL, t)
	}
	b.proceedSearchTrack(ctx, vkID, user, rawURL)
}

// handleSellerSkipFilter — кнопка «Без фильтра»: подключить магазин целиком.
func (b *Bot) handleSellerSkipFilter(ctx context.Context, vkID int64, user *domain.User) {
	fsm, ok := b.getSearchFSM(ctx, vkID)
	if !ok || fsm.SellerURL == "" {
		return
	}
	b.clearSearchFSM(ctx, vkID)
	b.proceedSearchTrack(ctx, vkID, user, fsm.SellerURL)
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

// checkSellerCap — гейт размера витрины продавца. false (+ ответ юзеру), если
// магазин пуст или товаров больше CAP. Для обычных поиск-ссылок — no-op (true);
// сбой запроса размера не блокирует подключение.
func (b *Bot) checkSellerCap(ctx context.Context, vkID int64, kb *Keyboard, ss interface{}, rawURL string) bool {
	sz, ok := ss.(sellerSizer)
	if !ok {
		return true
	}
	total, err := sz.SellerTotal(ctx, rawURL)
	if err != nil {
		b.log.Error("vk: seller total", "err", err)
		return true
	}
	if total == 0 {
		b.send(ctx, vkID, "🏬 В этом магазине по такой ссылке нет товаров. Проверь ссылку или ослабь фильтры.", kb)
		return false
	}
	if limit := sz.MaxItems(); total > limit {
		b.send(ctx, vkID, fmt.Sprintf(
			"🏬 В выдаче %d товаров — это больше лимита (%d).\n\n"+
				"Сузь выбор фильтрами на сайте (категория, бренд, модель) и пришли ссылку снова. "+
				"Можно добавить в конец ссылки &tb_q=текст — оставлю только карточки с этим текстом в названии.",
			total, limit), kb)
		return false
	}
	return true
}

// ── Выбор типа триггера (кнопка strack) ───────────────────────────────────────

func (b *Bot) handleSearchTrigger(ctx context.Context, vkID int64, user *domain.User, p payloadData) {
	switch p.Kind {
	case "any":
		b.createSearchSub(ctx, vkID, user, p.ID, domain.TriggerAnyDrop, nil, nil)
	case "below":
		if err := b.setSearchFSM(ctx, vkID, searchFSM{QueryID: p.ID, Trigger: string(domain.TriggerBelowTarget)}); err != nil {
			b.send(ctx, vkID, "Не получилось начать ввод (нет связи с хранилищем). Попробуй «Любое снижение».", nil)
			return
		}
		b.send(ctx, vkID, "💰 Введи целевую цену в рублях (например 59990).\nУведомлю, когда найдётся товар дешевле.", nil)
	case "disc":
		if err := b.setSearchFSM(ctx, vkID, searchFSM{QueryID: p.ID, Trigger: string(domain.TriggerDiscountPct)}); err != nil {
			b.send(ctx, vkID, "Не получилось начать ввод (нет связи с хранилищем). Попробуй «Любое снижение».", nil)
			return
		}
		b.send(ctx, vkID, "％ Введи процент скидки от стартовой цены (1–99, например 20).", nil)
	}
}

// ── Ввод числа (порог/процент) ────────────────────────────────────────────────

func (b *Bot) handleSearchThreshold(ctx context.Context, vkID int64, user *domain.User, text string, fsm searchFSM) {
	switch domain.TriggerType(fsm.Trigger) {
	case domain.TriggerBelowTarget:
		price, err := domain.ParsePrice(text)
		if err != nil {
			b.send(ctx, vkID, "Нужно число — цена в рублях, например 59990. Любая кнопка внизу отменит ввод.", nil)
			return
		}
		b.clearSearchFSM(ctx, vkID)
		b.createSearchSub(ctx, vkID, user, fsm.QueryID, domain.TriggerBelowTarget, &price, nil)

	case domain.TriggerDiscountPct:
		pct, err := domain.ParsePct(text)
		if err != nil {
			b.send(ctx, vkID, "Нужно целое число от 1 до 99, например 20. Любая кнопка внизу отменит ввод.", nil)
			return
		}
		b.clearSearchFSM(ctx, vkID)
		b.createSearchSub(ctx, vkID, user, fsm.QueryID, domain.TriggerDiscountPct, nil, &pct)

	default:
		b.clearSearchFSM(ctx, vkID)
		b.send(ctx, vkID, "Что-то пошло не так, отправь поисковую ссылку заново.", menuKeyboard(user.TelegramID != 0))
	}
}

// createSearchSub — создать поиск-подписку и зафиксировать стартовые цены.
func (b *Bot) createSearchSub(ctx context.Context, vkID int64, user *domain.User, queryID int64, trigger domain.TriggerType, target *float64, pct *int16) {
	// Жёсткий guard на случай гонки/обхода фейл-фаста.
	plan := user.EffectivePlan(time.Now())
	if cnt, err := b.searchSubRepo.CountActiveByUserID(ctx, user.ID); err == nil && cnt >= plan.MaxSearch {
		b.send(ctx, vkID, b.searchLimitText(plan, cnt), menuKeyboard(user.TelegramID != 0))
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
		b.log.Error("vk: create search subscription", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}

	// Baseline по уже известным товарам выдачи; для нового запроса выдача пуста —
	// baseline заведёт планировщик при первом скрейпе.
	if _, err := b.searchSubRepo.BackfillBaselines(ctx, sub.ID, queryID); err != nil {
		b.log.Warn("vk: backfill baselines", "sub_id", sub.ID, "err", err)
	}

	// Мгновенная первая оценка below_target по сохранённой выдаче — паритет с TG.
	if b.searchResults != nil && b.searchEvents != nil {
		if q, err := b.searchQueryRepo.GetByID(ctx, queryID); err == nil {
			n, err := searchsub.SendInstantBelowTarget(ctx, b.searchResults, b.searchSubRepo, b.searchEvents, sub, q, user.TelegramID)
			if err != nil {
				b.log.Warn("vk: instant search eval", "sub_id", sub.ID, "err", err)
			} else if n > 0 {
				b.log.Info("vk: instant search hits queued", "sub_id", sub.ID, "items", n)
			}
		}
	}

	b.send(ctx, vkID, "✅ Готово! "+domain.TriggerDescription(trigger, target, pct)+
		"\n\nПроверяю выдачу регулярно и пришлю, когда товары подешевеют 🔔",
		menuKeyboard(user.TelegramID != 0))
}

// ── Список поиск-подписок ─────────────────────────────────────────────────────

func (b *Bot) handleListSearch(ctx context.Context, vkID int64, user *domain.User, prefix string) {
	b.showSearchList(ctx, vkID, user, prefix, 0)
}

// showSearchList — постраничный список поиск-подписок (как showProductList).
func (b *Bot) showSearchList(ctx context.Context, vkID int64, user *domain.User, prefix string, page int) {
	subs, err := b.searchSubRepo.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		b.log.Error("vk: get search subscriptions", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	if len(subs) == 0 {
		text := "📡 У тебя пока нет поиск-подписок.\n\nОтправь ссылку на поисковую выдачу Wildberries — буду следить за всей выдачей."
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
	b.send(ctx, vkID, sb.String(), &Keyboard{Inline: true, Buttons: rows})
}

func (b *Bot) handleUntrackSearch(ctx context.Context, vkID int64, user *domain.User, subID int64) {
	if subID == 0 {
		b.send(ctx, vkID, b.welcomeText(user), menuKeyboard(user.TelegramID != 0))
		return
	}
	// id — из payload кнопки; гасим только подписку этого юзера.
	if err := b.searchSubRepo.Deactivate(ctx, subID, user.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		b.log.Error("vk: deactivate search sub", "err", err)
		b.send(ctx, vkID, "Произошла ошибка, попробуй позже.", nil)
		return
	}
	b.handleListSearch(ctx, vkID, user, "✅ Поиск-подписка отменена.")
}

func (b *Bot) searchLimitText(plan domain.Plan, used int) string {
	if plan.MaxSearch == 0 {
		return "🔎 Поиск-подписки на тарифе " + plan.Title + " недоступны.\n\n" +
			"Они есть на тарифах Lite и выше — кнопка «Тарифы». А ещё можно попробовать бесплатный триал — кнопка «Триал»."
	}
	if plan.Name == "free" {
		metrics.SearchUpsellShown.WithLabelValues("vk").Inc()
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
