package telegram

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

func TestChartURL(t *testing.T) {
	cases := []struct {
		base, public, want string
	}{
		{"https://tryberry.ru", "abc123def456", "https://tryberry.ru/p/abc123def456"},
		{"", "abc123def456", ""},        // сайт не сконфигурирован
		{"https://tryberry.ru", "", ""}, // нет public_id
	}
	for _, c := range cases {
		b := &Bot{chartBaseURL: c.base}
		if got := b.chartURL(c.public); got != c.want {
			t.Errorf("chartURL(base=%q, public=%q) = %q, want %q", c.base, c.public, got, c.want)
		}
	}
}

func TestChartButtonRow(t *testing.T) {
	if chartButtonRow("") != nil {
		t.Error("пустой URL → строки быть не должно (иначе Telegram отклонит клавиатуру)")
	}
	row := chartButtonRow("https://tryberry.ru/p/abc")
	if len(row) != 1 {
		t.Fatalf("ожидали 1 кнопку, получили %d", len(row))
	}
	if row[0].URL == nil || *row[0].URL != "https://tryberry.ru/p/abc" {
		t.Errorf("URL кнопки = %v, want https://tryberry.ru/p/abc", row[0].URL)
	}
}

// Кнопка графика появляется в клавиатуре трека только при заданном URL.
func TestTrackKeyboardIncludesChart(t *testing.T) {
	withURL := trackTriggerKeyboard(7, domain.TriggerAnyDrop, "https://tryberry.ru/p/abc")
	if !hasURLButton(withURL, "https://tryberry.ru/p/abc") {
		t.Error("клавиатура с URL должна содержать кнопку графика")
	}
	without := trackTriggerKeyboard(7, domain.TriggerAnyDrop, "")
	if hasURLButton(without, "https://tryberry.ru/p/abc") {
		t.Error("без URL кнопки графика быть не должно")
	}
}

func TestPriceAlertKeyboard_Chart(t *testing.T) {
	// С настроенным сайтом и public_id — первой строкой кнопка графика.
	n := NewNotifier("token", "https://tryberry.ru")
	rows := alertRows(n.priceAlertKeyboard(PriceAlert{SubscriptionID: 5, PublicID: "abc123def456"}))
	if len(rows) != 2 {
		t.Fatalf("ожидали 2 строки (график + keep/untrack), получили %d", len(rows))
	}
	if rows[0][0]["url"] != "https://tryberry.ru/p/abc123def456" || rows[0][0]["text"] != "📈 График цены" {
		t.Errorf("первая строка должна быть кнопкой графика, got %v", rows[0][0])
	}

	// Без сайта — только keep/untrack, никакой битой кнопки.
	n2 := NewNotifier("token", "")
	rows2 := alertRows(n2.priceAlertKeyboard(PriceAlert{SubscriptionID: 5, PublicID: "abc123def456"}))
	if len(rows2) != 1 {
		t.Errorf("без сайта ожидали 1 строку, получили %d", len(rows2))
	}
}

func alertRows(kb map[string]any) [][]map[string]any {
	return kb["inline_keyboard"].([][]map[string]any)
}

func hasURLButton(kb tgbotapi.InlineKeyboardMarkup, url string) bool {
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			if btn.URL != nil && *btn.URL == url {
				return true
			}
		}
	}
	return false
}
