package telegram

import (
	"strings"
	"testing"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

func ptrInt64(v int64) *int64 { return &v }

func TestTGIdentityCount(t *testing.T) {
	cases := []struct {
		name string
		u    domain.User
		want int
	}{
		{"только TG", domain.User{TelegramID: 1}, 1},
		{"TG+VK", domain.User{TelegramID: 1, VKID: ptrInt64(2)}, 2},
		{"TG+MAX", domain.User{TelegramID: 1, MaxID: ptrInt64(3)}, 2},
		{"все три", domain.User{TelegramID: 1, VKID: ptrInt64(2), MaxID: ptrInt64(3)}, 3},
	}
	for _, c := range cases {
		if got := tgIdentityCount(&c.u); got != c.want {
			t.Errorf("%s: tgIdentityCount = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestTGNotifyCycle(t *testing.T) {
	cases := []struct {
		name string
		u    domain.User
		want string // через запятую
	}{
		{"только TG", domain.User{TelegramID: 1}, "tg,all"},
		{"TG+VK", domain.User{TelegramID: 1, VKID: ptrInt64(2)}, "tg,vk,all"},
		{"TG+MAX", domain.User{TelegramID: 1, MaxID: ptrInt64(3)}, "tg,max,all"},
		{"все три", domain.User{TelegramID: 1, VKID: ptrInt64(2), MaxID: ptrInt64(3)}, "tg,vk,max,all"},
	}
	for _, c := range cases {
		if got := strings.Join(tgNotifyCycle(&c.u), ","); got != c.want {
			t.Errorf("%s: tgNotifyCycle = %q, want %q", c.name, got, c.want)
		}
	}
}
