package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/config"
	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
)

// runCleanup — удалить синтетических юзеров нагрузочного теста и всё, что на
// них ссылается (подписки, notifications, pending_alerts). Товары и
// price_history НЕ трогаем — накопленная история цен и есть ценный выход теста.
func runCleanup(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	yes := fs.Bool("yes", false, "подтверждение удаления")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := db.NewPostgresPool(ctx, config.MustEnv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	var users, subs int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE is_synthetic`).Scan(&users); err != nil {
		return err
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM subscriptions s JOIN users u ON u.id = s.user_id
		WHERE u.is_synthetic`).Scan(&subs); err != nil {
		return err
	}
	if users == 0 {
		log.Info("синтетиков нет — удалять нечего")
		return nil
	}
	if !*yes {
		return fmt.Errorf("dry-run: будет удалено %d юзеров и %d подписок; добавь -yes", users, subs)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Порядок — по FK: notifications → pending_alerts → subscriptions → users.
	steps := []struct{ name, q string }{
		{"notifications", `
			DELETE FROM notifications WHERE subscription_id IN (
				SELECT s.id FROM subscriptions s JOIN users u ON u.id = s.user_id
				WHERE u.is_synthetic)`},
		{"pending_alerts", `
			DELETE FROM pending_alerts WHERE user_id IN (
				SELECT id FROM users WHERE is_synthetic)`},
		{"subscriptions", `
			DELETE FROM subscriptions WHERE user_id IN (
				SELECT id FROM users WHERE is_synthetic)`},
		{"users", `DELETE FROM users WHERE is_synthetic`},
	}
	for _, s := range steps {
		tag, err := tx.Exec(ctx, s.q)
		if err != nil {
			return fmt.Errorf("delete %s: %w", s.name, err)
		}
		log.Info("deleted", "table", s.name, "rows", tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	log.Info("cleanup finished; товары и price_history сохранены")
	return nil
}
