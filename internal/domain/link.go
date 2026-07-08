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
	NotifyMax  = "max"
	NotifyBoth = "both" // легаси: tg+vk (до появления MAX)
	NotifyAll  = "all"  // все привязанные идентичности
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
	LinkDirTG2VK  = "tg2vk"  // код выдан в TG, предъявляется в VK
	LinkDirVK2TG  = "vk2tg"  // код выдан в VK, предъявляется в TG
	LinkDirTG2Max = "tg2max" // код выдан в TG, предъявляется в MAX
	LinkDirMax2TG = "max2tg" // код выдан в MAX, предъявляется в TG
	LinkDirVK2Max = "vk2max" // код выдан в VK, предъявляется в MAX
	LinkDirMax2VK = "max2vk" // код выдан в MAX, предъявляется в VK
)

var (
	// ErrVKAccountBusy — VK-аккаунт уже привязан к другому непустому аккаунту;
	// тихое поглощение невозможно → боты предлагают слияние (ComputeMerge).
	ErrVKAccountBusy = errors.New("vk account busy")

	// ErrTGAccountBusy — то же для Telegram-аккаунта (направление vk2tg).
	ErrTGAccountBusy = errors.New("tg account busy")

	// ErrMaxAccountBusy — MAX-аккаунт уже привязан к другому непустому аккаунту.
	ErrMaxAccountBusy = errors.New("max account busy")

	// ErrLinkCodeRateLimited — код уже выдавался только что, подожди минуту.
	ErrLinkCodeRateLimited = errors.New("link code rate limited")
)

const linkCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// ResolveNotifyTargets — в какие каналы доставлять уведомление при настройке
// channel и доступных идентичностях. Недоступный выбранный канал откатывается
// на доступный (лучше доставить «не туда», чем потерять уведомление).
func ResolveNotifyTargets(channel string, hasTG, hasVK, hasMax bool) (tg, vk, mx bool) {
	switch channel {
	case NotifyTG:
		tg = hasTG
	case NotifyVK:
		vk = hasVK
	case NotifyMax:
		mx = hasMax
	case NotifyBoth: // легаси: только tg+vk
		tg, vk = hasTG, hasVK
	case NotifyAll:
		tg, vk, mx = hasTG, hasVK, hasMax
	default: // auto: куда зарегистрировался (приоритет tg → vk → max)
		tg = hasTG
		vk = !hasTG && hasVK
		mx = !hasTG && !hasVK && hasMax
	}
	// Фолбэк: выбранный канал недоступен → шлём в первый доступный.
	if !tg && !vk && !mx {
		tg = hasTG
		vk = !hasTG && hasVK
		mx = !hasTG && !hasVK && hasMax
	}
	return tg, vk, mx
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

// MaxStartLink — deep-link на MAX-бота с start-payload: официальный формат
// https://max.ru/<bot>?start=<payload> (payload ≤128 символов, наш link_<код>
// = 13). MAX отдаёт payload в BotStartedUpdate.Payload → handleStart редимит
// link_/ref_ без ручного ввода кода. "" если ссылка на бота не задана.
func MaxStartLink(botURL, payload string) string {
	if botURL == "" || payload == "" {
		return ""
	}
	return botURL + "?start=" + payload
}

// TGStartLink — deep-link на Telegram-бота с start-payload: t.me/<bot>?start=<payload>.
// TG отдаёт payload в аргументах /start (CommandArguments) → бот редимит link_<код>
// без ручного ввода «привязать <код>». Формат совпадает с MaxStartLink, но держим
// отдельно ради читаемости места вызова. "" если ссылка на бота не задана.
func TGStartLink(botURL, payload string) string {
	if botURL == "" || payload == "" {
		return ""
	}
	return botURL + "?start=" + payload
}

// VKRefLink — ссылка на VK-бота с ref-параметром: https://vk.me/club<id>?ref=<payload>.
// VK кладёт payload в message_new.object.message.ref первого сообщения после
// перехода (юзеру достаточно нажать «Начать») → VK-бот редимит link_<код> без
// ручного ввода. "" если ссылка на бота не задана.
func VKRefLink(botURL, payload string) string {
	if botURL == "" || payload == "" {
		return ""
	}
	return botURL + "?ref=" + payload
}
