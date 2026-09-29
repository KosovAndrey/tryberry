package main

import (
	"testing"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
)

var testNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func ptime(t time.Time) *time.Time { return &t }

// созданные позже = больший created_at; функции полагаются на порядок (user_id, created_at).
func at(i int64) time.Time { return testNow.Add(time.Duration(i) * time.Minute) }

func TestSelectProductPauses(t *testing.T) {
	// Лимиты берутся из domain.Plans: free.MaxProduct=5, pro.MaxProduct=100.
	past := testNow.Add(-time.Hour)  // план истёк → free, MaxProduct=5
	future := testNow.Add(time.Hour) // план действует → не трогаем

	var cands []postgres.ProductSubForReconcile
	// user 1: истёкший pro → free, 12 активных → гасим избыток сверх 5 (id 6..12).
	for i := int64(1); i <= 12; i++ {
		cands = append(cands, postgres.ProductSubForReconcile{
			ID: i, UserID: 1, TelegramID: 100, Plan: "pro", PlanExpiresAt: ptime(past), CreatedAt: at(i),
		})
	}
	// user 2: истёкший pro → free, 8 активных → гасим избыток сверх 5 (id 18, 19, 20).
	for i := int64(13); i <= 20; i++ {
		cands = append(cands, postgres.ProductSubForReconcile{
			ID: i, UserID: 2, TelegramID: 200, Plan: "pro", PlanExpiresAt: ptime(past), CreatedAt: at(i),
		})
	}
	// user 3: действующий pro (MaxProduct=100) — не трогаем.
	for i := int64(21); i <= 35; i++ {
		cands = append(cands, postgres.ProductSubForReconcile{
			ID: i, UserID: 3, TelegramID: 300, Plan: "pro", PlanExpiresAt: ptime(future), CreatedAt: at(i),
		})
	}

	pause, affected := selectProductPauses(cands, testNow)

	if want := []int64{6, 7, 8, 9, 10, 11, 12, 18, 19, 20}; !eq(pause, want) {
		t.Fatalf("pause = %v, want %v", pause, want)
	}
	// affected — users.id (не telegram_id): уведомление о паузе роутится по
	// notify_channel и должно доходить и до VK-only юзеров.
	if want := []int64{1, 2}; !eq(affected, want) {
		t.Fatalf("affected = %v, want %v", affected, want)
	}
}

func TestSelectProductRestores(t *testing.T) {
	past := testNow.Add(-time.Hour)
	future := testNow.Add(time.Hour)

	var cands []postgres.ProductSubForReconcile
	// user 1: вернул pro (действует), активных 10 → слотов 90 → вернём все 5 паузных.
	for i := int64(1); i <= 5; i++ {
		cands = append(cands, postgres.ProductSubForReconcile{
			ID: i, UserID: 1, Plan: "pro", PlanExpiresAt: ptime(future), ActiveCount: 10, CreatedAt: at(i),
		})
	}
	// user 2: pro действует, но активных уже 98 → слотов 2 → вернём только 2 самых старых (id 6, 7).
	for i := int64(6); i <= 10; i++ {
		cands = append(cands, postgres.ProductSubForReconcile{
			ID: i, UserID: 2, Plan: "pro", PlanExpiresAt: ptime(future), ActiveCount: 98, CreatedAt: at(i),
		})
	}
	// user 3: всё ещё истёкший (free), активных 10 → слотов 0 → не возвращаем.
	for i := int64(11); i <= 13; i++ {
		cands = append(cands, postgres.ProductSubForReconcile{
			ID: i, UserID: 3, Plan: "pro", PlanExpiresAt: ptime(past), ActiveCount: 10, CreatedAt: at(i),
		})
	}

	got := selectProductRestores(cands, testNow)
	if want := []int64{1, 2, 3, 4, 5, 6, 7}; !eq(got, want) {
		t.Fatalf("restore = %v, want %v", got, want)
	}
}

func TestSelectSearchRestores(t *testing.T) {
	past := testNow.Add(-time.Hour)
	future := testNow.Add(time.Hour)

	var cands []postgres.PausedSearchSub
	// Лимиты из domain.Plans: pro.MaxSearch=10, trial.MaxSearch=10, free.MaxSearch=1.
	// user 1: pro действует (MaxSearch=10), 4 паузных → вернём все 4.
	for i := int64(1); i <= 4; i++ {
		cands = append(cands, postgres.PausedSearchSub{ID: i, UserID: 1, Plan: "pro", PlanExpiresAt: ptime(future), CreatedAt: at(i)})
	}
	// user 2: trial действует (MaxSearch=10), 4 паузных → вернём все 4 (id 5..8).
	for i := int64(5); i <= 8; i++ {
		cands = append(cands, postgres.PausedSearchSub{ID: i, UserID: 2, Plan: "trial", PlanExpiresAt: ptime(future), CreatedAt: at(i)})
	}
	// user 3: истёкший (→free, MaxSearch=1), 2 паузных → возвращаем СТАРЕЙШИЙ (id 9):
	// фри-поиск как хук — после даунгрейда один поиск продолжает жить
	// (docs/TARIFF-FREE-SEARCH-LINK.md §1, «бонус-эффект free MaxSearch=1»).
	for i := int64(9); i <= 10; i++ {
		cands = append(cands, postgres.PausedSearchSub{ID: i, UserID: 3, Plan: "pro", PlanExpiresAt: ptime(past), CreatedAt: at(i)})
	}

	got := selectSearchRestores(cands, testNow)
	if want := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9}; !eq(got, want) {
		t.Fatalf("restore = %v, want %v", got, want)
	}
}

