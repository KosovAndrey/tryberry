package main

import (
	"errors"
	"fmt"
	"testing"

	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
	"gitlab.com/KosovAndrey/tryberrybot/internal/vk"
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
		{"vk 901: запретил сообщения", fmt.Errorf("%w: vk messages.send: api error 901", vk.ErrRecipientGone), "rejected"},
		{"vk 5: битый токен", errors.New("vk messages.send: api error 5: User authorization failed"), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusLabel(tc.err); got != tc.want {
				t.Errorf("statusLabel = %q, ждали %q", got, tc.want)
			}
		})
	}
}

// Перманентный отказ всех упавших каналов не ретраим; любой транзиентный —
// ретраим (ретрай не задублирует доставленное: сюда приходим, только если
// ничего не доставлено).
func TestNoRetry(t *testing.T) {
	tgGone := fmt.Errorf("%w: %w: blocked", telegram.ErrTelegramPermanent, telegram.ErrTelegramRecipientGone)
	vkGone := fmt.Errorf("%w: api error 901", vk.ErrRecipientGone)
	net := errors.New("timeout")
	cases := []struct {
		name       string
		tg, vk, mx error
		want       bool
	}{
		{"только vk 901", nil, vkGone, nil, true},
		{"tg бан + vk 901", tgGone, vkGone, nil, true},
		{"vk 901 + max сеть", nil, vkGone, net, false},
		{"vk сеть", nil, net, nil, false},
		{"ошибок нет", nil, nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := noRetry(tc.tg, tc.vk, tc.mx); got != tc.want {
				t.Errorf("noRetry = %v, ждали %v", got, tc.want)
			}
		})
	}
}
