// cmd/scraper/searchloop.go
//
// Поиск-воркер: consumer топика search-tasks. На каждую задачу (одна выдача)
// делает скрейп → апсерт товаров/результатов → baseline новым товарам →
// оценку триггеров по подпискам. На сработавшие триггеры НЕ шлёт в Telegram сам,
// а продюсит SearchHitEvent в топик search-events. Отправку и запись
// search_notifications (после успешной доставки) делает notifier — как в
// товарном пути. Раньше тут жил in-process тикер runSearchLoop; теперь
// планирование вынесено в scheduler (топик search-tasks), а воркер
// реплицируется как consumer group.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
	"gitlab.com/KosovAndrey/tryberrybot/internal/searchsub"
)

const wbMarketplace = "wildberries"

type searchWorker struct {
	log      *slog.Logger
	registry *scraper.Registry
	queries  *postgres.SearchQueryRepo
	subs     *postgres.SearchSubscriptionRepo
	results  *postgres.SearchResultRepo
	notifs   *postgres.SearchNotificationRepo
	products *postgres.ProductRepo
	events   *kafka.Producer // топик search-events
}

// makeHandler — обработчик одной задачи из search-tasks.
//
// При ошибке возвращаем nil (offset коммитится, задача дропается): следующий
// тик планировщика (~20 мин) переотправит её. Так бережём WB и пул токенов —
// иначе at-least-once гонял бы тяжёлый скрейп в плотном цикле повторов.
func (w *searchWorker) makeHandler() kafka.HandlerFunc {
	return func(ctx context.Context, msg kafka.Message) error {
		task, err := kafka.Decode[domain.SearchTask](msg)
		if err != nil {
			w.log.Error("decode search task", "err", err)
			return nil
		}
		// scrapeQuery нужны только ID/URL/QueryText — собираем частичный SearchQuery.
		q := &domain.SearchQuery{
			ID:            task.QueryID,
			NormalizedURL: task.URL,
			QueryText:     task.QueryText,
		}
		if err := w.scrapeQuery(ctx, q); err != nil {
			w.log.Error("scrape query failed", "query_id", q.ID, "text", q.QueryText, "err", err)
		}
		return nil
	}
}

// entry — товар выдачи после апсерта (с product_id и эфф. ценой в копейках).
type entry struct {
	item scraper.SearchItem
	pid  int64
	eff  int64
}

func (w *searchWorker) scrapeQuery(ctx context.Context, q *domain.SearchQuery) error {
	ss, err := w.registry.FindSearchByURL(q.NormalizedURL)
	if err != nil {
		return fmt.Errorf("no search scraper: %w", err)
	}

	set, err := ss.ScrapeSearch(ctx, q.NormalizedURL)
	if err != nil {
		metrics.SearchScrapes.WithLabelValues("error").Inc()
		return fmt.Errorf("scrape: %w", err)
	}
	if err := w.queries.UpdateLastScraped(ctx, q.ID); err != nil {
		w.log.Warn("update last_scraped", "query_id", q.ID, "err", err)
	}
	if len(set.Items) == 0 {
		metrics.SearchScrapes.WithLabelValues("empty").Inc()
		w.log.Warn("empty search result", "query_id", q.ID, "pages", set.PagesRead)
		return nil
	}
	metrics.SearchScrapes.WithLabelValues("success").Inc()

	// Апсерт товаров и текущей выдачи.
	entries := make([]entry, 0, len(set.Items))
	for _, it := range set.Items {
		p, err := w.products.Upsert(ctx, it.URL, it.Name, it.ImageURL, wbMarketplace)
		if err != nil {
			w.log.Error("upsert product", "art", it.ArticleID, "err", err)
			continue
		}
		eff := it.EffectivePriceKopecks()
		if _, err := w.results.Upsert(ctx, q.ID, p.ID, it.Position, searchsub.Rubles(eff)); err != nil {
			w.log.Error("upsert result", "product_id", p.ID, "err", err)
			continue
		}
		entries = append(entries, entry{item: it, pid: p.ID, eff: eff})
	}

	w.log.Info("query scraped",
		"query_id", q.ID, "text", q.QueryText,
		"items", len(entries), "pages", set.PagesRead, "total_found", set.TotalFound)

	// Триггеры по каждой активной подписке.
	subs, err := w.subs.GetActiveByQueryID(ctx, q.ID)
	if err != nil {
		return fmt.Errorf("active subs: %w", err)
	}
	for _, sub := range subs {
		if err := w.evaluateSubscription(ctx, q, sub, entries); err != nil {
			w.log.Error("evaluate subscription", "sub_id", sub.ID, "err", err)
		}
	}
	return nil
}

func (w *searchWorker) evaluateSubscription(ctx context.Context, q *domain.SearchQuery, sub *domain.SearchSubscription, entries []entry) error {
	rule := searchsub.RuleFromSubscription(sub)

	byID := make(map[int64]entry, len(entries))
	states := make([]searchsub.ProductState, 0, len(entries))

	for _, e := range entries {
		byID[e.pid] = e

		baseRub, ok, err := w.subs.GetBaseline(ctx, sub.ID, e.pid)
		if err != nil {
			return fmt.Errorf("get baseline: %w", err)
		}
		if !ok {
			// Товар впервые виден этой подпиской — фиксируем стартовую (эфф.)
			// цену как baseline и используем её уже в этом цикле. below_target
			// сработает сразу; any_drop и discount_pct — нет (current == baseline).
			if err := w.subs.UpsertBaseline(ctx, sub.ID, e.pid, searchsub.Rubles(e.eff)); err != nil {
				w.log.Error("upsert baseline", "sub_id", sub.ID, "product_id", e.pid, "err", err)
			}
			baseRub = searchsub.Rubles(e.eff)
		}

		lastRub, hasNotif, err := w.notifs.GetLastNotifiedPrice(ctx, sub.ID, e.pid)
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

	// Формируем событие. Отправку в Telegram и запись search_notifications
	// (только при успешной доставке) делает notifier — единое место, как в
	// товарном пути; заодно идёт через уже настроенный там HTTPS_PROXY.
	ev := domain.SearchHitEvent{
		SubID:       sub.ID,
		TelegramID:  sub.TelegramID,
		QueryText:   q.QueryText,
		SearchURL:   q.NormalizedURL,
		TriggerType: string(sub.TriggerType),
	}
	for _, h := range hits {
		e := byID[h.ProductID]
		ev.Items = append(ev.Items, domain.SearchHitItem{
			ProductID:             e.pid,
			Name:                  e.item.Name,
			URL:                   e.item.URL,
			PriceKopecks:          e.item.PriceKopecks,
			OldPriceKopecks:       e.item.OldPriceKopecks,
			FeedbackPointsKopecks: e.item.FeedbackPointsKopecks(),
			EffectiveKopecks:      e.eff,
		})
	}

	key := strconv.FormatInt(sub.TelegramID, 10)
	if err := w.events.Send(ctx, key, ev); err != nil {
		// Событие не ушло — notifier ничего не запишет, значит на следующем
		// скрейпе те же хиты сработают снова и переотправятся. Самовосстановление.
		return fmt.Errorf("send search hit event: %w", err)
	}
	w.log.Info("search hits queued", "sub_id", sub.ID, "telegram_id", sub.TelegramID, "items", len(ev.Items))
	return nil
}
