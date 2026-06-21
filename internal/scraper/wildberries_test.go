package scraper

import "testing"

func TestBasketCandidates(t *testing.T) {
	// Кандидат первым, затем ближайшие соседи (±1, ±2 …) — для vol8943 формула даёт
	// 40, а реальный шард 39, он должен пробоваться сразу после кандидата.
	got := basketCandidates(40, 4)
	want := []int64{40, 39, 41, 38, 42, 37, 43, 36, 44}
	if len(got) != len(want) {
		t.Fatalf("basketCandidates(40,4) len=%d %v; want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("basketCandidates(40,4)=%v; want %v", got, want)
		}
	}
	// Номера <1 отбрасываются (без 0 и отрицательных).
	for _, b := range basketCandidates(2, 4) {
		if b < 1 {
			t.Errorf("basketCandidates(2,4) дал номер <1: %v", basketCandidates(2, 4))
		}
	}
}
