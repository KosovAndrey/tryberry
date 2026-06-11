package domain

import (
	"crypto/rand"
	"errors"
	"time"
)

// Каналы доставки уведомлений (users.notify_channel).
const (
	NotifyAuto = "auto" // куда зарегистрировался (по наличию идентичности)
	NotifyTG   = "tg"
	NotifyVK   = "vk"
	NotifyBoth = "both"
)

// Привязка аккаунтов между платформами: одноразовый код доказывает владение
// обеими сторонами (получен в одной, предъявлен в другой). Без кода привязка
// по голому id — это угон чужих аккаунтов, см. docs/VK-INTEGRATION-PLAN.md.
const (
	// LinkCodeTTL — время жизни кода привязки.
	LinkCodeTTL = 15 * time.Minute

	// LinkCodeLen — длина кода. Алфавит без похожих символов (0/O, 1/I/L).
	LinkCodeLen = 8

	// LinkCodeRateLimit — минимальный интервал между выдачами кода одному юзеру.
	LinkCodeRateLimit = time.Minute
)

// Направления привязки: где код выдан → где предъявлен.
const (
	LinkDirTG2VK = "tg2vk" // код выдан в TG, предъявляется в VK
	LinkDirVK2TG = "vk2tg" // код выдан в VK, предъявляется в TG
)

var (
	// ErrVKAccountBusy — VK-аккаунт уже привязан к другому непустому аккаунту;
	// автоматический merge запрещён (v1), объединение — вручную через поддержку.
	ErrVKAccountBusy = errors.New("vk account busy")

	// ErrLinkCodeRateLimited — код уже выдавался только что, подожди минуту.
	ErrLinkCodeRateLimited = errors.New("link code rate limited")
)

const linkCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// ResolveNotifyTargets — в какие каналы доставлять уведомление при настройке
// channel и доступных идентичностях. Недоступный выбранный канал откатывается
// на доступный (лучше доставить «не туда», чем потерять уведомление).
func ResolveNotifyTargets(channel string, hasTG, hasVK bool) (tg, vk bool) {
	switch channel {
	case NotifyTG:
		tg = hasTG
	case NotifyVK:
		vk = hasVK
	case NotifyBoth:
		tg, vk = hasTG, hasVK
	default: // auto: куда зарегистрировался
		tg = hasTG
		vk = !hasTG && hasVK
	}
	// Фолбэк: выбранный канал недоступен → шлём в доступный.
	if !tg && !vk {
		tg, vk = hasTG, !hasTG && hasVK
	}
	return tg, vk
}

// NewLinkCode — криптослучайный код привязки.
func NewLinkCode() (string, error) {
	buf := make([]byte, LinkCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = linkCodeAlphabet[int(b)%len(linkCodeAlphabet)]
	}
	return string(buf), nil
}