func TestSelectorsEmpty(t *testing.T) {
	if p, a := selectProductPauses(nil, testNow); p != nil || a != nil {
		t.Fatalf("empty pauses: got %v %v", p, a)
	}
	if r := selectProductRestores(nil, testNow); r != nil {
		t.Fatalf("empty product restores: got %v", r)
	}
	if r := selectSearchRestores(nil, testNow); r != nil {
		t.Fatalf("empty search restores: got %v", r)
	}
}

func eq(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Товар не в наличии в дайджест попадать не должен. Регрессия прода 25-08-2026:
// у распроданного товара price_history замирает, CurrentPrice остаётся ценой
// последнего живого скрейпа — а она и есть минимум истории, поэтому вердикт
// «минимум за всё время» держится вечно. Юзеру пришло три таких «лучших цены»,
// и все три товара были распроданы.
func TestPickDigestDealsSkipsOutOfStock(t *testing.T) {
	now := time.Now()
	longAgo := now.Add(-200 * 24 * time.Hour)
	// История, на которой текущая цена = минимум за всё время.
	stats := domain.PriceStats{
		Min30: 100, Median30: 200, Min90: 100, MinAll: 100,
		Seg30: 5, CountAll: 50, Since: longAgo, HasData: true,
	}
	statsOf := func(int64) (domain.PriceStats, error) { return stats, nil }

	inStock := &domain.Subscription{
		ProductID: 1, ProductName: "В наличии", ProductURL: "https://example.com/1",
		CurrentPrice: 100, ProductInStock: true,
	}
	sold := &domain.Subscription{
		ProductID: 2, ProductName: "Распродан", ProductURL: "https://example.com/2",
		CurrentPrice: 100, ProductInStock: false,
	}

	deals := pickDigestDeals([]*domain.Subscription{inStock, sold}, statsOf, now)
	if len(deals) != 1 {
		t.Fatalf("в дайджест попало %d товаров, want 1 (только тот, что в наличии): %+v", len(deals), deals)
	}
	if deals[0].name != "В наличии" {
		t.Errorf("в дайджесте %q, ожидался товар в наличии", deals[0].name)
	}

	// Контроль: тот же распроданный товар с наличием — попадает. Значит
	// отсекает именно флаг наличия, а не что-то ещё в условии.
	sold.ProductInStock = true
	if got := pickDigestDeals([]*domain.Subscription{sold}, statsOf, now); len(got) != 1 {
		t.Errorf("с наличием товар должен попадать в дайджест, got %d", len(got))
	}

	// Нулевая цена не проходит независимо от наличия.
	zero := &domain.Subscription{ProductID: 3, CurrentPrice: 0, ProductInStock: true}
	if got := pickDigestDeals([]*domain.Subscription{zero}, statsOf, now); len(got) != 0 {
		t.Errorf("товар без цены не должен попадать, got %d", len(got))
	}
}

func TestBackInStockFires(t *testing.T) {
	cases := []struct {
		name              string
		inStock, notified bool
		want              bool
	}{
		{"нет в наличии", false, false, false},
		{"появился, ещё не сообщали", true, false, true},
		{"в наличии, об этом появлении уже сообщили", true, true, false},
		{"пропал после уведомления", false, true, false},
	}
	for _, c := range cases {
		if got := backInStockFires(c.inStock, c.notified); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRearmBackInStock(t *testing.T) {
	bis := domain.TriggerBackInStock
	cases := []struct {
		name              string
		trigger           domain.TriggerType
		inStock, notified bool
		want              bool
	}{
		{"пропал после уведомления — перевзводим", bis, false, true, true},
		{"пропал, ещё не сообщали — нечего снимать", bis, false, false, false},
		{"в наличии — не трогаем", bis, true, true, false},
		{"ценовой триггер не перевзводим", domain.TriggerAnyDrop, false, true, false},
	}
	for _, c := range cases {
		sub := &domain.Subscription{TriggerType: c.trigger, Notified: c.notified}
		if got := rearmBackInStock(sub, c.inStock); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
