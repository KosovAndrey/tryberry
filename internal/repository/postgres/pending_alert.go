package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// PendingAlertRepo — durable-outbox доставки уведомлений (см.
// docs/SCALING-NOTIFIER-DELIVERY.md). Рассчитан на ОДНОГО флашера-синглтона:
// он последовательно берёт FetchDue → доставляет → MarkSent/MarkFailed, поэтому
// без claim-через-UPDATE и FOR UPDATE — провалившаяся строка не перечитается до
// истечения бэкоффа (MarkFailed двигает deliver_after), успешная гасится sent_at.
// Для нескольких флашеров понадобился бы атомарный claim — пока не нужен.
type PendingAlertRepo struct {
	db *pgxpool.Pool
}

func NewPendingAlertRepo(db *pgxpool.Pool) *PendingAlertRepo {
	return &PendingAlertRepo{db: db}
}

// Insert кладёт строку в outbox. ON CONFLICT (idem_key) DO NOTHING гасит
// задвоение при перечитывании Kafka. Возвращает true, если строка реально
// добавлена (а не отсечена конфликтом).
func (r *PendingAlertRepo) Insert(ctx context.Context, a *domain.PendingAlert) (bool, error) {
	const q = `
		INSERT INTO pending_alerts
			(user_id, subscription_id, product_id, payload, idem_key, deliver_after)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idem_key) DO NOTHING`

	deliverAfter := a.DeliverAfter
	if deliverAfter.IsZero() {
		deliverAfter = time.Now()
	}
	tag, err := r.db.Exec(ctx, q,
		a.UserID, a.SubscriptionID, a.ProductID, a.Payload, a.IdemKey, deliverAfter)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// FetchDue возвращает недоставленные созревшие строки по очереди (старейшие
// сперва). limit ограничивает размер батча на один тик флашера. maxAttempts
// отсекает исчерпанные строки (напр. юзер заблокировал бота → 403 навсегда):
// они оседают в таблице с sent_at IS NULL и видны в метрике глубины, но больше
// не ретраятся.
func (r *PendingAlertRepo) FetchDue(ctx context.Context, limit, maxAttempts int) ([]domain.PendingAlert, error) {
	const q = `
		SELECT id, user_id, subscription_id, product_id, payload, idem_key,
		       deliver_after, attempts, sent_at
		FROM pending_alerts
		WHERE sent_at IS NULL AND deliver_after <= now() AND attempts < $2
		ORDER BY deliver_after, id
		LIMIT $1`

	rows, err := r.db.Query(ctx, q, limit, maxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.PendingAlert
	for rows.Next() {
		var a domain.PendingAlert
		if err := rows.Scan(&a.ID, &a.UserID, &a.SubscriptionID, &a.ProductID,
			&a.Payload, &a.IdemKey, &a.DeliverAfter, &a.Attempts, &a.SentAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkSent помечает строку доставленной.
func (r *PendingAlertRepo) MarkSent(ctx context.Context, id int64) error {
	const q = `UPDATE pending_alerts SET sent_at = now() WHERE id = $1`
	_, err := r.db.Exec(ctx, q, id)
	return err
}

// MarkFailed инкрементит attempts, пишет ошибку и отодвигает deliver_after на
// бэкофф — строка не перечитается до nextAfter.
func (r *PendingAlertRepo) MarkFailed(ctx context.Context, id int64, errMsg string, nextAfter time.Time) error {
	const q = `
		UPDATE pending_alerts
		SET attempts = attempts + 1, last_error = $2, deliver_after = $3
		WHERE id = $1`
	_, err := r.db.Exec(ctx, q, id, errMsg, nextAfter)
	return err
}

// CountUnsent — глубина очереди (для метрики).
func (r *PendingAlertRepo) CountUnsent(ctx context.Context) (int, error) {
	const q = `SELECT count(*) FROM pending_alerts WHERE sent_at IS NULL`
	var n int
	err := r.db.QueryRow(ctx, q).Scan(&n)
	return n, err
}

// DeleteSentBefore чистит доставленные старше cutoff. Возвращает число удалённых.
func (r *PendingAlertRepo) DeleteSentBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `DELETE FROM pending_alerts WHERE sent_at IS NOT NULL AND sent_at < $1`
	tag, err := r.db.Exec(ctx, q, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
