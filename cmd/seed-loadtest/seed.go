package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.com/KosovAndrey/tryberrybot/internal/config"
	"gitlab.com/KosovAndrey/tryberrybot/internal/db"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
)

// synthTGBase — база фейковых идентичностей синтетиков. Далеко за пределами
// реальных id Telegram/VK/MAX (влезает в BIGINT), коллизии исключены.
const synthTGBase = int64(9_100_000_000_000_000_000)

// planShare / identShare — распределение синтетиков по тарифам и каналам.
// Тарифы задают каданс скрейпа (free 60м / lite 30м / pro 15м) и лимиты товаров.
var planShare = []struct {
	plan   string
	weight int
	minSub int // товаров на юзера: [minSub, maxSub]
	maxSub int
	paid   bool // plan_expires_at = +30 дней
}{
	{"free", 75, 1, 5, false},
	{"lite", 15, 2, 15, true},
	{"pro", 10, 3, 50, true},
}

var identShare = []struct {
	tg, vk, max bool
	weight      int
}{
	{true, false, false, 60}, // tg-only
	{false, true, false, 20}, // vk-only
	{false, false, true, 10}, // max-only
	{true, true, false, 7},   // tg+vk
	{true, false, true, 3},   // tg+max
}

// runSeed — создать синтетических юзеров и раздать им товары по Zipf: все
// «трекают» одни и те же топовые позиции, хвост достаётся немногим — как в жизни.
// Товары апсертятся (продукт может уже существовать — тогда просто добавляются
// подписчики), первая точка price_history пишется из collect-цены.
func runSeed(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	inPath := fs.String("in", "products.jsonl", "JSONL из collect")
	users := fs.Int("users", 1000, "сколько синтетических юзеров создать")
	zipfS := fs.Float64("zipf", 1.0, "показатель Zipf-распределения популярности")
	randSeed := fs.Int64("rand-seed", 42, "seed ГПСЧ (воспроизводимость раздачи)")
	yes := fs.Bool("yes", false, "подтверждение записи в БД")
	if err := fs.Parse(args); err != nil {
		return err
	}

	products, err := readProducts(*inPath)
	if err != nil {
		return err
	}
	if len(products) == 0 {
		return fmt.Errorf("нет товаров в %s — сначала collect", *inPath)
	}
	if !*yes {
		return fmt.Errorf("dry-run: %d товаров, %d юзеров; добавь -yes для записи", len(products), *users)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := db.NewPostgresPool(ctx, config.MustEnv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	var existing int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE is_synthetic`).Scan(&existing); err != nil {
		return fmt.Errorf("count synthetic: %w", err)
	}
	if existing > 0 {
		return fmt.Errorf("в БД уже %d синтетиков — сначала cleanup (повторный seed навалит дублей)", existing)
	}

	rng := rand.New(rand.NewSource(*randSeed))

	// ── Товары: батч-апсерт + первая точка истории ────────────────────────────
	items := make([]postgres.ProductUpsert, 0, len(products))
	for _, p := range products {
		name := p.Name
		if name == "" {
			name = p.URL
		}
		items = append(items, postgres.ProductUpsert{
			URL: p.URL, Name: name, ImageURL: p.ImageURL, Marketplace: p.Marketplace,
		})
	}
	productRepo := postgres.NewProductRepo(pool)
	idByURL, err := productRepo.UpsertBatch(ctx, items)
	if err != nil {
		return fmt.Errorf("upsert products: %w", err)
	}

	histBatch := &pgx.Batch{}
	for _, p := range products {
		// Точка только товарам без истории — не портим серию уже трекаемых.
		histBatch.Queue(`
			INSERT INTO price_history (product_id, price, recorded_at)
			SELECT $1, $2, NOW()
			WHERE NOT EXISTS (SELECT 1 FROM price_history WHERE product_id = $1)`,
			idByURL[p.URL], float64(p.PriceKopecks)/100)
	}
	if err := flushBatch(ctx, pool, histBatch); err != nil {
		return fmt.Errorf("price history: %w", err)
	}

	// ── Zipf-популярность: перемешиваем товары, вес 1/rank^s ─────────────────
	order := rng.Perm(len(products))
	cum := make([]float64, len(products)) // кумулятивные веса по order
	sum := 0.0
	for i := range order {
		sum += 1 / math.Pow(float64(i+1), *zipfS)
		cum[i] = sum
	}
	pickProduct := func() int {
		x := rng.Float64() * sum
		return order[sort.SearchFloat64s(cum, x)]
	}

	// ── Юзеры + подписки ──────────────────────────────────────────────────────
	userRows := 0
	subRows := 0
	expires := time.Now().Add(30 * 24 * time.Hour)
	for i := 0; i < *users; i++ {
		plan := pickWeighted(rng, planShareWeights())
		ident := pickWeighted(rng, identShareWeights())
		p, id := planShare[plan], identShare[ident]

		var tgID, vkID, maxID *int64
		fake := synthTGBase + int64(i)
		if id.tg {
			tgID = &fake
		}
		if id.vk {
			vkID = &fake
		}
		if id.max {
			maxID = &fake
		}
		var exp *time.Time
		if p.paid {
			exp = &expires
		}

		var userID int64
		err := pool.QueryRow(ctx, `
			INSERT INTO users (telegram_id, vk_id, max_id, username, plan, plan_expires_at,
			                   notify_channel, pd_consent_at, is_synthetic)
			VALUES ($1, $2, $3, $4, $5, $6, 'auto', NOW(), TRUE)
			RETURNING id`,
			tgID, vkID, maxID, fmt.Sprintf("synth_%05d", i), p.plan, exp).Scan(&userID)
		if err != nil {
			return fmt.Errorf("insert user %d: %w", i, err)
		}
		userRows++

		nSubs := p.minSub + rng.Intn(p.maxSub-p.minSub+1)
		chosen := map[int]bool{}
		subBatch := &pgx.Batch{}
		for len(chosen) < nSubs && len(chosen) < len(products) {
			pi := pickProduct()
			if chosen[pi] {
				continue // популярные дублируются часто — просто перетягиваем
			}
			chosen[pi] = true
			prod := products[pi]
			// first_seen_price обязателен: any_drop в первой фазе сравнивает цену
			// именно с ним (searchsub.Decide), дефолтный 0 = триггер никогда не
			// сработает и алертов не будет вовсе.
			subBatch.Queue(`
				INSERT INTO subscriptions (user_id, product_id, baseline_price, first_seen_price, active)
				VALUES ($1, $2, $3, $3, TRUE)
				ON CONFLICT (user_id, product_id) DO NOTHING`,
				userID, idByURL[prod.URL], float64(prod.PriceKopecks)/100)
		}
		if err := flushBatch(ctx, pool, subBatch); err != nil {
			return fmt.Errorf("subscriptions user %d: %w", i, err)
		}
		subRows += len(chosen)
	}

	log.Info("seed finished",
		"users", userRows, "subscriptions", subRows, "products", len(products))
	return nil
}

func planShareWeights() []int {
	w := make([]int, len(planShare))
	for i, p := range planShare {
		w[i] = p.weight
	}
	return w
}

func identShareWeights() []int {
	w := make([]int, len(identShare))
	for i, s := range identShare {
		w[i] = s.weight
	}
	return w
}

func pickWeighted(rng *rand.Rand, weights []int) int {
	total := 0
	for _, w := range weights {
		total += w
	}
	x := rng.Intn(total)
	for i, w := range weights {
		if x < w {
			return i
		}
		x -= w
	}
	return len(weights) - 1
}

type pgxPool interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

func flushBatch(ctx context.Context, pool pgxPool, b *pgx.Batch) error {
	if b.Len() == 0 {
		return nil
	}
	br := pool.SendBatch(ctx, b)
	defer br.Close()
	for i := 0; i < b.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

func readProducts(path string) ([]collectedProduct, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []collectedProduct
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for sc.Scan() {
		var p collectedProduct
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			return nil, fmt.Errorf("bad jsonl line: %w", err)
		}
		if p.URL == "" || p.PriceKopecks <= 0 || seen[p.URL] {
			continue
		}
		seen[p.URL] = true
		out = append(out, p)
	}
	return out, sc.Err()
}
