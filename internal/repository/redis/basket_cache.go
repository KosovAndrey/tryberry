package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// basketTTL — WB basket-шарды стабильны (меняются только когда WB добавляет новые),
// поэтому держим выученный vol→basket неделю: первый товар нового vol резолвится
// пробой, дальше — из кэша (1 запрос, без перебора). Реализует scraper.BasketResolver.
const basketTTL = 7 * 24 * time.Hour

type BasketCache struct {
	client *redis.Client
}

func NewBasketCache(client *redis.Client) *BasketCache {
	return &BasketCache{client: client}
}

func basketKey(vol int64) string {
	return fmt.Sprintf("wb:basket:vol:%d", vol)
}

// Get — номер выученного basket-шарда для vol (ok=false, если в кэше нет).
func (c *BasketCache) Get(ctx context.Context, vol int64) (int64, bool) {
	val, err := c.client.Get(ctx, basketKey(vol)).Result()
	if errors.Is(err, redis.Nil) || err != nil {
		return 0, false // нет в кэше или Redis недоступен → пробуем пробой
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Put — запомнить найденный пробой basket для vol (best-effort: ошибку Redis глотаем).
func (c *BasketCache) Put(ctx context.Context, vol, basket int64) {
	_ = c.client.Set(ctx, basketKey(vol), strconv.FormatInt(basket, 10), basketTTL).Err()
}
