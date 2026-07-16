-- Отчёт по дублям товаров: одна карточка заведена под несколькими products,
-- потому что products.url (UNIQUE) — де-факто ключ, а один товар приходит
-- десятком написаний ссылки. ТОЛЬКО ЧТЕНИЕ, ничего не меняет.
--
-- Запуск на проде (PG-порт закрыт, изнутри):
--   docker exec -i pt_postgres psql -U tryberry_app -d tryberrybot \
--     < scripts/sql/product-dupes-report.sql
--
-- Канон = id товара у маркетплейса. Регулярки ОБЯЗАНЫ совпадать с Go
-- (ExtractArticleID / ozonProductRe / aliItemRe в internal/scraper) — иначе
-- отчёт покажет не те группы, что склеит миграция.
--
-- yandex_market разбирается с 2026-07-16: канон /product/<id> проверен живым
-- скрейпером (cmd/ym-canon-probe, 8 из 8 совпали). Раньше он тут отсутствовал —
-- и именно поэтому его ~1057 дублей (у одного товара 12 копий) никто не видел,
-- пока отчёт бодро показывал «одна группа дублей» по Ozon.

\echo '== 1. Сколько групп дублей и сколько лишних строк =='

WITH keyed AS (
    SELECT id,
           marketplace,
           CASE marketplace
               WHEN 'wildberries' THEN substring(url from '/catalog/(\d+)/')
               WHEN 'ozon'        THEN substring(url from 'ozon\.ru/product/(?:[^/?#]*-)?(\d+)')
               WHEN 'aliexpress'  THEN substring(url from '/item/(\d+)\.html')
               WHEN 'yandex_market' THEN substring(url from 'market\.yandex\.ru/product(?:--[^/?#]*)?/(\d+)')
           END AS ext_id
    FROM products
    WHERE marketplace IN ('wildberries', 'ozon', 'aliexpress', 'yandex_market')
),
groups AS (
    SELECT marketplace, ext_id, count(*) AS n
    FROM keyed
    WHERE ext_id IS NOT NULL
    GROUP BY marketplace, ext_id
    HAVING count(*) > 1
)
SELECT marketplace,
       count(*)         AS dupe_groups,
       sum(n)           AS rows_total,
       sum(n) - count(*) AS rows_to_merge
FROM groups
GROUP BY marketplace
ORDER BY marketplace;

\echo ''
\echo '== 2. Товары, у которых url не распарсился (канон их НЕ тронет) =='

SELECT marketplace, count(*) AS unparsed
FROM products
WHERE marketplace IN ('wildberries', 'ozon', 'aliexpress', 'yandex_market')
  AND CASE marketplace
          WHEN 'wildberries' THEN substring(url from '/catalog/(\d+)/')
          WHEN 'ozon'        THEN substring(url from 'ozon\.ru/product/(?:[^/?#]*-)?(\d+)')
          WHEN 'aliexpress'  THEN substring(url from '/item/(\d+)\.html')
      END IS NULL
GROUP BY marketplace
ORDER BY marketplace;

\echo ''
\echo '== 3. Что реально прицеплено к дублям (это и решает, нужна ли склейка) =='
\echo '   Строки с subs/history у НЕ-старейшего — та самая потерянная история.'

WITH keyed AS (
    SELECT id, marketplace, url, created_at,
           CASE marketplace
               WHEN 'wildberries' THEN substring(url from '/catalog/(\d+)/')
               WHEN 'ozon'        THEN substring(url from 'ozon\.ru/product/(?:[^/?#]*-)?(\d+)')
               WHEN 'aliexpress'  THEN substring(url from '/item/(\d+)\.html')
               WHEN 'yandex_market' THEN substring(url from 'market\.yandex\.ru/product(?:--[^/?#]*)?/(\d+)')
           END AS ext_id
    FROM products
    WHERE marketplace IN ('wildberries', 'ozon', 'aliexpress', 'yandex_market')
),
dupes AS (
    SELECT k.*,
           min(k.id) OVER (PARTITION BY k.marketplace, k.ext_id) AS keeper_id,
           count(*)  OVER (PARTITION BY k.marketplace, k.ext_id) AS grp
    FROM keyed k
    WHERE k.ext_id IS NOT NULL
)
SELECT d.marketplace,
       d.ext_id,
       d.id,
       (d.id = d.keeper_id)                                   AS is_keeper,
       (SELECT count(*) FROM subscriptions s WHERE s.product_id = d.id)  AS subs,
       (SELECT count(*) FROM price_history h WHERE h.product_id = d.id)  AS history,
       (SELECT count(*) FROM search_results r WHERE r.product_id = d.id) AS in_search,
       d.created_at::date AS created,
       left(d.url, 70)    AS url
FROM dupes d
WHERE d.grp > 1
ORDER BY d.marketplace, d.ext_id, d.id
LIMIT 200;

\echo ''
\echo '== 4. Коллизии, которые склейка обязана расшить, а не упасть на них =='
\echo '   Это пары, где ОДИН юзер подписан на ОБА дубля: склеить в лоб нельзя'
\echo '   (UNIQUE(user_id, product_id)), одну подписку придётся убить.'

WITH keyed AS (
    SELECT id, marketplace,
           CASE marketplace
               WHEN 'wildberries' THEN substring(url from '/catalog/(\d+)/')
               WHEN 'ozon'        THEN substring(url from 'ozon\.ru/product/(?:[^/?#]*-)?(\d+)')
               WHEN 'aliexpress'  THEN substring(url from '/item/(\d+)\.html')
               WHEN 'yandex_market' THEN substring(url from 'market\.yandex\.ru/product(?:--[^/?#]*)?/(\d+)')
           END AS ext_id
    FROM products
    WHERE marketplace IN ('wildberries', 'ozon', 'aliexpress', 'yandex_market')
)
SELECT k.marketplace, k.ext_id, s.user_id, count(*) AS subs_on_dupes
FROM keyed k
JOIN subscriptions s ON s.product_id = k.id
WHERE k.ext_id IS NOT NULL
GROUP BY k.marketplace, k.ext_id, s.user_id
HAVING count(*) > 1
ORDER BY subs_on_dupes DESC
LIMIT 50;
