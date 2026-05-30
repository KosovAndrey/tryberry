package repository

import (
	"context"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// SearchQueryRepo — отслеживаемые поисковые выдачи (шарятся между юзерами).
type SearchQueryRepo interface {
	Upsert(ctx context.Context, marketplace, normalizedURL, queryText string, filters []byte) (*domain.SearchQuery, bool, error)
	GetByID(ctx context.Context, id int64) (*domain.SearchQuery, error)
	// GetScrapable — запросы с >=1 активной подпиской, давно не скрейпленные первыми.
	GetScrapable(ctx context.Context) ([]*domain.SearchQuery, error)
	UpdateLastScraped(ctx context.Context, id int64) error
}

// SearchSubscriptionRepo — подписки пользователей на выдачи + их baseline-цены.
type SearchSubscriptionRepo interface {
	Create(ctx context.Context, s *domain.SearchSubscription) (*domain.SearchSubscription, error)
	GetByID(ctx context.Context, id int64) (*domain.SearchSubscription, error)
	GetActiveByUserID(ctx context.Context, userID int64) ([]*domain.SearchSubscription, error)
	GetActiveByQueryID(ctx context.Context, queryID int64) ([]*domain.SearchSubscription, error)
	Deactivate(ctx context.Context, id int64) error

	// search_subscription_products
	UpsertBaseline(ctx context.Context, subID, productID int64, firstSeenPrice float64) error
	BackfillBaselines(ctx context.Context, subID, queryID int64) (int64, error)
	GetBaseline(ctx context.Context, subID, productID int64) (float64, bool, error)
}

// SearchResultRepo — текущая выдача каждого запроса.
type SearchResultRepo interface {
	// Upsert возвращает isNew=true если товар появился в выдаче впервые.
	Upsert(ctx context.Context, queryID, productID int64, position int, price float64) (bool, error)
	GetByQueryID(ctx context.Context, queryID int64) ([]*domain.SearchResultItem, error)
}

// SearchNotificationRepo — лог уведомлений (и baseline для повторных срабатываний).
type SearchNotificationRepo interface {
	Insert(ctx context.Context, subID, productID int64, price float64) error
	// GetLastNotifiedPrice: ok=false если уведомлений по паре ещё не было.
	GetLastNotifiedPrice(ctx context.Context, subID, productID int64) (float64, bool, error)
	GetBySubscription(ctx context.Context, subID int64) ([]*domain.SearchNotification, error)
}
