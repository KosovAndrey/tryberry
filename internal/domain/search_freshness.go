package domain

import (
	"fmt"
	"time"
)

// staleSearchFloor — нижняя граница порога «данные застоялись». Даже у самых
// частых планов (reseller — минута) не ругаемся раньше часа: короткий провал
// это норма работы конвейера, а не отказ площадки.
const staleSearchFloor = time.Hour

// StaleSearchNote — пометка о застоявшейся выдаче для списка поиск-подписок,
// пустая строка, когда всё свежо. Смысл: под блоком маркетплейса подписка
// внешне выглядит живой (уведомлений просто нет), и отличить «цены не падали»
// от «мы неделю не видим выдачу» пользователь не может.
//
// Порог — три ожидаемых интервала плана, но не меньше часа: один-два
// пропущенных цикла бывают штатно, три подряд означают, что площадка не отдаёт
// данные. Свежесть меряем по last_scraped_at запроса; пока его нет, отсчёт идёт
// от создания подписки, иначе новая подписка сразу выглядела бы сломанной.
func StaleSearchNote(lastScrapedAt *time.Time, subCreatedAt time.Time, searchInterval time.Duration, now time.Time) string {
	threshold := 3 * searchInterval
	if threshold < staleSearchFloor {
		threshold = staleSearchFloor
	}
	if lastScrapedAt == nil {
		if now.Sub(subCreatedAt) < threshold {
			return ""
		}
		return "⏳ выдачу пока ни разу не удалось обновить — площадка не отдаёт данные"
	}
	age := now.Sub(*lastScrapedAt)
	if age < threshold {
		return ""
	}
	return fmt.Sprintf("⚠️ выдача не обновлялась %s — похоже, площадка не отдаёт данные", HumanizeAge(age))
}
