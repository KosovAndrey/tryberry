package postgres

import "context"

// CountActiveByUserID — число активных товарных подписок пользователя (для лимитов).
func (r *SubscriptionRepo) CountActiveByUserID(ctx context.Context, userID int64) (int, error) {
	const q = `SELECT count(*) FROM subscriptions WHERE user_id = $1 AND active`
	var n int
	if err := r.db.QueryRow(ctx, q, userID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CountActiveByUserID — число активных поиск-подписок пользователя (для лимитов).
func (r *SearchSubscriptionRepo) CountActiveByUserID(ctx context.Context, userID int64) (int, error) {
	const q = `SELECT count(*) FROM search_subscriptions WHERE user_id = $1 AND active`
	var n int
	if err := r.db.QueryRow(ctx, q, userID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
