package domain

import (
	"strings"
	"time"
)

// TrialDuration — срок бесплатного триала поиска.
const TrialDuration = 3 * 24 * time.Hour

// PlanGracePeriod — сколько держим поиск-подписки на паузе после истечения плана,
// прежде чем удалить насовсем. Юзеру обещаем 7 дней на продление; храним 8 с
// запасом (лаг reconcile-тика + часовые пояса). Reconciler в notifier чистит позже.
const PlanGracePeriod = 8 * 24 * time.Hour

// Plan — тариф и его лимиты. Лимиты держим в коде (легко крутить),
// у пользователя в БД хранится только имя плана и срок действия.
type Plan struct {
	Name       string
	Title      string // для отображения в боте
	MaxProduct int    // лимит товарных подписок
	MaxSearch  int    // лимит поиск-подписок

	// Interval — частота проверки (скрейп + оценка уведомлений) для подписок этого
	// плана, ОБА пути: товары и поиск. Эффективный интервал запроса/товара =
	// MIN по подписчикам; доставка же каждому строго по его плану (throttle в
	// воркере/notifier). Reseller-планы ставят 1 минуту → их запросы уходят в
	// быструю дорожку (reseller-tasks, отдельный пул токенов). 0 → дефолт-фолбэк.
	Interval time.Duration

	// PriceRub — цена разовой оплаты в рублях. 0 → план не покупается.
	PriceRub int

	// SubPriceRub — цена автопродления (подписки) в рублях, чуть ниже разовой как
	// nudge к подключению автоплатежа. 0 → подписка для плана недоступна.
	SubPriceRub int
}

// Plans — каталог тарифов. ЦИФРЫ МЕНЯЮТСЯ ЗДЕСЬ.
var Plans = map[string]Plan{
	"free":           {Name: "free", Title: "Free", MaxProduct: 5, MaxSearch: 0, Interval: 60 * time.Minute, PriceRub: 0},
	"trial":          {Name: "trial", Title: "Триал (3 дня)", MaxProduct: 100, MaxSearch: 10, Interval: 15 * time.Minute, PriceRub: 0},
	"lite":           {Name: "lite", Title: "Lite", MaxProduct: 20, MaxSearch: 3, Interval: 30 * time.Minute, PriceRub: 199, SubPriceRub: 189},
	"pro":            {Name: "pro", Title: "Pro", MaxProduct: 100, MaxSearch: 10, Interval: 15 * time.Minute, PriceRub: 499, SubPriceRub: 479},
	"reseller_start": {Name: "reseller_start", Title: "Reseller Start", MaxProduct: 5, MaxSearch: 1, Interval: time.Minute, PriceRub: 990, SubPriceRub: 940},
	"reseller_pro":   {Name: "reseller_pro", Title: "Reseller Pro", MaxProduct: 15, MaxSearch: 3, Interval: time.Minute, PriceRub: 1990, SubPriceRub: 1890},
	"unlimited":      {Name: "unlimited", Title: "Unlimited", MaxProduct: 100000, MaxSearch: 100000, Interval: time.Minute, PriceRub: 0},

	// Legacy-алиасы: чтобы users.plan со старыми именами не откатывался на free
	// до миграции (см. 010_per_plan_intervals.sql). Не предлагаются в /grant.
	"basic":    {Name: "basic", Title: "Basic (legacy)", MaxProduct: 100, MaxSearch: 0, Interval: 30 * time.Minute, PriceRub: 0},
	"reseller": {Name: "reseller", Title: "Reseller (legacy)", MaxProduct: 15, MaxSearch: 3, Interval: time.Minute, PriceRub: 1990},
}

const planFree = "free"

// PlanByName — план по имени (ok=false, если такого нет).
func PlanByName(name string) (Plan, bool) {
	p, ok := Plans[name]
	return p, ok
}

// EffectivePlan — действующий план с учётом срока: если срок истёк → free.
func (u *User) EffectivePlan(now time.Time) Plan {
	name := u.Plan
	if name == "" {
		name = planFree
	}
	if u.PlanExpiresAt != nil && now.After(*u.PlanExpiresAt) {
		name = planFree
	}
	if p, ok := Plans[name]; ok {
		return p
	}
	return Plans[planFree]
}

// PlanExpired — был задан срок и он прошёл.
func (u *User) PlanExpired(now time.Time) bool {
	return u.PlanExpiresAt != nil && now.After(*u.PlanExpiresAt)
}

// EffectiveInterval — частота проверки для плана: Interval, либо переданный
// дефолт-фолбэк, если план её не задаёт (Interval==0).
func (p Plan) EffectiveInterval(def time.Duration) time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}
	return def
}

// BundleWindow — окно коалесинга алертов перед отправкой одним сообщением
// (бандлинг доставки, Phase 1b — см. docs/SCALING-NOTIFIER-DELIVERY.md). Дорогие
// тарифы получают уведомления быстрее (короче окно), free сильнее склеивается;
// reseller/unlimited — без задержки (0). Подбор по тарифу, не по интервалу.
func (p Plan) BundleWindow() time.Duration {
	switch {
	case IsResellerPlan(p.Name), p.Name == "unlimited":
		return 0
	case p.Name == "pro" || p.Name == "trial":
		return 5 * time.Minute
	case p.Name == "lite":
		return 10 * time.Minute
	default: // free, basic (legacy), неизвестный
		return 15 * time.Minute
	}
}

// EffectivePlanFor — действующий план по имени и сроку (без полного User).
// Удобно планировщику/воркеру, у которых на руках только plan + plan_expires_at.
func EffectivePlanFor(name string, expiresAt *time.Time, now time.Time) Plan {
	u := User{Plan: name, PlanExpiresAt: expiresAt}
	return u.EffectivePlan(now)
}

// IsResellerPlan — план «для перекупов» (минутная быстрая дорожка).
// По таким подпискам Ozon НЕ скрейпится: антибот FAB не любит частоту, а Ozon и
// так идёт замедленным тарифным кадансом (×OZON_INTERVAL_MULTIPLIER от WB).
// Распознаём по префиксу имени (reseller_start/reseller_pro/legacy reseller).
func IsResellerPlan(name string) bool {
	return strings.HasPrefix(name, "reseller")
}
