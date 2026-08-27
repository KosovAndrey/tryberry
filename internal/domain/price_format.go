package domain

import (
	"fmt"
	"strings"
)

// nbsp — неразрывный пробел. И разряды, и знак рубля отделяем именно им: в
// мессенджерах строка переносится по обычному пробелу, и «75 000 ₽» рвётся
// посреди числа или отрывает ₽ на следующую строку.
const nbsp = " "

// FormatPrice — денежная строка для сообщений: «75 000 ₽». Копейки отбрасываем
// (цены маркетплейсов целые), разряды разделяем.
//
// Заведено взамен рассыпанного по каналам Sprintf("%.0f ₽"), который печатал
// «75000 ₽» — заметили при съёмке промо-ролика 02-08-2026.
func FormatPrice(v float64) string {
	return FormatRub(v) + nbsp + "₽"
}

// FormatRub — только число, без знака валюты (для строк, где ₽ уже стоит рядом
// или валюта подразумевается).
func FormatRub(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	digits := fmt.Sprintf("%.0f", v)

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteString(nbsp)
		}
		b.WriteRune(c)
	}
	return b.String()
}
