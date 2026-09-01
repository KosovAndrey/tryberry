package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type ProductRepo struct {
	db *pgxpool.Pool
}

func NewProductRepo(db *pgxpool.Pool) *ProductRepo {
	return &ProductRepo{db: db}
}

// ProductUpsert — одна строка для батч-апсерта товаров.
type ProductUpsert struct {
	URL, Name, ImageURL, Marketplace string
	// DisplayURL — ссылка для показа, когда она отличается от канона URL
	// (Я.Маркет: канон без слага, показываем со слагом). Пусто = показываем URL.
	DisplayURL string
}

// UpsertBatch апсертит много товаров за ОДИН round-trip (pgx.Batch) и возвращает
// id по URL. Для поиск-выдачи (сотни item'ов на скрейп) — вместо N отдельных
// Upsert. Дубли URL во входе схлопываются (один queue на URL).
func (r *ProductRepo) UpsertBatch(ctx context.Context, items []ProductUpsert) (map[string]int64, error) {
	out := make(map[string]int64, len(items))
	if len(items) == 0 {
		return out, nil
	}
	// дедуп по URL: последний выигрывает (свежие name/image)
	uniq := make(map[string]ProductUpsert, len(items))
	order := make([]string, 0, len(items))
	for _, it := range items {
		if _, ok := uniq[it.URL]; !ok {
			order = append(order, it.URL)
		}
		uniq[it.URL] = it
	}

	// display_url обновляем только непустым: скрейпер карточки его не знает, и
	// пустое значение не должно затирать адрес, добытый из выдачи.
	const q = `
		INSERT INTO products (url, name, image_url, marketplace, display_url)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''))
		ON CONFLICT (url) DO UPDATE
			SET name        = EXCLUDED.name,
			    image_url   = EXCLUDED.image_url,
			    marketplace = EXCLUDED.marketplace,
			    display_url = COALESCE(EXCLUDED.display_url, products.display_url),
			    updated_at  = NOW()
		RETURNING id, url`

	b := &pgx.Batch{}
	for _, u := range order {
		it := uniq[u]
		b.Queue(q, it.URL, it.Name, it.ImageURL, it.Marketplace, it.DisplayURL)
	}
	br := r.db.SendBatch(ctx, b)
	defer br.Close()
	for range order {
		var id int64
		var url string
		if err := br.QueryRow().Scan(&id, &url); err != nil {
			return nil, err
		}
		out[url] = id
	}
	return out, nil
}

