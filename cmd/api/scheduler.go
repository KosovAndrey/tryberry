package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/kafka"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
)

func runScheduler(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool, _ string) {
	brokers := strings.Split(mustEnv("KAFKA_BROKERS"), ",")
	intervalStr := getEnv("SCRAPE_INTERVAL_MINUTES", "15")
	intervalMin, _ := strconv.Atoi(intervalStr)
	interval := time.Duration(intervalMin) * time.Minute

	producer := kafka.NewProducer(brokers, "scrape-tasks")
	defer producer.Close()

	productRepo := postgres.NewProductRepo(pool)

	// Запускаем сразу при старте, потом по таймеру
	tick := func() {
		if err := schedulerTick(ctx, log, productRepo, producer); err != nil {
			log.Error("scheduler tick failed", "err", err)
		}
	}

	tick()

	timer := time.NewTicker(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			tick()
		}
	}
}

func schedulerTick(
	ctx context.Context,
	log *slog.Logger,
	productRepo *postgres.ProductRepo,
	producer *kafka.Producer,
) error {
	productIDs, err := productRepo.GetActiveProductIDs(ctx)
	if err != nil {
		return fmt.Errorf("get active product ids: %w", err)
	}

	if len(productIDs) == 0 {
		log.Info("scheduler: no active products")
		return nil
	}

	// Нужны URL для каждого product_id
	sent := 0
	for _, id := range productIDs {
		product, err := productRepo.GetByID(ctx, id)
		if err != nil {
			log.Error("get product", "id", id, "err", err)
			continue
		}

		task := domain.ScrapeTask{
			ProductID: product.ID,
			URL:       product.URL,
		}

		key := strconv.FormatInt(product.ID, 10)
		if err := producer.Send(ctx, key, task); err != nil {
			log.Error("send scrape task", "product_id", id, "err", err)
			continue
		}
		sent++
	}

	log.Info("scheduler tick done", "total", len(productIDs), "sent", sent)
	return nil
}
