package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

const priceTTL = 30 * time.Minute

type PriceCache struct {
	client *redis.Client
}

func NewPriceCache(client *redis.Client) *PriceCache {
	return &PriceCache{client: client}
}

func priceKey(productID int64) string {
	return fmt.Sprintf("price:%d", productID)
}

func (c *PriceCache) Get(ctx context.Context, productID int64) (float64, error) {
	val, err := c.client.Get(ctx, priceKey(productID)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, domain.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(val, 64)
}

func (c *PriceCache) Set(ctx context.Context, productID int64, price float64) error {
	val := strconv.FormatFloat(price, 'f', 2, 64)
	return c.client.Set(ctx, priceKey(productID), val, priceTTL).Err()
}

func (c *PriceCache) Invalidate(ctx context.Context, productID int64) error {
	return c.client.Del(ctx, priceKey(productID)).Err()
}
