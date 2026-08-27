package domain

import (
	"fmt"
	"time"
)

// HumanizeAge — «сколько времени назад» по-русски: «только что», «12 минут
// назад», «3 часа назад», «2 дня назад». Нужен там, где показываем данные не
// первой свежести (последняя известная цена под отказом площадки) — без
// возраста такая цифра выдаёт себя за текущую.
//
// Точность намеренно грубая: пользователю важно «свежее это или вчерашнее», а
// не минуты. Отрицательный возраст (часы сервера разъехались) считаем нулевым.
func HumanizeAge(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return "только что"
	case d < time.Hour:
		n := int(d.Minutes())
		return fmt.Sprintf("%d %s назад", n, pluralRu(n, "минуту", "минуты", "минут"))
	case d < 24*time.Hour:
		n := int(d.Hours())
		return fmt.Sprintf("%d %s назад", n, pluralRu(n, "час", "часа", "часов"))
	default:
		n := int(d.Hours() / 24)
		return fmt.Sprintf("%d %s назад", n, pluralRu(n, "день", "дня", "дней"))
	}
}

// pluralRu — русская форма числительного: 1 час, 2 часа, 5 часов (с оговорками
// на 11–14 и на вторую цифру).
func pluralRu(n int, one, few, many string) string {
	if n < 0 {
		n = -n
	}
	if n%100 >= 11 && n%100 <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	default:
		return many
	}
}
