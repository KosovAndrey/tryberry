package payment

// TopicConfirmed — Kafka-топик подтверждённых оплат. api (вебхук ЮKassa)
// публикует сюда после перечтения статуса, bot-worker применяет.
const TopicConfirmed = "payments"

// ConfirmedEvent — подтверждённая оплата. Минимум полей: применяющая сторона
// перечитывает строку payments по yk_payment_id (авторитетный источник).
// UserID — только для ключа партиционирования (порядок событий на юзера).
type ConfirmedEvent struct {
	YKPaymentID string `json:"yk_payment_id"`
	UserID      int64  `json:"user_id"`
}
