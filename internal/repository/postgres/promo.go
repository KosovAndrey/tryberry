package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

type PromoRepo struct {
	db *pgxpool.Pool
}

func NewPromoRepo(db *pgxpool.Pool) *PromoRepo {
	return &PromoRepo{db: db}
}

// Create — создать промокод (код хранится в UPPER). Для /promo_create.
func (r *PromoRepo) Create(ctx context.Context, p domain.PromoCode) (int64, error) {
	const q = `
		INSERT INTO promo_codes (code, kind, plan, days, discount_pct, max_uses, expires_at)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, 0), NULLIF($5, 0), $6, $7)
		RETURNING id`
	var id int64
	err := r.db.QueryRow(ctx, q,
		domain.NormalizePromoCode(p.Code), p.Kind, p.Plan, p.Days, p.DiscountPct, p.MaxUses, p.ExpiresAt,
	).Scan(&id)
	return id, err
}

// GetActiveByCode — действующий код: active, срок не истёк. Регистронезависимо.
// Истёкший/выключенный/несуществующий код неразличимы для пользователя → ErrNotFound.
func (r *PromoRepo) GetActiveByCode(ctx context.Context, code string) (*domain.PromoCode, error) {
	const q = `
		SELECT id, code, kind, COALESCE(plan, ''), COALESCE(days, 0), COALESCE(discount_pct, 0),
		       max_uses, used_count, expires_at, active, created_at
		FROM promo_codes
		WHERE code = $1 AND active AND (expires_at IS NULL OR expires_at > NOW())`
	p := &domain.PromoCode{}
	err := r.db.QueryRow(ctx, q, domain.NormalizePromoCode(code)).Scan(
		&p.ID, &p.Code, &p.Kind, &p.Plan, &p.Days, &p.DiscountPct,
		&p.MaxUses, &p.UsedCount, &p.ExpiresAt, &p.Active, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// RedeemGrant атомарно гасит grant-код и выдаёт план: инкремент used_count под
// лимитом, запись погашения (UNIQUE отсекает повтор), ВЕЧНЫЕ promo_claims по
// идентичностям (анти-фарм через отвязку, как trial_claims) и установка плана —
// в одной транзакции, чтобы не было «код сгорел, а план не выдан».
func (r *PromoRepo) RedeemGrant(ctx context.Context, codeID, userID int64, plan string, expiresAt time.Time) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback после commit — no-op

	// Лимит использований: атомарный инкремент, без SELECT-проверки.
	tag, err := tx.Exec(ctx,
		`UPDATE promo_codes SET used_count = used_count + 1
		 WHERE id = $1 AND active AND used_count < max_uses`, codeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrPromoExhausted
	}

	// «Один код один раз на юзера» — отвечает UNIQUE (code_id, user_id).
	if _, err := tx.Exec(ctx,
		`INSERT INTO promo_redemptions (code_id, user_id) VALUES ($1, $2)`, codeID, userID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return domain.ErrPromoAlreadyRedeemed
		}
		return err
	}

	// «Один код один раз на идентичность»: строка users пересоздаётся при
	// отвязке платформы, а клеймы по telegram_id/vk_id — вечные.
	var tgID, vkID *int64
	if err := tx.QueryRow(ctx,
		`SELECT telegram_id, vk_id FROM users WHERE id = $1`, userID).Scan(&tgID, &vkID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	for _, c := range []struct {
		platform string
		id       *int64
	}{{"tg", tgID}, {"vk", vkID}} {
		if c.id == nil {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO promo_claims (code_id, platform, external_id) VALUES ($1, $2, $3)`,
			codeID, c.platform, *c.id); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" { // идентичность уже гасила код
				return domain.ErrPromoAlreadyRedeemed
			}
			return err
		}
	}

	tag, err = tx.Exec(ctx,
		`UPDATE users SET plan = $2, plan_expires_at = $3, plan_reminded_at = NULL
		 WHERE id = $1`, userID, plan, expiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	metrics.PromoRedeems.Inc()
	return nil
}

// SetActive — включить/выключить код. Для /promo_off.
func (r *PromoRepo) SetActive(ctx context.Context, code string, active bool) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE promo_codes SET active = $2 WHERE code = $1`,
		domain.NormalizePromoCode(code), active)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// List — последние коды с расходом лимита. Для /promo_list.
func (r *PromoRepo) List(ctx context.Context, limit int) ([]domain.PromoCode, error) {
	const q = `
		SELECT id, code, kind, COALESCE(plan, ''), COALESCE(days, 0), COALESCE(discount_pct, 0),
		       max_uses, used_count, expires_at, active, created_at
		FROM promo_codes
		ORDER BY created_at DESC
		LIMIT $1`
	rows, err := r.db.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.PromoCode
	for rows.Next() {
		var p domain.PromoCode
		if err := rows.Scan(&p.ID, &p.Code, &p.Kind, &p.Plan, &p.Days, &p.DiscountPct,
			&p.MaxUses, &p.UsedCount, &p.ExpiresAt, &p.Active, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
