package domain

import "time"

// PendingAlert — строка durable-outbox доставки уведомлений (таблица
// pending_alerts). Консьюмер price-events кладёт её на РЕШЕНИИ (двигая стейт
// подписки вперёд), флашер доставляет ретраями. Payload — сырой JSON
// telegram.PriceAlert: репозиторий слой-нейтрален и не знает про telegram,
// маршалинг/анмаршалинг делает cmd/notifier. См. docs/SCALING-NOTIFIER-DELIVERY.md.
type PendingAlert struct {
	ID             int64
	UserID         int64
	SubscriptionID int64
	ProductID      int64
	IdemKey        string
	Payload        []byte
	DeliverAfter   time.Time
	Attempts       int
	SentAt         *time.Time
}
