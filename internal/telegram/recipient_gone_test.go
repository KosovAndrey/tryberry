package telegram

import (
	"errors"
	"net/http"
	"testing"
)

// Отказ ПОЛУЧАТЕЛЯ (403 «bot was blocked», «chat not found») обязан отличаться
// от прочих перманентных 4xx: на нём метрика ставит rejected, а не error, и
// алерт «доставка в канал полностью падает» его не считает. Иначе один
// заблокировавший бота юзер в тихое окно поднимает critical при живом канале
// (ложная тревога 04-09-2026).
func TestClassifyError(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		description   string
		wantPermanent bool
		wantGone      bool
	}{
		{"заблокировал бота", http.StatusForbidden, "Forbidden: bot was blocked by the user", true, true},
		{"выкинут из чата", http.StatusForbidden, "Forbidden: bot was kicked from the group chat", true, true},
		{"чат не найден", http.StatusBadRequest, "Bad Request: chat not found", true, true},
		{"аккаунт удалён", http.StatusForbidden, "Forbidden: user is deactivated", true, true},
		{"битая картинка", http.StatusBadRequest, "Bad Request: wrong type of the web page content", true, false},
		{"битый токен", http.StatusUnauthorized, "Unauthorized", true, false},
		{"троттл", http.StatusTooManyRequests, "Too Many Requests: retry after 5", false, false},
		{"телега легла", http.StatusBadGateway, "Bad Gateway", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError(tc.status, tc.description)
			if err == nil {
				t.Fatal("ждали ошибку, получили nil")
			}
			if got := errors.Is(err, ErrTelegramPermanent); got != tc.wantPermanent {
				t.Errorf("перманентная = %v, ждали %v (err: %v)", got, tc.wantPermanent, err)
			}
			if got := errors.Is(err, ErrTelegramRecipientGone); got != tc.wantGone {
				t.Errorf("отказ получателя = %v, ждали %v (err: %v)", got, tc.wantGone, err)
			}
		})
	}
}