// Upsert — апсерт одного товара. displayURL — ссылка для показа, когда она
// отличается от канона url (Я.Маркет: канон без слага); пусто не затирает уже
// сохранённое значение. Возвращённый Product.URL — ссылка ДЛЯ ПОКАЗА
// (display_url, если есть), а не канон: вызывающие показывают её пользователю
// сразу после /track.
func (r *ProductRepo) Upsert(ctx context.Context, url, name, imageURL, marketplace, displayURL string) (*domain.Product, error) {
	const q = `
		INSERT INTO products (url, name, image_url, marketplace, display_url)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''))
		ON CONFLICT (url) DO UPDATE
			SET name        = EXCLUDED.name,
			    image_url   = EXCLUDED.image_url,
			    marketplace = EXCLUDED.marketplace,
			    display_url = COALESCE(EXCLUDED.display_url, products.display_url),
			    updated_at  = NOW()
		RETURNING id, public_id, COALESCE(display_url, url), name, image_url, marketplace, created_at, updated_at`

	p := &domain.Product{}
	err := withSpan(ctx, "upsert_product", func(ctx context.Context) error {
		return r.db.QueryRow(ctx, q, url, name, imageURL, marketplace, displayURL).
			Scan(&p.ID, &p.PublicID, &p.URL, &p.Name, &p.ImageURL, &p.Marketplace, &p.CreatedAt, &p.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (r *ProductRepo) GetByID(ctx context.Context, id int64) (*domain.Product, error) {
	// URL отдаём для ПОКАЗА (display_url, если есть). Скрейп-путь берёт канон
	// отдельным запросом (см. очередь в GetDueForScrape).
	const q = `
		SELECT id, public_id, COALESCE(display_url, url), name, image_url, marketplace, in_stock, created_at, updated_at
		FROM products WHERE id = $1`

	p := &domain.Product{}
	err := r.db.QueryRow(ctx, q, id).
		Scan(&p.ID, &p.PublicID, &p.URL, &p.Name, &p.ImageURL, &p.Marketplace, &p.InStock, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// FindByURL — карточка по каноническому URL, без создания записи (в отличие от
// Upsert). Нужна, когда площадка отказала и скрейпа нет: показать пользователю
// то, что мы уже знаем, вместо «не удалось получить данные».
// ok=false — товара в базе нет, показывать нечего.
func (r *ProductRepo) FindByURL(ctx context.Context, url string) (*domain.Product, bool, error) {
	const q = `
		SELECT id, public_id, url, name, image_url, marketplace, in_stock, created_at, updated_at
		FROM products WHERE url = $1`

	p := &domain.Product{}
	err := r.db.QueryRow(ctx, q, url).
		Scan(&p.ID, &p.PublicID, &p.URL, &p.Name, &p.ImageURL, &p.Marketplace, &p.InStock, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return p, true, nil
}

// GetByPublicID — резолв товара по публичному токену (страница графика /p/<public_id>).
// Read-only путь для сервиса api; отдаёт и in_stock для блока «снова в наличии».
func (r *ProductRepo) GetByPublicID(ctx context.Context, publicID string) (*domain.Product, bool, error) {
	const q = `
		SELECT id, public_id, url, name, image_url, marketplace, in_stock, created_at, updated_at
		FROM products WHERE public_id = $1`

	p := &domain.Product{}
	var inStock bool
	err := r.db.QueryRow(ctx, q, publicID).
		Scan(&p.ID, &p.PublicID, &p.URL, &p.Name, &p.ImageURL, &p.Marketplace, &inStock, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, domain.ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	return p, inStock, nil
}

// SitemapEntry — строка для sitemap.xml публичных страниц графиков.
type SitemapEntry struct {
	PublicID  string
	Name      string
	UpdatedAt time.Time
}

// ListPublicForSitemap — товары, у которых есть история цены (значит странице
// графика есть что показать). Кап limit защищает размер sitemap; при росте
// каталога переведём на пагинацию (sitemap index).
func (r *ProductRepo) ListPublicForSitemap(ctx context.Context, limit int) ([]SitemapEntry, error) {
	const q = `
		SELECT p.public_id, p.name, p.updated_at
		FROM products p
		WHERE EXISTS (SELECT 1 FROM price_history ph WHERE ph.product_id = p.id)
		ORDER BY p.updated_at DESC
		LIMIT $1`

	rows, err := r.db.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SitemapEntry
	for rows.Next() {
		var e SitemapEntry
		if err := rows.Scan(&e.PublicID, &e.Name, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateScrapedData обновляет имя/картинку/наличие товара и возвращает ПРЕДЫДУЩЕЕ
// значение in_stock — notifier по переходу wasInStock(false)→inStock(true) шлёт
// уведомление «снова в наличии» (триггер back_in_stock).
//
// Имя и картинку НЕ затираем заглушкой: если скрейп нашёл цену, но не вытащил
// заголовок/фото (вёрстка маркетплейса плавает — web/mobile, частичный ответ),
// scraper подставляет заглушку «Товар <маркетплейс>» / пустую картинку. Раньше
// она безусловно перезаписывала уже сохранённое нормальное имя — товар навсегда
// превращался в «Товар Ozon». Теперь нормальное имя сохраняем, заглушку пишем
// только если хорошего имени ещё нет. Все заглушки имеют форму 'Товар <…>'.
// inStock == nil означает «источник наличия не знает» (архив WB): сохранённое
// значение остаётся как есть, а вернётся оно же — вызывающий берёт его как
// действующее. Иначе архивный путь объявлял бы «в наличии» пропавший товар.
func (r *ProductRepo) UpdateScrapedData(ctx context.Context, id int64, name, imageURL string, inStock *bool) (wasInStock bool, err error) {
	const q = `
		WITH prev AS (SELECT in_stock FROM products WHERE id = $1)
		UPDATE products
		SET name = CASE
		             WHEN $2 LIKE 'Товар %' AND name <> '' AND name NOT LIKE 'Товар %'
		               THEN name
		             ELSE $2
		           END,
		    image_url = COALESCE(NULLIF($3, ''), image_url),
		    in_stock  = COALESCE($4, in_stock),
		    updated_at = NOW()
		WHERE id = $1
		RETURNING (SELECT in_stock FROM prev)`

	err = r.db.QueryRow(ctx, q, id, name, imageURL, inStock).Scan(&wasInStock)
	return wasInStock, err
}

// SetInStock проставляет наличие товара. Используется ботом при добавлении
// карточки без оффера: фиксируем in_stock=false, чтобы последующий скрейп с ценой
// дал переход false→true и сработал триггер back_in_stock.
func (r *ProductRepo) SetInStock(ctx context.Context, id int64, inStock bool) error {
	const q = `UPDATE products SET in_stock = $2, updated_at = NOW() WHERE id = $1`
	_, err := r.db.Exec(ctx, q, id, inStock)
	return err
}

func (r *ProductRepo) GetActiveProductIDs(ctx context.Context) ([]int64, error) {
	const q = `
		SELECT DISTINCT product_id
		FROM subscriptions
		WHERE active = TRUE`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SchedulableProduct — строка планирования: товар + один его активный подписчик
// (с тарифом владельца). Планировщик группирует по ProductID и считает
// минимальный эффективный интервал среди подписчиков (источник истины —
// domain.Plans), решая, пора ли скрейпить. Зеркало SearchQueryRepo.GetSchedulable.
type SchedulableProduct struct {
	ProductID      int64
	URL            string
	LastEnqueuedAt *time.Time
	OwnerPlan      string
	PlanExpiresAt  *time.Time
	// LastPriceChangeAt — когда цена менялась в последний раз (NULL = новый
	// трек); питает волатильностный бэкофф в планировщике (domain.VolatilityMult).
	LastPriceChangeAt *time.Time
}

// GetSchedulableProducts — по строке на каждую активную товарную подписку: товар
// + план владельца. MIN-интервал и решение «пора» планировщик считает в Go.
func (r *ProductRepo) GetSchedulableProducts(ctx context.Context) ([]SchedulableProduct, error) {
	const q = `
		SELECT p.id, p.url, p.last_enqueued_at, u.plan, u.plan_expires_at, p.last_price_change_at
		FROM products p
		JOIN subscriptions s ON s.product_id = p.id AND s.active = TRUE
		JOIN users u ON u.id = s.user_id`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SchedulableProduct
	for rows.Next() {
		var p SchedulableProduct
		if err := rows.Scan(&p.ProductID, &p.URL, &p.LastEnqueuedAt, &p.OwnerPlan, &p.PlanExpiresAt, &p.LastPriceChangeAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TouchPriceChanged — зафиксировать смену цены товара (сбрасывает
// волатильностный бэкофф в ×1). Дёргает scraper-worker рядом с change-only
// INSERT в price_history.
func (r *ProductRepo) TouchPriceChanged(ctx context.Context, productID int64) error {
	const q = `UPDATE products SET last_price_change_at = NOW() WHERE id = $1`
	_, err := r.db.Exec(ctx, q, productID)
	return err
}

// ClaimEnqueued — отметить товары поставленными в очередь (last_enqueued_at=NOW).
// Планировщик-синглтон делает это перед эмиссией в Kafka.
func (r *ProductRepo) ClaimEnqueued(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `UPDATE products SET last_enqueued_at = NOW() WHERE id = ANY($1)`
	_, err := r.db.Exec(ctx, q, ids)
	return err
}
