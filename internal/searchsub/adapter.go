package searchsub

import (
	"math"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// RuleFromSubscription — построить правило движка из доменной подписки.
//
// target_price хранится в БД как NUMERIC рубли → переводим в копейки через
// округление (math.Round убирает дрейф float, например 6239.50*100 = 623949.99).
// Для неприменимых к типу полей значения остаются нулевыми — Decide их
// корректно игнорирует.
func RuleFromSubscription(s *domain.SearchSubscription) Rule {
	r := Rule{Kind: TriggerKind(s.TriggerType)}
	if s.TargetPrice != nil {
		r.TargetKopecks = int64(math.Round(*s.TargetPrice * 100))
	}
	if s.DiscountPct != nil {
		r.DiscountPct = int(*s.DiscountPct)
	}
	return r
}
