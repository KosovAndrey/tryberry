package partition

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgDuplicateTable — код ошибки PostgreSQL 42P07 (duplicate_table). Возникает,
// если таблица/партиция уже создана параллельным процессом между нашей
// проверкой существования и CREATE TABLE.
const pgDuplicateTable = "42P07"

// pgUndefinedFunction — 42883: функции ensure_price_history_partition ещё нет
// (миграция 028 не накатана) → фолбэк на прямой DDL.
const pgUndefinedFunction = "42883"

type Manager struct {
	db *pgxpool.Pool
}

func NewManager(db *pgxpool.Pool) *Manager {
	return &Manager{db: db}
}

// EnsurePartitions создаёт партиции для текущего месяца и следующих n месяцев.
// Безопасно вызывать повторно и из нескольких процессов одновременно.
func (m *Manager) EnsurePartitions(ctx context.Context, monthsAhead int) error {
	now := time.Now().UTC()

	for i := 0; i <= monthsAhead; i++ {
		target := now.AddDate(0, i, 0)
		if err := m.ensureOne(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

// EnsureForTimes создаёт партиции для месяцев, в которые попадают переданные
// моменты (для бэкфилла исторических серий в прошлые месяцы). Месяцы дедупятся,
// поэтому серия из десятков точек = единицы CREATE. Идемпотентно и гонко-безопасно.
func (m *Manager) EnsureForTimes(ctx context.Context, times []time.Time) error {
	seen := make(map[string]struct{}, len(times))
	for _, t := range times {
		t = t.UTC()
		key := fmt.Sprintf("%d_%02d", t.Year(), t.Month())
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if err := m.ensureOne(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) ensureOne(ctx context.Context, t time.Time) error {
	// Начало и конец месяца
	from := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)

	tableName := fmt.Sprintf("price_history_%d_%02d", from.Year(), from.Month())

	// Проверяем существует ли партиция
	const checkQ = `
		SELECT EXISTS (
			SELECT 1 FROM pg_class
			WHERE relname = $1 AND relkind = 'r'
		)`

	var exists bool
	if err := m.db.QueryRow(ctx, checkQ, tableName).Scan(&exists); err != nil {
		return fmt.Errorf("check partition %s: %w", tableName, err)
	}
	if exists {
		return nil
	}

	// Основной путь — SECURITY DEFINER-функция (миграция 028): роль приложения
	// после хардинга 2026-07-03 DML-only, прямой CREATE ей запрещён (42501), а
	// функция исполняется правами владельца-суперюзера и умеет ровно одно —
	// создать месячную партицию price_history. Гонку 42P07 она гасит внутри.
	if _, err := m.db.Exec(ctx, `SELECT ensure_price_history_partition($1::date)`, from); err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgUndefinedFunction {
			return fmt.Errorf("create partition %s: %w", tableName, err)
		}
		// функции нет (база без миграции 028) → прямой DDL ниже
	} else {
		return nil
	}

	// Фолбэк: прямой DDL — работает там, где у роли есть права (локалка/тесты
	// под суперюзером).
	//
	// ВАЖНО: `CREATE TABLE IF NOT EXISTS ... PARTITION OF` НЕ идемпотентна под
	// гонкой. Если между нашим SELECT EXISTS и этим CREATE другой процесс
	// (вторая реплика scraper'а или параллельный старт api/notifier) уже создал
	// партицию, PostgreSQL вернёт 42P07 (duplicate_table), а `IF NOT EXISTS`
	// эту гонку не закрывает для partition-of. Поэтому 42P07 трактуем как успех:
	// партиция существует — ровно то, что нам нужно.
	createQ := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s PARTITION OF price_history
		FOR VALUES FROM ('%s') TO ('%s')`,
		tableName,
		from.Format("2006-01-02"),
		to.Format("2006-01-02"),
	)

	if _, err := m.db.Exec(ctx, createQ); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgDuplicateTable {
			// Кто-то опередил нас — партиция уже есть, это не ошибка.
			return nil
		}
		return fmt.Errorf("create partition %s: %w", tableName, err)
	}

	return nil
}
