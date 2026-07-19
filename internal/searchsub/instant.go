// instant.go — мгновенная первая оценка below_target при создании подписки.
//
// Зачем: оценка триггеров живёт в цикле скрейпа, поэтому подписка, созданная
// МЕЖДУ скрейпами, ждала следующего (Ozon-пол 15м, free — 6ч). Живой пример
// 2026-07-19: подписка создана через 2 секунды ПОСЛЕ скрейпа — юзер ждал 30
// минут при 67 подходящих товарах, уже лежавших в БД, и успел дважды пересоздать
// подписку, думая что она сломана. Здесь оцениваем по СОХРАНЁННОЙ выдаче сразу.
//
// Только below_target: any_drop/discount_pct при создании молчат по семантике —
// baseline только что зафиксирован, «снижения от него» ещё нет.
//
// Зависимости приходят узкими портами (пакет по-прежнему без прямых зависимостей
// от БД/сети): событие уходит в тот же топик search-events, дальше штатный
// нотифаер — рендер (hero-фото), доставка по каналу юзера, запись
// search_notifications. Анти-дубль с параллельным скрейпом двухслойный:
// MarkEvaluated (скрейп-оценка уважает интервал тарифа) + 6ч-кулдаун
// below_target после первого уведомления.
package searchsub

import (
	"context"
	"strconv"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// InstantResults — сохранённая выдача, обогащённая карточками товаров.
type InstantResults interface {
	ListHitItemsBelow(ctx context.Context, queryID int64, maxPrice float64) ([]domain.SearchHitItem, error)
}

// InstantMarker — отметка «подписка оценена» (дедуп с параллельным скрейпом).
type InstantMarker interface {
	MarkEvaluated(ctx context.Context, id int64) error
}

// EventSink — продюсер топика search-events.
type EventSink interface {
	Send(ctx context.Context, key string, value any) error
}

// SendInstantBelowTarget — если подписка below_target и в сохранённой выдаче
// есть товары ≤ target, шлёт SearchHitEvent немедленно. Возвращает число хитов
// (0 — не below_target / выдача пуста / подходящих нет; для нового запроса без
// выдачи первый скрейп оценит подписку штатно).
func SendInstantBelowTarget(
	ctx context.Context,
	results InstantResults,
	marker InstantMarker,
	events EventSink,
	sub *domain.SearchSubscription,
	q *domain.SearchQuery,
	telegramID int64,
) (int, error) {
	if sub.TriggerType != domain.TriggerBelowTarget || sub.TargetPrice == nil {
		return 0, nil
	}
	items, err := results.ListHitItemsBelow(ctx, q.ID, *sub.TargetPrice)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}

	ev := domain.SearchHitEvent{
		SubID:       sub.ID,
		TelegramID:  telegramID,
		UserID:      sub.UserID,
		QueryText:   q.QueryText,
		SearchURL:   q.NormalizedURL,
		TriggerType: string(sub.TriggerType),
		Items:       items,
	}
	if err := events.Send(ctx, strconv.FormatInt(sub.UserID, 10), ev); err != nil {
		return 0, err
	}
	// После успешной отправки: скрейп-оценка, идущая прямо сейчас, пропустит
	// подписку по интервалу тарифа. Ошибка не фатальна — второй слой защиты
	// (6ч-кулдаун below_target) дубль всё равно погасит.
	_ = marker.MarkEvaluated(ctx, sub.ID)
	return len(items), nil
}
