package redis

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	lockKey = "scheduler:lock"
	lockTTL = 14 * time.Minute // чуть меньше интервала в 15 минут
)

type SchedulerLock struct {
	client *redis.Client
}

func NewSchedulerLock(client *redis.Client) *SchedulerLock {
	return &SchedulerLock{client: client}
}

// Acquire — попытаться захватить lock. Возвращает true если захватили.
// Если Redis недоступен — возвращает (false, err), вызывающий код решает как поступить.
func (l *SchedulerLock) Acquire(ctx context.Context) (bool, error) {
	ok, err := l.client.SetNX(ctx, lockKey, "1", lockTTL).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (l *SchedulerLock) Release(ctx context.Context) error {
	return l.client.Del(ctx, lockKey).Err()
}
