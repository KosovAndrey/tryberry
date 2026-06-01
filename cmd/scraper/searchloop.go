package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
	"gitlab.com/KosovAndrey/tryberrybot/internal/searchsub"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
)

const wbMarketplace = "wildberries"

// searchLoop — конвейер поиск-подписок: скрейп выдач → апсерт товаров/результатов
// → baseline новым товарам → триггеры по подпискам → батч-уведомления.
//
// Живёт отдельным тикером в scraper-сервисе рядом с consumer'ом цен (выдач
// немного, конвейер выполняется синхронно по очереди запросов).
type searchLoop struct {
	log      *slog.Logger
	registry *scraper.Registry
	queries  *postgres.SearchQueryRepo
	subs     *postgres.SearchSubscriptionRepo
	results  *postgres.SearchResultRepo
	notifs   *postgres.SearchNotificationRepo
	products *postgres.ProductRepo
	notifier searchsub.SearchNotifier
}

func runSearchLoop(ctx context.Context, sl *searchLoop, interval time.Duration) {
	sl.log.Info("search loop started", "interval", interval.String())
	tick := func() {
		if err := sl.tick(ctx); err != nil {
			sl.log.Error("search tick failed", "err", err)
		}
	}
	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

func (sl *searchLoop) tick(ctx context.Context) error {
	queries, err := sl.queries.GetScrapable(ctx)
	if err != nil {
		return fmt.Errorf("get scrapable: %w", err)
	}
	if len(queries) == 0 {
		sl.log.Info("search tick: no active queries")
		return nil
	}
	sl.log.Info("search tick", "queries", len(queries))
	for _, q := range queries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := sl.scrapeQuery(ctx, q); err != nil {
			sl.log.Error("scrape query failed", "query_id", q.ID, "text", q.QueryText, "err", err)
		}
	}
	return nil
}

// entry — товар выдачи после апсерта (с product_id и эфф. ценой в копейках).
type entry struct {
	item scraper.SearchItem
	pid  int64
	eff  int64
}

func (sl *searchLoop) scrapeQuery(ctx context.Context, q *domain.SearchQuery) error {
	ss, err := sl.registry.FindSearchByURL(q.NormalizedURL)
	if err != nil {
		return fmt.Errorf("no search scraper: %w", err)
	}

	set, err := ss.ScrapeSearch(ctx, q.NormalizedURL)
	if err != nil {
		return fmt.Errorf("scrape: %w", err)
	}
	if err := sl.queries.UpdateLastScraped(ctx, q.ID); err != nil {
		sl.log.Warn("update last_scraped", "query_id", q.ID, "err", err)
	}
	if len(set.Items) == 0 {
		sl.log.Warn("empty search result", "query_id", q.ID, "pages", set.PagesRead)
		return nil
	}

	// Апсерт товаров и текущей выдачи.
	entries := make([]entry, 0, len(set.Items))
	for _, it := range set.Items {
		p, err := sl.products.Upsert(ctx, it.URL, it.Name, it.ImageURL, wbMarketplace)
		if err != nil {
			sl.log.Error("upsert product", "art", it.ArticleID, "err", err)
			continue
		}
		eff := it.EffectivePriceKopecks()
		if _, err := sl.results.Upsert(ctx, q.ID, p.ID, it.Position, searchsub.Rubles(eff)); err != nil {
			sl.log.Error("upsert result", "product_id", p.ID, "err", err)
			continue
		}
		entries = append(entries, entry{item: it, pid: p.ID, eff: eff})
	}

	sl.log.Info("query scraped",
		"query_id", q.ID, "text", q.QueryText,
		"items", len(entries), "pages", set.PagesRead, "total_found", set.TotalFound)

	// Триггеры по каждой активной подписке.
	subs, err := sl.subs.GetActiveByQueryID(ctx, q.ID)
	if err != nil {
		return fmt.Errorf("active subs: %w", err)
	}
	for _, sub := range subs {
		if err := sl.evaluateSubscription(ctx, q, sub, entries); err != nil {
			sl.log.Error("evaluate subscription", "sub_id", sub.ID, "err", err)
		}
	}
	return nil
}

