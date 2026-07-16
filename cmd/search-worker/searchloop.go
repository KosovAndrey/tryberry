// cmd/search-worker/searchloop.go
//
// Поиск-воркер: consumer топика поисковых задач (search-tasks или reseller-tasks).
// На каждую задачу (одна выдача) делает скрейп → апсерт товаров/результатов →
// baseline новым товарам → оценку триггеров по подпискам. На сработавшие триггеры
// НЕ шлёт в Telegram сам, а продюсит SearchHitEvent в топик search-events.
// Отправку и запись search_notifications (после успешной доставки) делает notifier.
//
// Вынесен из cmd/scraper в отдельный бинарь, чтобы перекуп-дорожку
// (reseller-tasks, отдельный пул токенов) можно было реплицировать независимо от
// товарного пути.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
	"gitlab.com/KosovAndrey/tryberrybot/internal/searchsub"
)

// evalSlack — допуск к интервалу оценки подписки: если шаг скрейпа примерно
// совпадает с интервалом тарифа, мелкий джиттер не должен «съедать» оценку.
const evalSlack = 5 * time.Second

type searchWorker struct {
	log      *slog.Logger
	registry *scraper.Registry
	queries  *postgres.SearchQueryRepo
	subs     *postgres.SearchSubscriptionRepo
	results  *postgres.SearchResultRepo
	notifs   *postgres.SearchNotificationRepo
	products *postgres.ProductRepo
	events   *kafka.Producer // топик search-events

	// defaultInterval — интервал оценки для тарифов без своего Interval (фолбэк).
	defaultInterval time.Duration

	// belowTargetCooldown — анти-спам: не слать below_target-уведомления подписке
	// чаще этого окна. Широкая выдача (особ. Ozon, ~8 ротирующихся позиций) иначе
	// сыплет новыми дешёвыми SKU каждый скрейп. 0 — троттлинг выключен.
	belowTargetCooldown time.Duration
}

// makeHandler — обработчик одной задачи из топика поисковых задач.
//
// При ошибке возвращаем nil (offset коммитится, задача дропается): следующий тик
// планировщика переотправит её по интервалу запроса. Так бережём WB и пул
// токенов — иначе at-least-once гонял бы тяжёлый скрейп в плотном цикле повторов.
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

// shouldEvaluate — пора ли оценивать подписку: ещё ни разу (lastEval==nil) или с
// прошлой оценки прошло не меньше интервала тарифа (с допуском evalSlack, чтобы
// джиттер шага скрейпа не «съедал» оценку, когда шаг ≈ интервалу). Это и
// обузживает перекуп-частоту для обычных подписчиков того же запроса.
func shouldEvaluate(lastEval *time.Time, interval time.Duration, now time.Time) bool {
	if lastEval == nil {
		return true
	}
	return now.Sub(*lastEval) >= interval-evalSlack
}

// entry — товар выдачи после апсерта (с product_id и эфф. ценой в копейках).
type entry struct {
	item scraper.SearchItem
	pid  int64
	eff  int64
}

// minPriceTopN — по скольким первым позициям выдачи считаем минимальную цену
// для детекта волатильности запроса.
const minPriceTopN = 10

