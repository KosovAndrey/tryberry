package domain

import "time"

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

	// SearchInterval — желаемая частота скрейпа выдачи (и оценки уведомлений)
	// для подписок этого плана. 0 → дефолт (SEARCH_SCRAPE_INTERVAL_MINUTES).
	// Тариф «перекуп» ставит сюда 1 минуту: запрос с таким подписчиком уходит
	// в отдельную быструю дорожку (топик reseller-tasks, отдельный пул токенов),
	// а обычные подписчики того же запроса оцениваются по своему (дефолтному)
	// интервалу — см. throttle в search-worker.
	SearchInterval time.Duration
}

// Plans — каталог тарифов. ЦИФРЫ МЕНЯЮТСЯ ЗДЕСЬ.
// Старт консервативный (под слабую VM): поиск только в pro/trial/unlimited.
var Plans = map[string]Plan{
	"free":      {Name: "free", Title: "Free", MaxProduct: 10, MaxSearch: 0},
	"trial":     {Name: "trial", Title: "Триал (3 дня)", MaxProduct: 10, MaxSearch: 3},
	"basic":     {Name: "basic", Title: "Basic", MaxProduct: 100, MaxSearch: 0},
	"pro":       {Name: "pro", Title: "Pro", MaxProduct: 100, MaxSearch: 5},
	"reseller":  {Name: "reseller", Title: "Перекуп", MaxProduct: 100, MaxSearch: 50, SearchInterval: time.Minute},
	"unlimited": {Name: "unlimited", Title: "Unlimited", MaxProduct: 100000, MaxSearch: 100000},
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

// EffectiveSearchInterval — частота скрейпа/оценки для плана: SearchInterval,
// либо переданный дефолт, если план её не задаёт.
func (p Plan) EffectiveSearchInterval(def time.Duration) time.Duration {
	if p.SearchInterval > 0 {
		return p.SearchInterval
	}
	return def
}

// EffectivePlanFor — действующий план по имени и сроку (без полного User).
// Удобно планировщику/воркеру, у которых на руках только plan + plan_expires_at.
func EffectivePlanFor(name string, expiresAt *time.Time, now time.Time) Plan {
	u := User{Plan: name, PlanExpiresAt: expiresAt}
	return u.EffectivePlan(now)
}
