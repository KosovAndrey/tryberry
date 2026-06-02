package domain

import "time"

// TrialDuration — срок бесплатного триала поиска.
const TrialDuration = 3 * 24 * time.Hour

// Plan — тариф и его лимиты. Лимиты держим в коде (легко крутить),
// у пользователя в БД хранится только имя плана и срок действия.
type Plan struct {
	Name       string
	Title      string // для отображения в боте
	MaxProduct int    // лимит товарных подписок
	MaxSearch  int    // лимит поиск-подписок
}

// Plans — каталог тарифов. ЦИФРЫ МЕНЯЮТСЯ ЗДЕСЬ.
// Старт консервативный (под слабую VM): поиск только в pro/trial/unlimited.
var Plans = map[string]Plan{
	"free":      {Name: "free", Title: "Free", MaxProduct: 10, MaxSearch: 0},
	"trial":     {Name: "trial", Title: "Триал (3 дня)", MaxProduct: 10, MaxSearch: 3},
	"basic":     {Name: "basic", Title: "Basic", MaxProduct: 100, MaxSearch: 0},
	"pro":       {Name: "pro", Title: "Pro", MaxProduct: 100, MaxSearch: 5},
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
