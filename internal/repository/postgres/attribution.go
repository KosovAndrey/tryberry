package postgres

import (
	"context"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// SaveAttribution пишет первое касание промо-атрибуции (user_attribution,
// см. миграцию 029). Write-once: PK(user_id) + ON CONFLICT DO NOTHING —
// повторные /start с промо-payload не перезаписывают источник. Гард по
// created_at отсекает старые аккаунты, кликнувшие промо-ссылку: атрибутируем
// только юзеров моложе окна (createdAfter = now − domain.AttributionWindow).
func (r *UserRepo) SaveAttribution(ctx context.Context, userID int64, channel string, att domain.StartAttribution, createdAfter time.Time) error {
	const q = `
		INSERT INTO user_attribution (user_id, channel, payload, format, platform)
		SELECT u.id, $2, $3, $4, $5
		FROM users u
		WHERE u.id = $1 AND u.created_at > $6
		ON CONFLICT (user_id) DO NOTHING`

	return withSpan(ctx, "save_attribution", func(ctx context.Context) error {
		_, err := r.db.Exec(ctx, q, userID, channel, att.Payload, att.Format, att.Platform, createdAfter)
		return err
	})
}
