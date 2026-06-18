package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type SearchNotificationRepo struct {
	db *pgxpool.Pool
}

func NewSearchNotificationRepo(db *pgxpool.Pool) *SearchNotificationRepo {
	return &SearchNotificationRepo{db: db}
}

// Insert — записать факт отправленного уведомления.
// Запись играет двойную роль (см. §4): лог + baseline. Цена последней записи
// для пары (подписка, товар) = «текущий baseline» для последующих триггеров.
func (r *SearchNotificationRepo) Insert(ctx context.Context, subID, productID int64, price float64) error {
	const q = `
		INSERT INTO search_notifications (subscription_id, product_id, price)
		VALUES ($1, $2, $3)`

	return withSpan(ctx, "insert_search_notification", func(ctx context.Context) error {
		_, err := r.db.Exec(ctx, q, subID, productID, price)
		return err
	})
}

// GetLastNotifiedPrice — цена последнего уведомления по паре (подписка, товар).
// ok=false если уведомлений ещё не было — это сигнал движку применить логику
// ПЕРВОГО уведомления (по типу триггера), а не унифицированный any_drop.
//
// Индекс (subscription_id, product_id, sent_at DESC) делает это O(1)-lookup.
func (r *SearchNotificationRepo) GetLastNotifiedPrice(ctx context.Context, subID, productID int64) (float64, bool, error) {
	const q = `
		SELECT price
		FROM search_notifications
		WHERE subscription_id = $1 AND product_id = $2
		ORDER BY sent_at DESC
		LIMIT 1`

	var price float64
	err := r.db.QueryRow(ctx, q, subID, productID).Scan(&price)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return price, true, nil
}

// GetLastNotifiedAt — время последнего отправленного уведомления подписки (любого
// товара). Для троттлинга below_target на широких/ротирующихся выдачах. false —
// если уведомлений ещё не было.
func (r *SearchNotificationRepo) GetLastNotifiedAt(ctx context.Context, subID int64) (time.Time, bool, error) {
	const q = `
		SELECT sent_at
		FROM search_notifications
		WHERE subscription_id = $1
		ORDER BY sent_at DESC
		LIMIT 1`

	var t time.Time
	err := r.db.QueryRow(ctx, q, subID).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// GetBySubscription — история уведомлений подписки (для UI/отладки).
func (r *SearchNotificationRepo) GetBySubscription(ctx context.Context, subID int64) ([]*domain.SearchNotification, error) {
	const q = `
		SELECT id, subscription_id, product_id, price, sent_at
		FROM search_notifications
		WHERE subscription_id = $1
		ORDER BY sent_at DESC`

	rows, err := r.db.Query(ctx, q, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.SearchNotification
	for rows.Next() {
		n := &domain.SearchNotification{}
		if err := rows.Scan(&n.ID, &n.SubscriptionID, &n.ProductID, &n.Price, &n.SentAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
