package telegram

import (
	"strings"
	"testing"
)

func mkItems(n int) []SearchAlertItem {
	items := make([]SearchAlertItem, n)
	for i := range items {
		items[i] = SearchAlertItem{
			Name:         "Товар",
			URL:          "https://market.yandex.ru/product/100",
			EffectiveRub: 2490,
			PrevRub:      2990,
		}
	}
	return items
}

func TestRenderSearchAlert_NoBudgetShowsAll(t *testing.T) {
	a := SearchAlert{QueryText: "кофеварка", TotalHits: 3, Items: mkItems(3)}
	got := renderSearchAlert(a, 0)
	// budget<=0 → показываем все, без «…и ещё».
	if strings.Contains(got, "…и ещё") {
		t.Errorf("без бюджета не должно быть обрезки:\n%s", got)
	}
	if n := strings.Count(got, "📉"); n != 3 {
		t.Errorf("позиций %d, ждали 3:\n%s", n, got)
	}
	if !strings.Contains(got, "подешевело <b>3</b> товаров") {
		t.Errorf("нет заголовка с числом:\n%s", got)
	}
}

func TestRenderSearchAlert_BudgetTruncatesWithTail(t *testing.T) {
	a := SearchAlert{QueryText: "кофеварка", TotalHits: 10, Items: mkItems(10)}
	got := renderSearchAlert(a, 200) // маленький бюджет → влезет пара позиций
	shown := strings.Count(got, "📉")
	if shown == 0 || shown >= 10 {
		t.Fatalf("ожидали частичный показ (1..9), показано %d:\n%s", shown, got)
	}
	if !strings.Contains(got, "…и ещё") {
		t.Errorf("нет хвоста «…и ещё» при обрезке:\n%s", got)
	}
}

func TestRenderSearchAlert_TopAlwaysShownEvenIfOverBudget(t *testing.T) {
	long := strings.Repeat("оченьдлинноеимя", 20) // заведомо длиннее любого бюджета
	a := SearchAlert{QueryText: "x", TotalHits: 2, Items: []SearchAlertItem{
		{Name: long, URL: "u", EffectiveRub: 100, PrevRub: 200},
		{Name: "второй", URL: "u2", EffectiveRub: 50, PrevRub: 90},
	}}
	got := renderSearchAlert(a, 50)
	// Даже при крошечном бюджете топ-позиция обязана присутствовать.
	if strings.Count(got, "📉") < 1 {
		t.Fatalf("топ-позиция должна показываться всегда:\n%s", got)
	}
	// И длинное имя в режиме бюджета обрезается многоточием.
	if !strings.Contains(got, "…") {
		t.Errorf("длинное имя должно обрезаться в бюджет-режиме:\n%s", got)
	}
}

func TestRenderSearchAlert_MoreThanShownHeader(t *testing.T) {
	a := SearchAlert{QueryText: "телефон", TotalHits: 50, Items: mkItems(10)}
	got := renderSearchAlert(a, 0)
	if !strings.Contains(got, "Нашлось больше") {
		t.Errorf("при TotalHits>len(Items) ждали «Нашлось больше»:\n%s", got)
	}
}

func TestSearchAlertKeyboard(t *testing.T) {
	if kb := searchAlertKeyboard(SearchAlert{SearchURL: ""}); kb != nil {
		t.Errorf("без SearchURL клавиатура должна быть nil, got %v", kb)
	}
	if kb := searchAlertKeyboard(SearchAlert{SearchURL: "https://x"}); kb == nil {
		t.Error("с SearchURL клавиатура не должна быть nil")
	}
}
