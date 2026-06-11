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

// LinkCodeStore — одноразовые коды привязки аккаунтов между платформами.
// Код выдаётся в одной платформе, предъявляется в другой; GETDEL гарантирует
// одноразовость даже при гонке двух предъявлений.
type LinkCodeStore struct {
	client *redis.Client
}

func NewLinkCodeStore(client *redis.Client) *LinkCodeStore {
	return &LinkCodeStore{client: client}
}

func linkCodeKey(code string) string {
	return "link:code:" + code
}

func linkRateKey(userID int64) string {
	return fmt.Sprintf("link:rate:%d", userID)
}

// Issue — выдать код привязки для userID в направлении dir ("tg2vk" | "vk2tg").
// Не чаще раза в минуту на юзера (анти-спам генерации).
func (s *LinkCodeStore) Issue(ctx context.Context, userID int64, dir string) (string, error) {
	ok, err := s.client.SetNX(ctx, linkRateKey(userID), "1", domain.LinkCodeRateLimit).Result()
	if err != nil {
		return "", err
	}
	if !ok {
		return "", domain.ErrLinkCodeRateLimited
	}

	code, err := domain.NewLinkCode()
	if err != nil {
		return "", err
	}
	val := dir + ":" + strconv.FormatInt(userID, 10)
	if err := s.client.Set(ctx, linkCodeKey(code), val, domain.LinkCodeTTL).Err(); err != nil {
		return "", err
	}
	return code, nil
}

// Redeem — атомарно изъять код. Возвращает направление и userID владельца кода.
// Несуществующий/истёкший код → domain.ErrNotFound.
func (s *LinkCodeStore) Redeem(ctx context.Context, code string) (dir string, userID int64, err error) {
	val, err := s.client.GetDel(ctx, linkCodeKey(strings.ToUpper(strings.TrimSpace(code)))).Result()
	if errors.Is(err, redis.Nil) {
		return "", 0, domain.ErrNotFound
	}
	if err != nil {
		return "", 0, err
	}
	dir, idStr, ok := strings.Cut(val, ":")
	if !ok {
		return "", 0, domain.ErrNotFound
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "", 0, domain.ErrNotFound
	}
	return dir, id, nil
}
