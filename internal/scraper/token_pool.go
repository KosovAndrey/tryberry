package scraper

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultPoolPrefix = "wb:search:" // обычная дорожка; перекупы — "wb:reseller:"
	bad429Threshold   = 2            // подряд идущих 429 на одном слоте → пометить битым
)

// RedisSearchTokenPool — round-robin провайдер WB-токенов из пула в Redis.
//
// Слот {prefix}pool:{i} — HASH с полями cookie/ua/token/status(ok|broken)/
// mined_at/exp, наполняется майнером. Token() выдаёт по кругу живые слоты
// (status=ok, непустой cookie). После bad429Threshold подряд идущих 429 на
// слоте MarkBad помечает его broken в Redis (майнер потом дольёт замену).
// Если живых слотов нет — фолбэк на legacy-ключи {prefix}cookie/{prefix}ua
// (на время миграции и как страховка).
//
// Префикс параметризован: обычная дорожка — "wb:search:", перекупы —
// "wb:reseller:" (отдельный пул, свой майнер). Ключи должны совпадать с
// SLOT_PREFIX майнера (token-miner/miner.py).
//
// Счётчик подряд идущих 429 — в памяти процесса (эфемерный, на реплику),
// сбрасывается успешным запросом (MarkGood).
type RedisSearchTokenPool struct {
	rc   *redis.Client
	size int
	log  *slog.Logger

	slotPrefix string // {prefix}pool:
	rrKey      string // {prefix}rr
	legacyCookieKey,
	legacyUAKey string

	mu    sync.Mutex
	fails map[int]int
}

// NewRedisSearchTokenPool — провайдер на size слотов с заданным префиксом ключей
// Redis (пустой → "wb:search:"). rc может быть nil (тогда Token вернёт ошибку),
// log nil → slog.Default().
func NewRedisSearchTokenPool(rc *redis.Client, size int, log *slog.Logger, prefix string) *RedisSearchTokenPool {
	if size <= 0 {
		size = 5
	}
	if log == nil {
		log = slog.Default()
	}
	if prefix == "" {
		prefix = defaultPoolPrefix
	}
	return &RedisSearchTokenPool{
		rc:              rc,
		size:            size,
		log:             log,
		slotPrefix:      prefix + "pool:",
		rrKey:           prefix + "rr",
		legacyCookieKey: prefix + "cookie",
		legacyUAKey:     prefix + "ua",
		fails:           make(map[int]int),
	}
}

// PoolSize — размер пула (используется fetchPage для расчёта числа попыток).
func (p *RedisSearchTokenPool) PoolSize() int { return p.size }

func (p *RedisSearchTokenPool) slotKey(i int) string {
	return p.slotPrefix + strconv.Itoa(i)
}

// Token — round-robin по живым слотам; при пустом пуле — фолбэк на legacy-ключи.
func (p *RedisSearchTokenPool) Token(ctx context.Context) (SearchToken, error) {
	if p.rc == nil {
		return SearchToken{}, fmt.Errorf("redis unavailable")
	}

	start := 0
	if n, err := p.rc.Incr(ctx, p.rrKey).Result(); err == nil {
		start = int(n % int64(p.size))
	}
	if start < 0 {
		start += p.size
	}

	for off := 0; off < p.size; off++ {
		i := (start + off) % p.size
		h, err := p.rc.HGetAll(ctx, p.slotKey(i)).Result()
		if err != nil || len(h) == 0 {
			continue
		}
		if h["status"] != "ok" || h["cookie"] == "" {
			continue
		}
		return SearchToken{Cookie: h["cookie"], UserAgent: h["ua"], Slot: i}, nil
	}

	// Фолбэк: пул пуст/битый — пробуем legacy-зеркало (его пишет майнер).
	if cookie, err := p.rc.Get(ctx, p.legacyCookieKey).Result(); err == nil && cookie != "" {
		ua, _ := p.rc.Get(ctx, p.legacyUAKey).Result()
		return SearchToken{Cookie: cookie, UserAgent: ua, Slot: -1}, nil
	}

	return SearchToken{}, fmt.Errorf("no healthy wb search token in pool (size %d)", p.size)
}

// MarkBad — 429 на слоте. После bad429Threshold подряд помечает слот broken.
func (p *RedisSearchTokenPool) MarkBad(ctx context.Context, slot int) {
	if slot < 0 || p.rc == nil {
		return
	}
	p.mu.Lock()
	p.fails[slot]++
	n := p.fails[slot]
	if n < bad429Threshold {
		p.mu.Unlock()
		return
	}
	p.fails[slot] = 0
	p.mu.Unlock()

	if err := p.rc.HSet(ctx, p.slotKey(slot),
		"status", "broken",
		"broken_at", time.Now().Unix(),
	).Err(); err != nil {
		p.log.Warn("token pool: mark broken failed", "slot", slot, "err", err)
		return
	}
	p.log.Warn("token pool: slot marked broken after consecutive 429",
		"slot", slot, "threshold", bad429Threshold)
}

// MarkGood — успешный запрос: сбрасываем счётчик подряд идущих 429 для слота.
func (p *RedisSearchTokenPool) MarkGood(_ context.Context, slot int) {
	if slot < 0 {
		return
	}
	p.mu.Lock()
	if p.fails[slot] != 0 {
		p.fails[slot] = 0
	}
	p.mu.Unlock()
}
