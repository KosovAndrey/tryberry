package domain

import (
	"strings"
	"testing"
	"time"
)

// Знакомый товар: под отказом площадки показываем цену из своей истории — но
// обязательно с возрастом, иначе она читается как текущая.
func TestBlockedFallbackTextKnownProduct(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	got := BlockedFallbackText("Wildberries", "Наушники <Sony> WH-1000XM5", 24990, now.Add(-3*time.Hour), now)

	for _, want := range []string{
		"Wildberries сейчас не отдаёт данные",
		"Наушники <Sony> WH-1000XM5",
		"24\u00a0990\u00a0₽", // разряды и ₽ через неразрывный пробел
		"3 часа назад",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("текст без %q:\n%s", want, got)
		}
	}
}

// Незнакомый товар: цены нет — отдаём только статус площадки, без выдуманных цифр.
func TestBlockedFallbackTextUnknownProduct(t *testing.T) {
	now := time.Now()
	got := BlockedFallbackText("Ozon", "", 0, time.Time{}, now)

	if !strings.Contains(got, "Ozon сейчас не отдаёт данные") {
		t.Errorf("нет статуса площадки:\n%s", got)
	}
	if strings.Contains(got, "₽") {
		t.Errorf("цена в тексте, хотя её не знаем:\n%s", got)
	}
}

// Цена есть, а метки времени нет — считаем, что показывать нечего: цифра без
// возраста как раз и есть то враньё, от которого текст защищает.
func TestBlockedFallbackTextPriceWithoutTimestamp(t *testing.T) {
	got := BlockedFallbackText("AliExpress", "Кабель", 990, time.Time{}, time.Now())
	if strings.Contains(got, "990") {
		t.Errorf("цена без времени попала в текст:\n%s", got)
	}
}
