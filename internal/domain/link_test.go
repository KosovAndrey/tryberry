package domain

import "testing"

func TestResolveNotifyTargets(t *testing.T) {
	cases := []struct {
		name                   string
		channel                string
		hasTG, hasVK, hasMax   bool
		wantTG, wantVK, wantMx bool
	}{
		{"auto: только TG", NotifyAuto, true, false, false, true, false, false},
		{"auto: TG+VK → только TG", NotifyAuto, true, true, false, true, false, false},
		{"auto: только VK", NotifyAuto, false, true, false, false, true, false},
		{"auto: только MAX", NotifyAuto, false, false, true, false, false, true},
		{"auto: VK+MAX → только VK", NotifyAuto, false, true, true, false, true, false},
		{"tg выбран и доступен", NotifyTG, true, true, false, true, false, false},
		{"vk выбран и доступен", NotifyVK, true, true, false, false, true, false},
		{"max выбран и доступен", NotifyMax, true, false, true, false, false, true},
		{"both: оба", NotifyBoth, true, true, false, true, true, false},
		{"both: только TG", NotifyBoth, true, false, false, true, false, false},
		{"all: все три", NotifyAll, true, true, true, true, true, true},
		{"all: tg+max", NotifyAll, true, false, true, true, false, true},
		{"vk выбран, но отвязан → фолбэк в TG", NotifyVK, true, false, false, true, false, false},
		{"tg выбран, но нет TG → фолбэк в VK", NotifyTG, false, true, false, false, true, false},
		{"max выбран, но нет MAX → фолбэк в TG", NotifyMax, true, false, false, true, false, false},
		{"пустая строка = auto", "", true, true, false, true, false, false},
	}
	for _, c := range cases {
		tg, vk, mx := ResolveNotifyTargets(c.channel, c.hasTG, c.hasVK, c.hasMax)
		if tg != c.wantTG || vk != c.wantVK || mx != c.wantMx {
			t.Fatalf("%s: got tg=%v vk=%v mx=%v, want tg=%v vk=%v mx=%v",
				c.name, tg, vk, mx, c.wantTG, c.wantVK, c.wantMx)
		}
	}
}
