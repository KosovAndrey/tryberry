package domain

import (
	"time"
)

type User struct {
	ID         int64
	TelegramID int64
	Username   string
	CreatedAt  time.Time

	// Тариф/лимиты
	Plan          string     // free | trial | basic | pro | unlimited
	PlanExpiresAt *time.Time // срок действия плана (nil = бессрочно)
	TrialUsed     bool       // триал уже активировался
}

type Product struct {
	ID          int64
	URL         string
	Name        string
	ImageURL    string
	Marketplace string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Subscription struct {
	ID        int64
	UserID    int64
	ProductID int64

	// BaselinePrice — «последняя опорная цена». При подписке = FirstSeenPrice,
	// после каждого уведомления обновляется на цену уведомления. Для повторных
	// срабатываний играет роль «цены последнего уведомления».
	BaselinePrice float64
	// FirstSeenPrice — НЕИЗМЕННАЯ цена на момент подписки. База ПЕРВОГО
	// срабатывания для any_drop и discount_pct (порог скидки не «уплывает»).
	FirstSeenPrice float64

	// Стратегия триггера (как у поиск-подписок).
	TriggerType TriggerType // below_target | any_drop | discount_pct
	TargetPrice *float64    // для below_target
	DiscountPct *int16      // для discount_pct (1..99)
	// Notified — было ли уже хоть одно уведомление по подписке (фаза первого vs
	// повторного срабатывания).
	Notified bool

	Active             bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	TelegramID         int64
	ProductMarketplace string

	// Поля для JOIN-запросов (не хранятся отдельно)
	ProductName     string
	ProductURL      string
	ProductImageURL string
	CurrentPrice    float64
}

type Notification struct {
	ID             int64
	SubscriptionID int64
	OldPrice       float64
	NewPrice       float64
	SentAt         time.Time
	IdempotencyKey string
}

// PriceEvent — сообщение в Kafka топике price-events
type PriceEvent struct {
	ProductID   int64     `json:"product_id"`
	Marketplace string    `json:"marketplace"`
	OldPrice    float64   `json:"old_price"`
	NewPrice    float64   `json:"new_price"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// ScrapeTask — сообщение в Kafka топике scrape-tasks
type ScrapeTask struct {
	ProductID int64  `json:"product_id"`
	URL       string `json:"url"`
}
