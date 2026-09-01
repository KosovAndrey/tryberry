package repository

import (
	"context"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type UserRepo interface {
	// Upsert — создать пользователя или вернуть существующего
	Upsert(ctx context.Context, telegramID int64, username string) (*domain.User, error)
	GetByTelegramID(ctx context.Context, telegramID int64) (*domain.User, error)
}

type ProductRepo interface {
	// Upsert — создать продукт по URL или обновить name/image_url если уже есть
	Upsert(ctx context.Context, url, name, imageURL string) (*domain.Product, error)
	GetByID(ctx context.Context, id int64) (*domain.Product, error)
	// inStock == nil — источник наличия не знает, сохранённое значение не трогаем.
	UpdateScrapedData(ctx context.Context, id int64, name, imageURL string, inStock *bool) (wasInStock bool, err error)
	// GetActiveProductIDs — все уникальные product_id с активными подписками (для планировщика)
	GetActiveProductIDs(ctx context.Context) ([]int64, error)
}

type SubscriptionRepo interface {
	// Upsert — подписать пользователя. Если подписка была отменена — переактивировать.
	// Возвращает (subscription, created, error)
	// created=true — новая подписка, created=false — переактивирована существующая
	Upsert(ctx context.Context, userID, productID int64, baselinePrice float64) (*domain.Subscription, bool, error)
	GetByID(ctx context.Context, id int64) (*domain.Subscription, error)
	GetActiveByUserID(ctx context.Context, userID int64) ([]*domain.Subscription, error)
	// GetActiveByProductID — все активные подписки на товар (для notifier)
	GetActiveByProductID(ctx context.Context, productID int64) ([]*domain.Subscription, error)
	Deactivate(ctx context.Context, id, userID int64) error
	UpdateBaseline(ctx context.Context, id int64, newPrice float64) error
}

type PriceHistoryRepo interface {
	Insert(ctx context.Context, productID int64, price float64) error
	// GetLatest — последняя записанная цена для товара (fallback если Redis недоступен)
	GetLatest(ctx context.Context, productID int64) (float64, time.Time, error)
}

type NotificationRepo interface {
	// Insert — записать уведомление. Если idempotencyKey уже есть — вернуть ErrAlreadyExists.
	Insert(ctx context.Context, n *domain.Notification) error
	ExistsByKey(ctx context.Context, idempotencyKey string) (bool, error)
}
