package domain

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Тексты и парсинг пользовательского ввода, общие для TG- и VK-ботов.
// Всё plain text (без HTML) — TG-обработчики добавляют разметку сами.

// PlanShowcase — порядок и слоганы тарифов на витрине (TG /plans и VK
// «Тарифы»). Цифры (цены/лимиты/интервалы) — в Plans.
var PlanShowcase = []struct {
	Name    string
	Tagline string
}{
	{"lite", "следить за своими покупками"},
	{"pro", "большие списки и быстрые проверки"},
	{"reseller_start", "для перекупов: проверка раз в минуту"},
	{"reseller_pro", "максимум скорости и объёма"},
}

// IntervalPhrase — «каждую минуту / каждые 15 минут / каждый час».
func IntervalPhrase(d time.Duration) string {
	m := int(d.Minutes())
	switch {
	case m <= 1:
		return "каждую минуту"
	case m == 60:
		return "каждый час"
	default:
		return fmt.Sprintf("каждые %d минут", m)
	}
}

// NotifyChannelTitle — человекочитаемое имя канала уведомлений.
func NotifyChannelTitle(ch string) string {
	switch ch {
	case NotifyTG:
		return "Telegram"
	case NotifyVK:
		return "VK"
	case NotifyBoth:
		return "Telegram + VK"
	default:
		return "Telegram" // auto у привязанных трактуем как TG (зарегался в TG)
	}
}

// TriggerDescription — человекочитаемое описание условия уведомления.
func TriggerDescription(t TriggerType, target *float64, pct *int16) string {
	switch t {
	case TriggerBelowTarget:
		if target != nil {
			return fmt.Sprintf("📉 уведомлю, когда цена опустится ниже %.0f ₽", *target)
		}
		return "📉 уведомлю при достижении целевой цены"
	case TriggerAnyDrop:
		return "🔻 уведомлю при любом снижении цены"
	case TriggerDiscountPct:
		if pct != nil {
			return fmt.Sprintf("％ уведомлю при скидке от %d%%", *pct)
		}
		return "％ уведомлю при заметной скидке"
	case TriggerBackInStock:
		return "🔔 уведомлю, когда товар снова появится в наличии"
	default:
		return ""
	}
}

// DaysWord — «3 дня», «7 дней», «21 день».
func DaysWord(n int) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return "день"
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return "дня"
	default:
		return "дней"
	}
}

// QueryTextFromNormalized — человекочитаемый запрос из нормализованного
// поискового URL (...search.aspx?search=...&sort=...) для отображения.
func QueryTextFromNormalized(normalized string) string {
	// Текст запроса лежит в query-параметре: у WB это search=, у Я.Маркета text=.
	// Парсим URL и берём первый непустой — так не зависим от формата конкретного
	// маркетплейса и порядка параметров.
	if u, err := url.Parse(normalized); err == nil {
		q := u.Query()
		for _, key := range []string{"search", "text"} {
			if v := strings.TrimSpace(q.Get(key)); v != "" {
				return v
			}
		}
	}
	return normalized
}

// ParsePrice — цена из пользовательского ввода («59 990», «59990,50», «100 ₽»).
func ParsePrice(s string) (float64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.TrimSuffix(s, "₽")
	s = strings.TrimSuffix(s, "руб")
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v <= 0 {
		return 0, errors.New("invalid price")
	}
	return v, nil
}

// ParsePct — процент скидки 1–99 («20», «20%»).
func ParsePct(s string) (int16, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > 99 {
		return 0, errors.New("invalid pct")
	}
	return int16(v), nil
}
