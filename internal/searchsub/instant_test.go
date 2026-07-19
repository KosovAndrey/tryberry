package searchsub

import (
	"context"
	"testing"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type fakeResults struct {
	items    []domain.SearchHitItem
	gotQuery int64
	gotMax   float64
}

func (f *fakeResults) ListHitItemsBelow(_ context.Context, queryID int64, maxPrice float64) ([]domain.SearchHitItem, error) {
	f.gotQuery, f.gotMax = queryID, maxPrice
	return f.items, nil
}

type fakeMarker struct{ marked []int64 }

func (f *fakeMarker) MarkEvaluated(_ context.Context, id int64) error {
	f.marked = append(f.marked, id)
	return nil
}

type fakeSink struct {
	sent []domain.SearchHitEvent
}

func (f *fakeSink) Send(_ context.Context, _ string, v any) error {
	f.sent = append(f.sent, v.(domain.SearchHitEvent))
	return nil
}

func target(v float64) *float64 { return &v }

func TestSendInstantBelowTarget_SendsHits(t *testing.T) {
	res := &fakeResults{items: []domain.SearchHitItem{
		{ProductID: 1, Name: "Соль", URL: "u1", ImageURL: "img1", EffectiveKopecks: 12300, PrevPriceKopecks: 12300},
		{ProductID: 2, Name: "Соль 2", URL: "u2", EffectiveKopecks: 18500, PrevPriceKopecks: 18500},
	}}
	mk := &fakeMarker{}
	sink := &fakeSink{}
	sub := &domain.SearchSubscription{ID: 42, UserID: 7, TriggerType: domain.TriggerBelowTarget, TargetPrice: target(2000)}
	q := &domain.SearchQuery{ID: 67, QueryText: "соль морская", NormalizedURL: "https://ozon.ru/search?text=соль"}

	n, err := SendInstantBelowTarget(context.Background(), res, mk, sink, sub, q, 1031)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v, ждали 2/nil", n, err)
	}
	if res.gotQuery != 67 || res.gotMax != 2000 {
		t.Errorf("выборка по query=%d max=%.0f, ждали 67/2000", res.gotQuery, res.gotMax)
	}
	if len(sink.sent) != 1 {
		t.Fatalf("событий %d, ждали 1", len(sink.sent))
	}
	ev := sink.sent[0]
	if ev.SubID != 42 || ev.UserID != 7 || ev.TelegramID != 1031 || len(ev.Items) != 2 {
		t.Errorf("событие собрано неверно: %+v", ev)
	}
	if ev.QueryText != "соль морская" || ev.TriggerType != "below_target" {
		t.Errorf("текст/триггер события: %+v", ev)
	}
	// hero-фото нотифаера берётся из Items[0].ImageURL — оно должно доехать.
	if ev.Items[0].ImageURL != "img1" {
		t.Errorf("ImageURL потерян: %+v", ev.Items[0])
	}
	if len(mk.marked) != 1 || mk.marked[0] != 42 {
		t.Errorf("MarkEvaluated: %v, ждали [42]", mk.marked)
	}
}

func TestSendInstantBelowTarget_SilentPaths(t *testing.T) {
	mk := &fakeMarker{}
	sink := &fakeSink{}
	q := &domain.SearchQuery{ID: 1}

	// Не below_target — молчим (any_drop при создании не имеет «снижения»).
	n, err := SendInstantBelowTarget(context.Background(), &fakeResults{}, mk, sink,
		&domain.SearchSubscription{ID: 1, TriggerType: domain.TriggerAnyDrop}, q, 0)
	if n != 0 || err != nil || len(sink.sent) != 0 {
		t.Errorf("any_drop: n=%d sent=%d", n, len(sink.sent))
	}

	// below_target, но подходящих товаров нет (или выдача пуста) — молчим,
	// первый скрейп оценит штатно.
	n, err = SendInstantBelowTarget(context.Background(), &fakeResults{}, mk, sink,
		&domain.SearchSubscription{ID: 2, TriggerType: domain.TriggerBelowTarget, TargetPrice: target(100)}, q, 0)
	if n != 0 || err != nil || len(sink.sent) != 0 {
		t.Errorf("пустая выдача: n=%d sent=%d", n, len(sink.sent))
	}
	if len(mk.marked) != 0 {
		t.Errorf("MarkEvaluated не должен зваться на тихих путях: %v", mk.marked)
	}
}