// minTopNPrice — минимальная эффективная цена (в копейках) среди первых
// minPriceTopN позиций выдачи; ok=false, если нечего считать.
func minTopNPrice(entries []entry) (int64, bool) {
	var minEff int64
	found := false
	for _, e := range entries {
		if e.item.Position > minPriceTopN || e.eff <= 0 {
			continue
		}
		if !found || e.eff < minEff {
			minEff = e.eff
			found = true
		}
	}
	return minEff, found
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
	// Апсерт товаров и текущей выдачи — БАТЧАМИ (1 round-trip на products, 1 на
	// results) вместо N запросов на каждый item: на 500-item выдачах это резко режет
	// латентность и нагрузку на БД (особенно reseller 1-мин). results.UpsertBatch
	// ещё и подавляет no-op перезаписи (heartbeat для last_seen_at).
	// Маркетплейс — у СКРЕЙПЕРА ВЫДАЧИ (ss): он резолвится по тому же URL и тем же
	// FindSearchByURL, а бот при заведении запроса пишет ровно его же
	// (string(ss.Marketplace())) — один источник правды, разъехаться нечему.
	//
	// Здесь стояла константа "wildberries" → товары из ЛЮБОЙ выдачи (Я.Маркет,
	// Ozon, Ali) ложились с ярлыком WB: на проде так было помечено 3119 товаров,
	// у Ali — 96% каталога. Скрейп это переживал (registry.Scrape выбирает по URL,
	// а не по колонке), но юзеру ямаркетовская карточка показывалась WB.
	// Брать q.Marketplace НЕЛЬЗЯ (пробовали, 2026-07-16): q здесь собран вручную
	// из Kafka-задачи (см. makeHandler), а она маркетплейс не несёт — поле пустое.
	//
	// URL канонизируем тем же CanonicalProductURL, что и ботовый /track. Без этого
	// одна карточка заводит РАЗНЫЕ products из выдачи и из бота: products.url
	// UNIQUE = ключ товара, а выдача Ozon отдаёт ссылки со slug-ом (bot — как
	// пришлёт юзер). Канон обязан стоять и здесь, и в /track — иначе половина
	// товаров ключуется одним написанием, половина другим.
	mp := ss.Marketplace()
	canon := make([]string, len(set.Items))
	prodUpserts := make([]postgres.ProductUpsert, 0, len(set.Items))
	for i, it := range set.Items {
		canon[i] = scraper.CanonicalProductURL(mp, it.URL)
		prodUpserts = append(prodUpserts, postgres.ProductUpsert{
			URL: canon[i], Name: it.Name, ImageURL: it.ImageURL, Marketplace: string(mp),
		})
	}
	idByURL, err := w.products.UpsertBatch(ctx, prodUpserts)
	if err != nil {
		return fmt.Errorf("batch upsert products: %w", err)
	}

	entries := make([]entry, 0, len(set.Items))
	resultRows := make([]postgres.ResultUpsert, 0, len(set.Items))
	for i, it := range set.Items {
		pid, ok := idByURL[canon[i]]
		if !ok {
			continue // товар не апсертнулся (редкий сбой) — пропускаем
		}
		eff := it.EffectivePriceKopecks()
		resultRows = append(resultRows, postgres.ResultUpsert{
			ProductID: pid, Position: it.Position, Price: searchsub.Rubles(eff),
		})
		entries = append(entries, entry{item: it, pid: pid, eff: eff})
	}
	if err := w.results.UpsertBatch(ctx, q.ID, resultRows); err != nil {
		return fmt.Errorf("batch upsert results: %w", err)
	}

	// Волатильность выдачи для бэкоффа планировщика: «изменением» считаем только
	// смену МИНИМАЛЬНОЙ цены топ-N (состав/позиции ротируются, особенно у Ozon,
	// и дребезжали бы бэкоффом). UpdateMinPrice — no-op, если минимум не менялся.
	if minEff, ok := minTopNPrice(entries); ok {
		if err := w.queries.UpdateMinPrice(ctx, q.ID, searchsub.Rubles(minEff)); err != nil {
			w.log.Warn("update min price", "query_id", q.ID, "err", err)
		}
	}

	w.log.Info("query scraped",
		"query_id", q.ID, "text", q.QueryText,
		"items", len(entries), "pages", set.PagesRead, "total_found", set.TotalFound)

	// Триггеры по каждой активной подписке — с throttl'ом по интервалу тарифа.
	// Выдача шарится по normalized_url, поэтому к одному запросу могут относиться
	// и перекуп-подписки (оцениваем каждый скрейп), и обычные (не чаще их
	// интервала, хотя выдача физически скрейпится раз в минуту).
	subs, err := w.subs.GetActiveByQueryID(ctx, q.ID)
	if err != nil {
		return fmt.Errorf("active subs: %w", err)
	}
	now := time.Now()
	for _, sub := range subs {
		// Поиск-интервал плана (у free он отдельный, 6ч): free-подписчик запроса,
		// который делит выдачу с pro (скрейп каждые 15м), оценивается не чаще
		// СВОЕГО поиск-каданса.
		iv := domain.EffectivePlanFor(sub.OwnerPlan, sub.OwnerPlanExpiresAt, now).EffectiveSearchInterval(w.defaultInterval)
		if !shouldEvaluate(sub.LastEvaluatedAt, iv, now) {
			continue // оценивали недавно — не чаще интервала тарифа
		}
		if err := w.evaluateSubscription(ctx, q, sub, entries); err != nil {
			w.log.Error("evaluate subscription", "sub_id", sub.ID, "err", err)
			continue
		}
		if err := w.subs.MarkEvaluated(ctx, sub.ID); err != nil {
			w.log.Warn("mark evaluated", "sub_id", sub.ID, "err", err)
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

	// Троттлинг below_target: широкая ротирующаяся выдача (особенно Ozon, ~8
	// ротирующихся позиций) иначе сыплет новыми дешёвыми SKU каждый скрейп. Если
	// этой подписке слали недавно — пропускаем (хиты никуда не денутся, всплывут
	// после окна). any_drop/discount не троттлим — там событие = реальная просадка.
	if w.belowTargetCooldown > 0 && sub.TriggerType == domain.TriggerBelowTarget {
		if last, ok, err := w.notifs.GetLastNotifiedAt(ctx, sub.ID); err != nil {
			w.log.Warn("get last notified at", "sub_id", sub.ID, "err", err)
		} else if ok && time.Since(last) < w.belowTargetCooldown {
			w.log.Info("below_target throttled", "sub_id", sub.ID,
				"since", time.Since(last).Round(time.Minute).String(),
				"cooldown", w.belowTargetCooldown.String())
			return nil
		}
	}

	// Формируем событие. Отправку в Telegram и запись search_notifications
	// (только при успешной доставке) делает notifier — единое место, как в
	// товарном пути; заодно идёт через уже настроенный там HTTPS_PROXY.
	ev := domain.SearchHitEvent{
		SubID:       sub.ID,
		TelegramID:  sub.TelegramID,
		UserID:      sub.UserID,
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
			PrevPriceKopecks:      searchsub.RefKopecks(h),
			FeedbackPointsKopecks: e.item.FeedbackPointsKopecks(),
			EffectiveKopecks:      e.eff,
		})
	}

	key := strconv.FormatInt(sub.UserID, 10)
	if err := w.events.Send(ctx, key, ev); err != nil {
		// Событие не ушло — notifier ничего не запишет, значит на следующем
		// скрейпе те же хиты сработают снова и переотправятся. Самовосстановление.
		return fmt.Errorf("send search hit event: %w", err)
	}
	w.log.Info("search hits queued", "sub_id", sub.ID, "telegram_id", sub.TelegramID, "items", len(ev.Items))
	return nil
}
