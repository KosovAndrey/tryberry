package domain

import (
	"time"
)

type User struct {
	ID         int64
	TelegramID int64
	Username   string
	CreatedAt  time.Time
}

type Product struct {
	ID        int64
	URL       string
	Name      string
	ImageURL  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Subscription struct {
	ID            int64
	UserID        int64
	ProductID     int64
	BaselinePrice float64
	Active        bool
	CreatedAt     time.Time
	UpdatedAt     time.Time

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
	ProductID  int64     `json:"product_id"`
	OldPrice   float64   `json:"old_price"`
	NewPrice   float64   `json:"new_price"`
	RecordedAt time.Time `json:"recorded_at"`
}

// ScrapeTask — сообщение в Kafka топике scrape-tasks
type ScrapeTask struct {
	ProductID int64  `json:"product_id"`
	URL       string `json:"url"`
}
