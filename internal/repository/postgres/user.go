package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type UserRepo struct {
	db *pgxpool.Pool
}

func NewUserRepo(db *pgxpool.Pool) *UserRepo {
	return &UserRepo{db: db}
}

func (r *UserRepo) Upsert(ctx context.Context, telegramID int64, username string) (*domain.User, error) {
	const q = `
		INSERT INTO users (telegram_id, username)
		VALUES ($1, $2)
		ON CONFLICT (telegram_id) DO UPDATE
			SET username = EXCLUDED.username
		RETURNING id, telegram_id, username, created_at, plan, plan_expires_at, trial_used, referred_by,
		          vk_id, notify_channel`

	u := &domain.User{}
	err := withSpan(ctx, "upsert_user", func(ctx context.Context) error {
		return r.db.QueryRow(ctx, q, telegramID, username).
			Scan(&u.ID, &u.TelegramID, &u.Username, &u.CreatedAt, &u.Plan, &u.PlanExpiresAt, &u.TrialUsed, &u.ReferredBy,
				&u.VKID, &u.NotifyChannel)
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (r *UserRepo) GetByTelegramID(ctx context.Context, telegramID int64) (*domain.User, error) {
	const q = `
		SELECT id, telegram_id, username, created_at, plan, plan_expires_at, trial_used, referred_by,
		       vk_id, notify_channel
		FROM users WHERE telegram_id = $1`

	u := &domain.User{}
	err := r.db.QueryRow(ctx, q, telegramID).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.CreatedAt, &u.Plan, &u.PlanExpiresAt, &u.TrialUsed, &u.ReferredBy,
			&u.VKID, &u.NotifyChannel)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// GetByID — юзер по внутреннему id (роутинг уведомлений, рефералка).
func (r *UserRepo) GetByID(ctx context.Context, id int64) (*domain.User, error) {
	const q = `
		SELECT id, COALESCE(telegram_id, 0), COALESCE(username, ''), created_at, plan, plan_expires_at,
		       trial_used, referred_by, vk_id, notify_channel
		FROM users WHERE id = $1`

	u := &domain.User{}
	err := r.db.QueryRow(ctx, q, id).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.CreatedAt, &u.Plan, &u.PlanExpiresAt, &u.TrialUsed, &u.ReferredBy,
			&u.VKID, &u.NotifyChannel)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// GetByVKID — юзер по VK-идентичности. telegram_id может быть NULL (VK-only) →
// COALESCE в 0.
func (r *UserRepo) GetByVKID(ctx context.Context, vkID int64) (*domain.User, error) {
	const q = `
		SELECT id, COALESCE(telegram_id, 0), COALESCE(username, ''), created_at, plan, plan_expires_at,
		       trial_used, referred_by, vk_id, notify_channel
		FROM users WHERE vk_id = $1`

	u := &domain.User{}
	err := r.db.QueryRow(ctx, q, vkID).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.CreatedAt, &u.Plan, &u.PlanExpiresAt, &u.TrialUsed, &u.ReferredBy,
			&u.VKID, &u.NotifyChannel)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// UpsertVK — регистрация/получение юзера по VK-идентичности (вход из VK-бота).
func (r *UserRepo) UpsertVK(ctx context.Context, vkID int64) (*domain.User, error) {
	const q = `
		INSERT INTO users (vk_id, notify_channel)
		VALUES ($1, 'auto')
		ON CONFLICT (vk_id) DO UPDATE SET vk_id = EXCLUDED.vk_id
		RETURNING id, COALESCE(telegram_id, 0), COALESCE(username, ''), created_at, plan, plan_expires_at,
		          trial_used, referred_by, vk_id, notify_channel`

	u := &domain.User{}
	err := r.db.QueryRow(ctx, q, vkID).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.CreatedAt, &u.Plan, &u.PlanExpiresAt, &u.TrialUsed, &u.ReferredBy,
			&u.VKID, &u.NotifyChannel)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// LinkVK привязывает vk_id к юзеру userID. Если vk_id уже занят другой строкой:
// пустую (free, без триала и подписок) поглощаем — удаляем и переносим id;
// непустую не трогаем → domain.ErrVKAccountBusy (merge только вручную).
func (r *UserRepo) LinkVK(ctx context.Context, userID, vkID int64) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback после commit — no-op

	// Кто уже держит этот vk_id? FOR UPDATE — чтобы гонка двух привязок
	// не поглотила одну строку дважды.
	var holderID int64
	var holderTrialUsed bool
	var holderEmpty bool
	err = tx.QueryRow(ctx, `
		SELECT u.id, u.trial_used,
		       u.plan = 'free' AND NOT EXISTS (SELECT 1 FROM subscriptions s WHERE s.user_id = u.id)
		                       AND NOT EXISTS (SELECT 1 FROM search_subscriptions ss WHERE ss.user_id = u.id)
		FROM users u WHERE u.vk_id = $1
		FOR UPDATE`, vkID).Scan(&holderID, &holderTrialUsed, &holderEmpty)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// vk_id свободен — просто привязываем.
	case err != nil:
		return err
	case holderID == userID:
		return nil // уже привязан к этому же юзеру
	case !holderEmpty || holderTrialUsed:
		return domain.ErrVKAccountBusy
	default:
		// Поглощаем пустой VK-аккаунт. trial_used у пустого всегда false —
		// наследовать нечего. FK-зависимостей у пустого обычно нет (подписок нет,
		// промо/рефералка требуют активности); если что-то всё же ссылается —
		// безопасно отказываем как «занят», merge вручную.
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, holderID); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
				return domain.ErrVKAccountBusy
			}
			return err
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE users SET vk_id = $2 WHERE id = $1`, userID, vkID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UnlinkVK — отвязать VK (для смены привязки из TG). Бывший VK-юзер при
// следующем сообщении создаст свежий пустой аккаунт — ничего не наследует.
func (r *UserRepo) UnlinkVK(ctx context.Context, userID int64) error {
	// CHECK chk_user_has_identity не даст отвязать VK у VK-only юзера.
	_, err := r.db.Exec(ctx, `UPDATE users SET vk_id = NULL WHERE id = $1 AND telegram_id IS NOT NULL`, userID)
	return err
}

// LinkTG привязывает telegram_id к юзеру userID (зеркало LinkVK: код выдан в VK,
// предъявлен в TG). Пустой TG-аккаунт (free, без триала и подписок) поглощаем,
// непустой → domain.ErrTGAccountBusy (merge только вручную).
func (r *UserRepo) LinkTG(ctx context.Context, userID, telegramID int64, username string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback после commit — no-op

	// Кто уже держит этот telegram_id? FOR UPDATE против гонки двух привязок.
	var holderID int64
	var holderTrialUsed bool
	var holderEmpty bool
	err = tx.QueryRow(ctx, `
		SELECT u.id, u.trial_used,
		       u.plan = 'free' AND NOT EXISTS (SELECT 1 FROM subscriptions s WHERE s.user_id = u.id)
		                       AND NOT EXISTS (SELECT 1 FROM search_subscriptions ss WHERE ss.user_id = u.id)
		FROM users u WHERE u.telegram_id = $1
		FOR UPDATE`, telegramID).Scan(&holderID, &holderTrialUsed, &holderEmpty)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// telegram_id свободен — просто привязываем.
	case err != nil:
		return err
	case holderID == userID:
		return nil // уже привязан к этому же юзеру
	case !holderEmpty || holderTrialUsed:
		return domain.ErrTGAccountBusy
	default:
		// Поглощаем пустой TG-аккаунт (см. LinkVK: FK-зависимость → «занят»).
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, holderID); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
				return domain.ErrTGAccountBusy
			}
			return err
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE users SET telegram_id = $2, username = $3 WHERE id = $1`,
		userID, telegramID, username); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UnlinkTG — отвязать Telegram (для смены привязки из VK). Бывший TG-юзер при
