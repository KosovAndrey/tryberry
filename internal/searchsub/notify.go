package searchsub

import "context"

// NotifyItem — один подешевевший товар в уведомлении. Цены в копейках.
type NotifyItem struct {
	Name                  string
	URL                   string
	PriceKopecks          int64 // финальная цена (product)
	OldPriceKopecks       int64 // цена до скидки (basic), 0 если нет
	FeedbackPointsKopecks int64 // баллы за отзыв в копейках, 0 если нет
	EffectiveKopecks      int64 // цена срабатывания (price − баллы)
}

// Notification — батч по одной подписке: список подешевевших товаров одному
// пользователю. Одно сообщение на подписку за цикл (антиспам).
type Notification struct {
	TelegramID int64
	QueryText  string
	SearchURL  string // нормализованная ссылка на выдачу (для кнопки «Открыть выдачу»)
	Items      []NotifyItem
}

// SearchNotifier — отправитель уведомлений. Реализации: лог-заглушка (до бота),
// позже — реальная отправка в Telegram. Планировщик пишет search_notifications
// только ПОСЛЕ успешного Notify, иначе протухнет baseline повторных срабатываний.
type SearchNotifier interface {
	Notify(ctx context.Context, n Notification) error
}
