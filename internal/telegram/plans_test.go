package telegram

import "testing"

// discountedRub формирует подпись цены со скидкой для кнопок карточки тарифа.
// Формат совпадает с экраном оплаты (KopecksToRubString → "NNN.NN").
func TestDiscountedRub(t *testing.T) {
	cases := []struct {
		fullRub int
		pct     int
		want    string
	}{
		{499, 20, "399.00"}, // 39920 коп → вниз до рубля (СБП не ест копейки)
		{1000, 50, "500.00"},
		{299, 0, "299.00"},  // без скидки
		{299, 100, "0.00"},  // 100% — кламп до нуля
		{349, 15, "296.00"}, // 29665 коп → 296.00
	}
	for _, c := range cases {
		if got := discountedRub(c.fullRub, c.pct); got != c.want {
			t.Errorf("discountedRub(%d, %d) = %q, want %q", c.fullRub, c.pct, got, c.want)
		}
	}
}
