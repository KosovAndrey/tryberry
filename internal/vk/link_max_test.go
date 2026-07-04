package vk

import (
	"encoding/json"
	"strings"
	"testing"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

func ptrInt64(v int64) *int64 { return &v }

func TestVKIdentityCount(t *testing.T) {
	cases := []struct {
		name string
		u    domain.User
		want int
	}{
		{"только VK", domain.User{VKID: ptrInt64(1)}, 1},
		{"VK+TG", domain.User{VKID: ptrInt64(1), TelegramID: 2}, 2},
		{"VK+MAX", domain.User{VKID: ptrInt64(1), MaxID: ptrInt64(3)}, 2},
		{"все три", domain.User{VKID: ptrInt64(1), TelegramID: 2, MaxID: ptrInt64(3)}, 3},
	}
	for _, c := range cases {
		if got := vkIdentityCount(&c.u); got != c.want {
			t.Errorf("%s: vkIdentityCount = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestVKNotifyCycle(t *testing.T) {
	cases := []struct {
		name string
		u    domain.User
		want string
	}{
		{"только VK", domain.User{VKID: ptrInt64(1)}, "vk,all"},
		{"VK+TG", domain.User{VKID: ptrInt64(1), TelegramID: 2}, "vk,tg,all"},
		{"VK+MAX", domain.User{VKID: ptrInt64(1), MaxID: ptrInt64(3)}, "vk,max,all"},
		{"все три", domain.User{VKID: ptrInt64(1), TelegramID: 2, MaxID: ptrInt64(3)}, "vk,tg,max,all"},
	}
	for _, c := range cases {
		if got := strings.Join(vkNotifyCycle(&c.u), ","); got != c.want {
			t.Errorf("%s: vkNotifyCycle = %q, want %q", c.name, got, c.want)
		}
	}
}

// Разбор ref из message_new: поле кладёт VK при переходе по vk.me/...?ref=…
// (кнопка «Привязать VK» в TG передаёт в нём link_<код>).
func TestMessageNewParsesRef(t *testing.T) {
	raw := `{"message":{"from_id":42,"peer_id":42,"text":"Начать","payload":"","ref":"link_ABCD2345"}}`
	var m messageNew
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Message.Ref != "link_ABCD2345" {
		t.Errorf("Ref = %q, want %q", m.Message.Ref, "link_ABCD2345")
	}
}
