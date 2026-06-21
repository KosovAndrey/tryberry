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

type UserRepo struct {
	db *pgxpool.Pool
}

func NewUserRepo(db *pgxpool.Pool) *UserRepo {
	return &UserRepo{db: db}
}

// DigestRecipient — кому слать дайджест (минимум для роутинга; канал/план
// дотягивает deliverer/билдер по user_id).
type DigestRecipient struct {
	UserID     int64
	TelegramID int64
}

// UsersDueForDigest — юзеры с ≥1 активной ТОВАРНОЙ подпиской, которым пора слать
// дайджест (last_digest_at IS NULL или старше before). limit ограничивает пачку на
// тик (чтобы не бластить всех разом). Сортировка по last_digest_at — самые «давние»
// первыми (NULL раньше всего).
func (r *UserRepo) UsersDueForDigest(ctx context.Context, before time.Time, limit int) ([]DigestRecipient, error) {
	// COALESCE: у VK-only юзеров telegram_id = NULL → 0 (deliverer отправит в VK по
	// notify_channel; в канареечном фильтре по tg_id такой юзер просто не совпадёт).
	const q = `
		SELECT u.id, COALESCE(u.telegram_id, 0)
		FROM users u
		WHERE (u.last_digest_at IS NULL OR u.last_digest_at < $1)
		  AND EXISTS (SELECT 1 FROM subscriptions s WHERE s.user_id = u.id AND s.active = TRUE)
		ORDER BY u.last_digest_at ASC NULLS FIRST
		LIMIT $2`

	rows, err := r.db.Query(ctx, q, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DigestRecipient
	for rows.Next() {
		var d DigestRecipient
		if err := rows.Scan(&d.UserID, &d.TelegramID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDigestSent двигает чек-поинт дайджеста (вызывается после обработки юзера,
// независимо от того, было ли что слать — чтобы каданс оставался недельным).
func (r *UserRepo) MarkDigestSent(ctx context.Context, userID int64, at time.Time) error {
	_, err := r.db.Exec(ctx, `UPDATE users SET last_digest_at = $2 WHERE id = $1`, userID, at)
	return err
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

// MergeAccounts — слияние двух непустых аккаунтов (владение обоими доказано
// кодом привязки): весь контент absorbed переезжает на kept, absorbed
// удаляется, kept получает обе идентичности и итоговый тариф (выбор юзера —
// см. domain.ComputeMerge). detail — JSON для аудита (account_merges).
func (r *UserRepo) MergeAccounts(ctx context.Context, keptID, absorbedID int64, plan string, expiresAt *time.Time, detail []byte) error {
	if keptID == absorbedID {
		return domain.ErrNotFound
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback после commit — no-op

	// Лочим обе строки в порядке id (анти-deadlock при встречных слияниях).
	type row struct {
		tgID, vkID    *int64
		username      *string
		plan          string
		planExpiresAt *time.Time
		trialUsed     bool
		referredBy    *int64
	}
	read := func(id int64) (row, error) {
		var x row
		err := tx.QueryRow(ctx, `
			SELECT telegram_id, vk_id, username, plan, plan_expires_at, trial_used, referred_by
			FROM users WHERE id = $1 FOR UPDATE`, id).
			Scan(&x.tgID, &x.vkID, &x.username, &x.plan, &x.planExpiresAt, &x.trialUsed, &x.referredBy)
		return x, err
	}
	var kept, absorbed row
	if keptID < absorbedID {
		if kept, err = read(keptID); err != nil {
			return err
		}
		if absorbed, err = read(absorbedID); err != nil {
			return err
		}
	} else {
		if absorbed, err = read(absorbedID); err != nil {
			return err
		}
		if kept, err = read(keptID); err != nil {
			return err
		}
	}

	// Товарные подписки: UNIQUE(user_id, product_id) — дубликаты absorbed
	// удаляем (вместе с их notifications, FK без каскада), остальные переносим.
	if _, err := tx.Exec(ctx, `
		DELETE FROM notifications WHERE subscription_id IN (
			SELECT a.id FROM subscriptions a
			JOIN subscriptions k ON k.user_id = $1 AND k.product_id = a.product_id
			WHERE a.user_id = $2)`, keptID, absorbedID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM subscriptions a USING subscriptions k
		WHERE a.user_id = $2 AND k.user_id = $1 AND k.product_id = a.product_id`,
		keptID, absorbedID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE subscriptions SET user_id = $1 WHERE user_id = $2`, keptID, absorbedID); err != nil {
		return err
	}

	// Поиск-подписки: уникальных констрейнтов по (user, query) нет — переносим всё.
	if _, err := tx.Exec(ctx,
		`UPDATE search_subscriptions SET user_id = $1 WHERE user_id = $2`, keptID, absorbedID); err != nil {
		return err
	}

	// Погашенные промокоды: UNIQUE(code_id, user_id) — дубликаты удаляем.
	if _, err := tx.Exec(ctx, `
		DELETE FROM promo_redemptions a USING promo_redemptions k
		WHERE a.user_id = $2 AND k.user_id = $1 AND k.code_id = a.code_id`,
		keptID, absorbedID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE promo_redemptions SET user_id = $1 WHERE user_id = $2`, keptID, absorbedID); err != nil {
		return err
	}

	// Рефералка: награды и указатели «кто привёл».
	if _, err := tx.Exec(ctx,
		`UPDATE referral_rewards SET referrer_id = $1 WHERE referrer_id = $2`, keptID, absorbedID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM referral_rewards a USING referral_rewards k
		WHERE a.referee_id = $2 AND k.referee_id = $1 AND k.event = a.event`,
		keptID, absorbedID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE referral_rewards SET referee_id = $1 WHERE referee_id = $2`, keptID, absorbedID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET referred_by = $1 WHERE referred_by = $2`, keptID, absorbedID); err != nil {
		return err
	}

	// Аудит — до удаления absorbed, чтобы зафиксировать исходное состояние.
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_merges
			(kept_user_id, absorbed_user_id, kept_plan, kept_expires_at,
			 absorbed_plan, absorbed_expires_at, result_plan, result_expires_at, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		keptID, absorbedID, kept.plan, kept.planExpiresAt,
		absorbed.plan, absorbed.planExpiresAt, plan, expiresAt, detail); err != nil {
		return err
	}

	// Удаляем absorbed (контент уже переехал) и собираем kept: обе идентичности,
	// итоговый тариф, trial_used = OR, referred_by наследуется, если не было.
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, absorbedID); err != nil {
		return err
	}
	newTG, newUsername := kept.tgID, kept.username
	if newTG == nil {
		newTG, newUsername = absorbed.tgID, absorbed.username
	}
	newVK := kept.vkID
	if newVK == nil {
		newVK = absorbed.vkID
	}
	newRef := kept.referredBy
	if newRef == nil && absorbed.referredBy != nil && *absorbed.referredBy != keptID {
		newRef = absorbed.referredBy
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users SET telegram_id = $2, username = $3, vk_id = $4,
		       plan = $5, plan_expires_at = $6, plan_reminded_at = NULL,
		       trial_used = $7, referred_by = $8
		WHERE id = $1`,
		keptID, newTG, newUsername, newVK, plan, expiresAt,
		kept.trialUsed || absorbed.trialUsed, newRef); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	metrics.AccountMerges.Inc()
	return nil
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

// GetEmail — email покупателя для чека 54-ФЗ ("" — ещё не задан).
func (r *UserRepo) GetEmail(ctx context.Context, userID int64) (string, error) {
	var email *string
	err := r.db.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if email == nil {
		return "", nil
	}
	return *email, nil
}

// SetEmail — сохранить email покупателя (для будущих оплат не переспрашиваем).
func (r *UserRepo) SetEmail(ctx context.Context, userID int64, email string) error {
	_, err := r.db.Exec(ctx, `UPDATE users SET email = $2 WHERE id = $1`, userID, email)
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
// использовался. Однократность держат ВЕЧНЫЕ trial_claims по идентичностям
// (telegram_id/vk_id): строка users пересоздаётся при отвязке платформы, и
// флаг trial_used сам по себе позволял фармить триалы циклом отвязок.
func (r *UserRepo) ActivateTrial(ctx context.Context, userID int64, expiresAt time.Time) (bool, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback после commit — no-op

	var tgID, vkID *int64
	var used bool
	err = tx.QueryRow(ctx,
		`SELECT telegram_id, vk_id, trial_used FROM users WHERE id = $1 FOR UPDATE`, userID).
		Scan(&tgID, &vkID, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if used {
		return false, nil
	}

	claim := func(platform string, id *int64) (bool, error) {
		if id == nil {
			return true, nil
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO trial_claims (platform, external_id) VALUES ($1, $2)`, platform, *id); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" { // идентичность уже брала триал
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	for _, c := range []struct {
		platform string
		id       *int64
	}{{"tg", tgID}, {"vk", vkID}} {
		ok, err := claim(c.platform, c.id)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET plan = 'trial', plan_expires_at = $2, trial_used = TRUE, plan_reminded_at = NULL
		 WHERE id = $1`, userID, expiresAt); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
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
