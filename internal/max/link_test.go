package max

import (
	"testing"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// Одноклик-кнопка привязки из MAX: TG deep-link для max2tg, VK ref-link для max2vk.
func TestLinkButtonFor(t *testing.T) {
	b := &Bot{
		tgBotURL: "https://t.me/TryBerryBot",
		vkBotURL: "https://vk.me/club239474122",
	}
	tgBtn, ok := b.linkButtonFor(domain.LinkDirMax2TG, "ABCD2345")
	if !ok {
		t.Fatal("max2tg: ожидалась кнопка")
	}
	if want := "https://t.me/TryBerryBot?start=link_ABCD2345"; tgBtn.Link != want {
		t.Errorf("max2tg link = %q, want %q", tgBtn.Link, want)
	}
	vkBtn, ok := b.linkButtonFor(domain.LinkDirMax2VK, "ABCD2345")
	if !ok {
		t.Fatal("max2vk: ожидалась кнопка")
	}
	if want := "https://vk.me/club239474122?ref=link_ABCD2345"; vkBtn.Link != want {
		t.Errorf("max2vk link = %q, want %q", vkBtn.Link, want)
	}
}

// Без ссылки на целевого бота одноклик-кнопка не показывается (фолбэк — ручной ввод).
func TestLinkButtonForMissingURL(t *testing.T) {
	b := &Bot{} // URL-ы не заданы
	if _, ok := b.linkButtonFor(domain.LinkDirMax2TG, "ABCD2345"); ok {
		t.Error("max2tg без TG_BOT_URL: кнопки быть не должно")
	}
	if _, ok := b.linkButtonFor(domain.LinkDirMax2VK, "ABCD2345"); ok {
		t.Error("max2vk без VK_BOT_URL: кнопки быть не должно")
	}
}
