package main

import (
	"testing"
	"time"

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
