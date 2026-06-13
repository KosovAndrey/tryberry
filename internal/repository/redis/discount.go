package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// DiscountStore — «ожидающая скидка» юзера от погашенного discount-промокода.
// Хранится до оплаты (TTL), ключ = users.id. На plan:buy скидка читается и
// зашивается в платёж; после успешной оплаты — удаляется.
type DiscountStore struct {
	client *redis.Client
}

func NewDiscountStore(client *redis.Client) *DiscountStore {
	return &DiscountStore{client: client}
}

func discountKey(userID int64) string {
	return fmt.Sprintf("promo:discount:%d", userID)
}

// PendingDiscount — отложенная скидка: id кода и процент.
type PendingDiscount struct {
	CodeID int64
	Pct    int
}

// Put — запомнить скидку pct% по коду codeID для userID (перезаписывает прежнюю).
func (s *DiscountStore) Put(ctx context.Context, userID, codeID int64, pct int) error {
	val := fmt.Sprintf("%d:%d", codeID, pct)
	return s.client.Set(ctx, discountKey(userID), val, domain.PromoDiscountTTL).Err()
}

// Get — текущая ожидающая скидка (ok=false, если нет/истекла).
func (s *DiscountStore) Get(ctx context.Context, userID int64) (PendingDiscount, bool, error) {
	val, err := s.client.Get(ctx, discountKey(userID)).Result()
	if errors.Is(err, redis.Nil) {
		return PendingDiscount{}, false, nil
	}
	if err != nil {
		return PendingDiscount{}, false, err
	}
	parts := strings.SplitN(val, ":", 2)
	if len(parts) != 2 {
		return PendingDiscount{}, false, nil
	}
	codeID, err1 := strconv.ParseInt(parts[0], 10, 64)
	pct, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return PendingDiscount{}, false, nil
	}
	return PendingDiscount{CodeID: codeID, Pct: pct}, true, nil
}

// Del — снять ожидающую скидку (после успешной оплаты).
func (s *DiscountStore) Del(ctx context.Context, userID int64) error {
	return s.client.Del(ctx, discountKey(userID)).Err()
}
