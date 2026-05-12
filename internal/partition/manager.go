package partition

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Manager struct {
	db *pgxpool.Pool
}

func NewManager(db *pgxpool.Pool) *Manager {
	return &Manager{db: db}
}

// EnsurePartitions создаёт партиции для текущего месяца и следующих n месяцев.
// Безопасно вызывать повторно — использует IF NOT EXISTS.
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

	// Создаём партицию
	createQ := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s PARTITION OF price_history
		FOR VALUES FROM ('%s') TO ('%s')`,
		tableName,
		from.Format("2006-01-02"),
		to.Format("2006-01-02"),
	)

	if _, err := m.db.Exec(ctx, createQ); err != nil {
		return fmt.Errorf("create partition %s: %w", tableName, err)
	}

	return nil
}
