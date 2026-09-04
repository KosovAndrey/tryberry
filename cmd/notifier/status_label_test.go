package main

import (
	"errors"
	"fmt"
	"testing"

	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
)

// Метка исхода решает, разбудит ли отправка critical NotificationChannelFailing:
// правило считает только status="error". Отказ получателя туда попадать не должен.
func TestStatusLabel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"доставлено", nil, "ok"},
		{"заблокировал бота", fmt.Errorf("%w: %w: blocked", telegram.ErrTelegramPermanent, telegram.ErrTelegramRecipientGone), "rejected"},
		{"битая картинка", fmt.Errorf("%w: wrong type of the web page content", telegram.ErrTelegramPermanent), "error"},
		{"сеть", errors.New("http do: context deadline exceeded"), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusLabel(tc.err); got != tc.want {
				t.Errorf("statusLabel = %q, ждали %q", got, tc.want)
			}
		})
	}
}
