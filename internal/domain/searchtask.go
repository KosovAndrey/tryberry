// internal/domain/searchtask.go
package domain

// SearchTask — сообщение в Kafka топике search-tasks: задание на скрейп одной
// поисковой выдачи. Кладёт scheduler (синглтон), разбирает scraper (consumer
// group "search-workers"). Ключ партиционирования — QueryID, поэтому задачи по
// одному запросу всегда попадают на один воркер.
type SearchTask struct {
	QueryID   int64  `json:"query_id"`
	URL       string `json:"url"` // normalized_url выдачи
	QueryText string `json:"query_text"`
}

// SearchHitItem — один сработавший товар в уведомлении поиск-подписки.
//
// PrevPriceKopecks — опорная цена, от которой движок считал снижение:
// last_notified (если по паре уже было уведомление) или baseline
// (first_seen_price). Зачёркнутая цена WB (price.basic) сюда НЕ попадает:
// она маркетинговая, к реальной динамике цены отношения не имеет и при
// первом появлении товара давала ложное «было X ₽».
type SearchHitItem struct {
	ProductID             int64  `json:"product_id"`
	Name                  string `json:"name"`
	URL                   string `json:"url"`
	PriceKopecks          int64  `json:"price_kopecks"`
	PrevPriceKopecks      int64  `json:"prev_price_kopecks"`
	FeedbackPointsKopecks int64  `json:"feedback_points_kopecks"`
	EffectiveKopecks      int64  `json:"effective_kopecks"` // текущая эффективная цена (== current)
}

// SearchHitEvent — сообщение в Kafka топике search-events: сработавшие триггеры
// одной поиск-подписки. Продюсит scraper (после оценки триггеров), разбирает
// notifier: шлёт в Telegram и ТОЛЬКО после успешной доставки пишет
// search_notifications (как в товарном пути). Ключ партиционирования —
// UserID, чтобы уведомления одного пользователя не разъезжались по партициям.
type SearchHitEvent struct {
	SubID       int64           `json:"sub_id"`
	TelegramID  int64           `json:"telegram_id"` // 0 — TG не привязан (VK-only)
	UserID      int64           `json:"user_id"`     // users.id — роутинг по notify_channel
	QueryText   string          `json:"query_text"`
	SearchURL   string          `json:"search_url"`
	TriggerType string          `json:"trigger_type"`
	Items       []SearchHitItem `json:"items"`
}
