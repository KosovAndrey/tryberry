package domain

import "testing"

func TestResolveNotifyTargets(t *testing.T) {
	cases := []struct {
		name           string
		channel        string
		hasTG, hasVK   bool
		wantTG, wantVK bool
	}{
		{"auto: только TG", NotifyAuto, true, false, true, false},
		{"auto: TG+VK → только TG", NotifyAuto, true, true, true, false},
		{"auto: только VK", NotifyAuto, false, true, false, true},
		{"tg выбран и доступен", NotifyTG, true, true, true, false},
		{"vk выбран и доступен", NotifyVK, true, true, false, true},
		{"both: оба", NotifyBoth, true, true, true, true},
		{"both: только TG", NotifyBoth, true, false, true, false},
		{"vk выбран, но отвязан → фолбэк в TG", NotifyVK, true, false, true, false},
		{"tg выбран, но нет TG → фолбэк в VK", NotifyTG, false, true, false, true},
		{"пустая строка = auto", "", true, true, true, false},
	}
	for _, c := range cases {
		tg, vk := ResolveNotifyTargets(c.channel, c.hasTG, c.hasVK)
		if tg != c.wantTG || vk != c.wantVK {
			t.Fatalf("%s: got tg=%v vk=%v, want tg=%v vk=%v", c.name, tg, vk, c.wantTG, c.wantVK)
		}
	}
}
