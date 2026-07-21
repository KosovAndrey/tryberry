-- Подбор реальных товаров под сценарии батчей коротких видео.
-- Правило: цифры в роликах — только из БД, ничего не подрисовываем
-- (канон docs/PRESS-SHOTS-GUIDE.md).
--
-- Запуск на проде (PG-порт закрыт снаружи, только изнутри):
--   docker compose exec -T postgres psql -U <user> -d <db> -f - < scripts/sql/shorts-candidates.sql
-- либо скопировать нужный блок в psql.

\echo '=== F1: кандидаты «фейковая скидка» — цена стоит ровно, скидка нарисована ==='
-- Ищем товары, у которых за 60 дней разброс цены мизерный (<5%),
-- но наблюдений много. На карточке такого товара ярлык «−N%» = вранье про
-- старую цену: её в этом окне не существовало. Проверить ярлык глазами перед съёмкой.
WITH w AS (
    SELECT product_id,
           COUNT(*)                        AS points,
           MIN(price)                       AS min_price,
           MAX(price)                       AS max_price,
           (array_agg(price ORDER BY recorded_at DESC))[1] AS last_price
    FROM price_history
    WHERE recorded_at > NOW() - INTERVAL '60 days'
    GROUP BY product_id
    HAVING COUNT(*) >= 100
)
SELECT p.id,
       p.marketplace,
       left(p.name, 60)                                    AS name,
       w.points,
       w.min_price,
       w.max_price,
       round((w.max_price - w.min_price) / NULLIF(w.min_price, 0) * 100, 1) AS spread_pct,
       'https://tryberry.ru/p/' || p.public_id             AS chart_url,
       p.url
FROM w
         JOIN products p ON p.id = w.product_id
WHERE (w.max_price - w.min_price) / NULLIF(w.min_price, 0) < 0.05
ORDER BY w.points DESC
LIMIT 25;

\echo '=== F2: кандидаты «цена реально упала» — чем ловим алерт ==='
-- Максимальная просадка от 60-дневного максимума к текущей цене, в рублях.
-- Берём в ролик те, где падение и в % заметное, и в ₽ звучит весомо.
WITH w AS (
    SELECT product_id,
           MAX(price)                                       AS max_price,
           (array_agg(price ORDER BY recorded_at DESC))[1]   AS last_price,
           COUNT(*)                                         AS points
    FROM price_history
    WHERE recorded_at > NOW() - INTERVAL '60 days'
    GROUP BY product_id
    HAVING COUNT(*) >= 50
)
SELECT p.id,
       p.marketplace,
       left(p.name, 60)                          AS name,
       w.max_price,
       w.last_price,
       (w.max_price - w.last_price)              AS drop_rub,
       round((w.max_price - w.last_price) / NULLIF(w.max_price, 0) * 100, 1) AS drop_pct,
       'https://tryberry.ru/p/' || p.public_id   AS chart_url,
       p.url
FROM w
         JOIN products p ON p.id = w.product_id
WHERE w.last_price < w.max_price * 0.75
ORDER BY drop_rub DESC
LIMIT 25;

\echo '=== F1/G3: кандидаты «качели» — цена гуляет туда-сюда ==='
-- Считаем количество разворотов направления: чем больше, тем нагляднее
-- тезис «покупать надо не когда нужно, а когда дёшево».
WITH series AS (
    SELECT product_id,
           price,
           lag(price) OVER (PARTITION BY product_id ORDER BY recorded_at)  AS prev,
           lag(price, 2) OVER (PARTITION BY product_id ORDER BY recorded_at) AS prev2
    FROM price_history
    WHERE recorded_at > NOW() - INTERVAL '90 days'
),
     turns AS (
         SELECT product_id, COUNT(*) AS reversals
         FROM series
         WHERE prev2 IS NOT NULL
           AND sign(price - prev) <> 0
           AND sign(price - prev) <> sign(prev - prev2)
         GROUP BY product_id
     )
SELECT p.id,
       p.marketplace,
       left(p.name, 60)                        AS name,
       t.reversals,
       'https://tryberry.ru/p/' || p.public_id AS chart_url,
       p.url
FROM turns t
         JOIN products p ON p.id = t.product_id
WHERE t.reversals >= 8
ORDER BY t.reversals DESC
LIMIT 25;

\echo '=== Разбивка кандидатов по маркетплейсам (следим, чтобы батч не был только про WB) ==='
SELECT marketplace, COUNT(*) AS products
FROM products
GROUP BY marketplace
ORDER BY products DESC;
