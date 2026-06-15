package payment

// TopicConfirmed — Kafka-топик подтверждённых оплат. api (вебхуки провайдеров)
// публикует сюда после подтверждения оплаты, bot-worker применяет.
const TopicConfirmed = "payments"

// ConfirmedEvent — подтверждённая оплата. Провайдеро-нейтрально: несём наш
// payments.id, применяющая сторона перечитывает строку payments по нему
// (авторитетный источник). UserID — только для ключа партиционирования
// (порядок событий на юзера).
type ConfirmedEvent struct {
	PaymentID int64 `json:"payment_id"`
	UserID    int64 `json:"user_id"`
}