func (sl *searchLoop) evaluateSubscription(ctx context.Context, q *domain.SearchQuery, sub *domain.SearchSubscription, entries []entry) error {
	rule := searchsub.RuleFromSubscription(sub)

	byID := make(map[int64]entry, len(entries))
	states := make([]searchsub.ProductState, 0, len(entries))

	for _, e := range entries {
		byID[e.pid] = e

		baseRub, ok, err := sl.subs.GetBaseline(ctx, sub.ID, e.pid)
		if err != nil {
			return fmt.Errorf("get baseline: %w", err)
		}
		if !ok {
			// Товар впервые виден этой подпиской — фиксируем стартовую
			// (эффективную) цену и используем её как baseline уже в этом цикле.
			// below_target (абсолютный порог) сработает сразу; any_drop и
			// discount_pct — нет, т.к. current == baseline (нужно реальное падение).
			if err := sl.subs.UpsertBaseline(ctx, sub.ID, e.pid, searchsub.Rubles(e.eff)); err != nil {
				sl.log.Error("upsert baseline", "sub_id", sub.ID, "product_id", e.pid, "err", err)
			}
			baseRub = searchsub.Rubles(e.eff)
		}

		lastRub, hasNotif, err := sl.notifs.GetLastNotifiedPrice(ctx, sub.ID, e.pid)
		if err != nil {
			return fmt.Errorf("get last notified: %w", err)
		}

		states = append(states, searchsub.ProductState{
			ProductID:           e.pid,
			CurrentKopecks:      e.eff,
			BaselineKopecks:     searchsub.Kopecks(baseRub),
			LastNotifiedKopecks: searchsub.Kopecks(lastRub),
			HasNotified:         hasNotif,
		})
	}

	hits := searchsub.Evaluate(rule, states)
	if len(hits) == 0 {
		return nil
	}

	n := searchsub.Notification{TelegramID: sub.TelegramID, QueryText: q.QueryText, SearchURL: q.NormalizedURL}
	for _, h := range hits {
		e := byID[h.ProductID]
		n.Items = append(n.Items, searchsub.NotifyItem{
			Name:                  e.item.Name,
			URL:                   e.item.URL,
			PriceKopecks:          e.item.PriceKopecks,
			OldPriceKopecks:       e.item.OldPriceKopecks,
			FeedbackPointsKopecks: e.item.FeedbackPointsKopecks(),
			EffectiveKopecks:      e.eff,
		})
	}

	// Сначала отправляем; запись в search_notifications — только при успехе,
	// иначе исказится baseline повторных срабатываний.
	if err := sl.notifier.Notify(ctx, n); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	for _, h := range hits {
		if err := sl.notifs.Insert(ctx, sub.ID, h.ProductID, searchsub.Rubles(h.CurrentKopecks)); err != nil {
			sl.log.Error("insert notification", "sub_id", sub.ID, "product_id", h.ProductID, "err", err)
		}
	}
	sl.log.Info("search notification sent", "sub_id", sub.ID, "telegram_id", sub.TelegramID, "items", len(n.Items))
	return nil
}

// logNotifier — временная заглушка отправки (до бота/нотифаера): пишет в лог,
// что отправил бы. Позволяет прогнать конвейер end-to-end без Telegram.
type logNotifier struct{ log *slog.Logger }

func (l logNotifier) Notify(_ context.Context, n searchsub.Notification) error {
	l.log.Info("SEARCH NOTIFY (заглушка, отправили бы в Telegram)",
		"telegram_id", n.TelegramID, "query", n.QueryText, "items", len(n.Items))
	for _, it := range n.Items {
		l.log.Info("  → товар",
			"name", it.Name,
			"price_rub", searchsub.Rubles(it.PriceKopecks),
			"old_rub", searchsub.Rubles(it.OldPriceKopecks),
			"points_rub", searchsub.Rubles(it.FeedbackPointsKopecks),
			"effective_rub", searchsub.Rubles(it.EffectiveKopecks),
			"url", it.URL)
	}
	return nil
}

// tgSearchNotifier — реальная отправка поиск-уведомлений в Telegram.
// Сортирует подешевевшие товары по размеру скидки и шлёт топ-N одним
// сообщением; остаток отражается числом «нашлось больше».
type tgSearchNotifier struct {
	n    *telegram.Notifier
	topN int
}

func (t tgSearchNotifier) Notify(ctx context.Context, n searchsub.Notification) error {
	items := make([]searchsub.NotifyItem, len(n.Items))
	copy(items, n.Items)

	// Больше скидка (old − effective) → выше в списке.
	sort.SliceStable(items, func(i, j int) bool {
		return (items[i].OldPriceKopecks - items[i].EffectiveKopecks) >
			(items[j].OldPriceKopecks - items[j].EffectiveKopecks)
	})

	total := len(items)
	top := t.topN
	if top <= 0 {
		top = 10
	}
	if len(items) > top {
		items = items[:top]
	}

	alert := telegram.SearchAlert{
		ChatID:    n.TelegramID,
		QueryText: n.QueryText,
		SearchURL: n.SearchURL,
		TotalHits: total,
	}
	for _, it := range items {
		alert.Items = append(alert.Items, telegram.SearchAlertItem{
			Name:         it.Name,
			URL:          it.URL,
			EffectiveRub: searchsub.Rubles(it.EffectiveKopecks),
			OldRub:       searchsub.Rubles(it.OldPriceKopecks),
			PointsRub:    searchsub.Rubles(it.FeedbackPointsKopecks),
		})
	}
	return t.n.SendSearchAlert(ctx, alert)
}
