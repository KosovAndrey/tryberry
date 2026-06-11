// Package searchsub содержит бизнес-логику поиск-подписок: решение о
// срабатывании уведомления по паре (подписка, товар) и сборку батча по
// подписке. Пакет НЕ зависит от БД, сети и доменных типов хранилища —
// только чистые функции, полностью покрываемые юнит-тестами.
package searchsub

// TriggerKind — тип первого срабатывания подписки.
type TriggerKind string

const (
	// Below — уведомить когда эффективная цена <= target.
	Below TriggerKind = "below_target"
	// AnyDrop — уведомить при любом снижении от стартовой цены (baseline).
	AnyDrop TriggerKind = "any_drop"
	// Discount — уведомить когда скидка от baseline достигает discount_pct.
	Discount TriggerKind = "discount_pct"
)

// Rule — правило подписки. Все деньги в КОПЕЙКАХ.
type Rule struct {
	Kind          TriggerKind
	TargetKopecks int64 // для Below
	DiscountPct   int   // для Discount (1..99)
}

// ProductState — состояние одного товара относительно конкретной подписки
// на момент текущего цикла. Все цены — эффективные (price − баллы), в копейках.
type ProductState struct {
	ProductID int64

	// CurrentKopecks — эффективная цена сейчас.
	CurrentKopecks int64
	// BaselineKopecks — first_seen_price: цена в момент появления товара у
	// подписки. Основа ПЕРВОГО срабатывания (особенно для Discount/AnyDrop).
	BaselineKopecks int64
	// LastNotifiedKopecks — цена последнего отправленного уведомления по паре.
	// Значима только если HasNotified == true.
	LastNotifiedKopecks int64
	// HasNotified — были ли уже уведомления по этой паре (подписка, товар).
	HasNotified bool
}

// Decide — слать ли уведомление по товару. Чистая функция.
//
// Две фазы (см. §7H source-of-truth):
//   - ПЕРВОЕ уведомление: по типу триггера, относительно baseline/target.
//   - ПОВТОРНЫЕ: унифицированно — только если цена упала НИЖЕ цены последнего
//     уведомления. Это само по себе глушит спам: пока цена не падает дальше,
//     тишина; отдельный таймер-кулдаун не нужен.
func Decide(r Rule, st ProductState) bool {
	if st.HasNotified {
		return st.CurrentKopecks < st.LastNotifiedKopecks
	}

	switch r.Kind {
	case Below:
		return r.TargetKopecks > 0 && st.CurrentKopecks <= r.TargetKopecks
	case AnyDrop:
		return st.BaselineKopecks > 0 && st.CurrentKopecks < st.BaselineKopecks
	case Discount:
		t := DiscountThreshold(st.BaselineKopecks, r.DiscountPct)
		return t > 0 && st.CurrentKopecks <= t
	default:
		return false
	}
}

// DiscountThreshold — цена, при которой скидка от baseline достигает pct
// процентов: current <= baseline * (100 - pct) / 100.
//
// Целочисленное деление округляет порог вниз → требуется чуть большее падение
// (консервативно, без ложных срабатываний). Возвращает 0, если параметры
// бессмысленны (нет baseline или pct вне 1..99) — тогда Decide не сработает.
func DiscountThreshold(baselineKopecks int64, pct int) int64 {
	if baselineKopecks <= 0 || pct < 1 || pct > 99 {
		return 0
	}
	return baselineKopecks * int64(100-pct) / 100
}

// RefKopecks — опорная цена товара: от неё считалось снижение в Decide и её
// показываем пользователю как «было». last_notified, если уведомление по паре
// уже было, иначе baseline (first_seen_price). Для товара, впервые попавшего
// в выдачу, опорная цена равна текущей — «было» в сообщении не появится.
func RefKopecks(st ProductState) int64 {
	if st.HasNotified {
		return st.LastNotifiedKopecks
	}
	return st.BaselineKopecks
}

// Evaluate — отобрать товары для уведомления по подписке с правилом r.
// Возвращает попавшие ProductState (с ценами) для формирования батч-сообщения.
// nil, если ничего не сработало.
func Evaluate(r Rule, products []ProductState) []ProductState {
	var hits []ProductState
	for _, st := range products {
		if Decide(r, st) {
			hits = append(hits, st)
		}
	}
	return hits
}
