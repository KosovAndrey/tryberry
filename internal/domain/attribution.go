package domain

import (
	"strings"
	"time"
)

// Атрибуция первого касания: deep-link из промо-роликов (docs/PROMO-SHORTS-PLAN.md §6)
// вида v_<формат>_<площадка> — t.me/bot?start=v_f1_yt, vk.me/...?ref=v_f1_yt,
// MAX start-payload. Пишется один раз на юзера (user_attribution, PK user_id);
// префикс v_ выбран, чтобы не пересекаться с promo_/ref_/link_.

const (
	// AttributionWindow — максимальный возраст аккаунта, при котором первое
	// касание ещё засчитывается. Отсекает старых юзеров, кликнувших промо-ссылку;
	// с запасом покрывает TG-гейт согласия ПД (payload ждёт в Redis до 10 минут,
	// но юзер может нажать «Принимаю» сильно позже создания строки users).
	AttributionWindow = 48 * time.Hour

	attributionPrefix  = "v_"
	attributionPartMax = 32 // потолок длины формата/площадки
)

// StartAttribution — распарсенный промо-payload.
type StartAttribution struct {
	Payload  string // сырой payload (v_f1_yt)
	Format   string // f1
	Platform string // yt
}

// ParseStartAttribution разбирает payload вида v_<формат>_<площадка>.
// Формат — до первого «_», площадка — остаток (допускает «_» внутри, например
// v_f1_yt_shorts). Обе части: [a-z0-9-], непустые, не длиннее attributionPartMax.
func ParseStartAttribution(payload string) (StartAttribution, bool) {
	rest, ok := strings.CutPrefix(payload, attributionPrefix)
	if !ok {
		return StartAttribution{}, false
	}
	format, platform, ok := strings.Cut(rest, "_")
	if !ok || !validAttributionPart(format) || !validAttributionPart(platform) {
		return StartAttribution{}, false
	}
	return StartAttribution{Payload: payload, Format: format, Platform: platform}, true
}

func validAttributionPart(s string) bool {
	if s == "" || len(s) > attributionPartMax {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