// следующем /start создаст свежий пустой аккаунт.
func (r *UserRepo) UnlinkTG(ctx context.Context, userID int64) error {
	// CHECK chk_user_has_identity не даст отвязать TG у TG-only юзера.
	_, err := r.db.Exec(ctx,
		`UPDATE users SET telegram_id = NULL, username = NULL WHERE id = $1 AND vk_id IS NOT NULL`, userID)
	return err
}

// SetNotifyChannel — куда слать уведомления (auto|tg|vk|both).
func (r *UserRepo) SetNotifyChannel(ctx context.Context, userID int64, channel string) error {
	_, err := r.db.Exec(ctx, `UPDATE users SET notify_channel = $2 WHERE id = $1`, userID, channel)
	return err
}

// SetPlan — выставить план и срок (expiresAt=nil → бессрочно). Для /grant и /revoke.
func (r *UserRepo) SetPlan(ctx context.Context, telegramID int64, plan string, expiresAt *time.Time) error {
	// plan_reminded_at сбрасываем: новый срок → снова можно напомнить об истечении.
	const q = `UPDATE users SET plan = $2, plan_expires_at = $3, plan_reminded_at = NULL WHERE telegram_id = $1`
	tag, err := r.db.Exec(ctx, q, telegramID, plan, expiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ActivateTrial — однократно включить триал. Возвращает false, если триал уже
// использовался (или пользователь не найден). Ключ — users.id, чтобы работало
// и для VK-аккаунтов без telegram_id.
func (r *UserRepo) ActivateTrial(ctx context.Context, userID int64, expiresAt time.Time) (bool, error) {
	const q = `
		UPDATE users
		SET plan = 'trial', plan_expires_at = $2, trial_used = TRUE, plan_reminded_at = NULL
		WHERE id = $1 AND trial_used = FALSE`
	tag, err := r.db.Exec(ctx, q, userID, expiresAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ListExpiringUnreminded — users.id юзеров с тарифом, истекающим в окне
// (now, until], которым ещё не слали напоминание. Reconciler в notifier шлёт им
// разовое уведомление об истечении (TG/VK по notify_channel) и помечает MarkReminded.
func (r *UserRepo) ListExpiringUnreminded(ctx context.Context, until time.Time) ([]int64, error) {
	const q = `
		SELECT id
		FROM users
		WHERE plan_expires_at IS NOT NULL
		  AND plan_expires_at > NOW() AND plan_expires_at <= $1
		  AND plan_reminded_at IS NULL`
	rows, err := r.db.Query(ctx, q, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkReminded ставит plan_reminded_at=NOW() — защита от повторной отправки.
func (r *UserRepo) MarkReminded(ctx context.Context, userIDs []int64) error {
	if len(userIDs) == 0 {
		return nil
	}
	const q = `UPDATE users SET plan_reminded_at = NOW() WHERE id = ANY($1)`
	_, err := r.db.Exec(ctx, q, userIDs)
	return err
}

// UserUsage — строка для админского /users: план + занятость лимитов.
type UserUsage struct {
	TelegramID    int64
	Username      string
	Plan          string
	PlanExpiresAt *time.Time
	TrialUsed     bool
	Products      int
	Searches      int
}

// ListWithUsage — пользователи с числом активных подписок, по убыванию активности.
func (r *UserRepo) ListWithUsage(ctx context.Context, limit int) ([]UserUsage, error) {
	const q = `
		SELECT COALESCE(u.telegram_id, 0), COALESCE(u.username, ''), u.plan, u.plan_expires_at, u.trial_used,
		       (SELECT count(*) FROM subscriptions s WHERE s.user_id = u.id AND s.active) AS products,
		       (SELECT count(*) FROM search_subscriptions ss WHERE ss.user_id = u.id AND ss.active) AS searches
		FROM users u
		ORDER BY products DESC, searches DESC, u.created_at DESC
		LIMIT $1`
	rows, err := r.db.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UserUsage
	for rows.Next() {
		var x UserUsage
		if err := rows.Scan(&x.TelegramID, &x.Username, &x.Plan, &x.PlanExpiresAt, &x.TrialUsed, &x.Products, &x.Searches); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
