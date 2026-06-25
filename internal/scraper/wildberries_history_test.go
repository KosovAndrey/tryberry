package scraper

import (
	"os"
	"testing"
	"time"
)

// Реальный фикстур с basket-CDN WB (товар 252334498): 12 точек, недельный каданс,
// 2026-04-05..2026-06-21, цены в копейках (price.RUB).
func loadWBFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/wb_price_history.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func TestParseWBHistory_RealFixture(t *testing.T) {
	body := loadWBFixture(t)
	now := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC) // в пределах 180д от всех точек

	price, points, err := parseWBHistory(body, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Текущая цена = последняя точка (35542 коп → 355.42 ₽).
	if price != 355.42 {
		t.Errorf("price = %v, want 355.42", price)
	}
	// 12 точек всего, последняя исключена → 11 в History.
	if len(points) != 11 {
		t.Fatalf("history points = %d, want 11", len(points))
	}
	// Отсортированы по возрастанию, цены ненулевые, копейки сохранены.
	for i, p := range points {
		if p.Price <= 0 {
			t.Errorf("point %d: non-positive price %v", i, p.Price)
		}
		if i > 0 && p.At.Before(points[i-1].At) {
			t.Errorf("points not sorted at %d", i)
		}
	}
	// Первая историческая точка — 2026-04-05, 619.07 ₽ (61907 коп).
	if points[0].Price != 619.07 {
		t.Errorf("first point price = %v, want 619.07 (kopecks preserved)", points[0].Price)
	}
	if got := points[0].At.UTC().Format("2006-01-02"); got != "2026-04-05" {
		t.Errorf("first point date = %s, want 2026-04-05", got)
	}
}

// Окно отсекает старые точки: если now далеко в будущем, в History не попадёт
// ничего (всё старше 180д), но текущая цена всё равно вернётся.
func TestParseWBHistory_AgeCutoff(t *testing.T) {
	body := loadWBFixture(t)
	now := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)

	price, points, err := parseWBHistory(body, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if price != 355.42 {
		t.Errorf("price = %v, want 355.42", price)
	}
	if len(points) != 0 {
		t.Errorf("history points = %d, want 0 (all older than cutoff)", len(points))
	}
}

func TestParseWBHistory_Errors(t *testing.T) {
	if _, _, err := parseWBHistory([]byte(`[]`), time.Now()); err == nil {
		t.Error("empty array must error")
	}
	if _, _, err := parseWBHistory([]byte(`[{"dt":1,"price":{"RUB":0}}]`), time.Now()); err == nil {
		t.Error("zero current price must error")
	}
	if _, _, err := parseWBHistory([]byte(`not json`), time.Now()); err == nil {
		t.Error("bad json must error")
	}
}
