-- Миграция 030: (1) чиним враньё в products.marketplace, (2) склеиваем дубли
-- товаров, заведённые разными написаниями одного URL.
--
-- Прод (PG-порт закрыт, goose снаружи не ходит — см. грабли хардинга):
--   docker exec -i pt_postgres psql -U tryberry_app -d tryberrybot -v ON_ERROR_STOP=1 \
--     < migrations/030_product_marketplace_and_dupes.sql
--
-- Идемпотентна: повторный прогон ничего не найдёт и ничего не сделает.
-- Разрушительна ровно в одном месте (удаление строк-дублей) — поэтому вся
-- работа в ОДНОЙ транзакции и с отчётом до/после.

BEGIN;

-- ── 1. products.marketplace: ярлык вместо правды ─────────────────────────────
--
-- cmd/search-worker/searchloop.go при апсерте выдачи ставил КОНСТАНТУ
-- "wildberries" всем товарам из любого поиска → карточки Я.Маркета лежали с
-- ярлыком WB (на 2026-07-16 таких 3108). Скрейп это переживал (registry.Scrape
-- выбирает скрейпер по URL, а не по колонке), но юзеру ямаркетовский товар
-- показывался вайлдберрисовским. Код исправлен там же; здесь чиним данные.
--
-- Правда — в URL: он единственный источник, которому мы верим (по нему же
-- реально выбирается скрейпер). Хосты берём из Matches() соответствующих
-- скрейперов, чтобы не разъехаться с Go.

\echo '== marketplace до починки =='
SELECT marketplace, count(*) FROM products GROUP BY marketplace ORDER BY 1;

UPDATE products SET marketplace = 'yandex_market'
 WHERE url LIKE '%market.yandex.ru/%' AND marketplace <> 'yandex_market';

UPDATE products SET marketplace = 'ozon'
 WHERE url LIKE '%ozon.ru/product/%' AND marketplace <> 'ozon';

UPDATE products SET marketplace = 'aliexpress'
 WHERE (url LIKE '%aliexpress.ru/item/%' OR url LIKE '%aliexpress.com/item/%')
   AND marketplace <> 'aliexpress';

UPDATE products SET marketplace = 'wildberries'
 WHERE url LIKE '%wildberries.ru/catalog/%' AND marketplace <> 'wildberries';

\echo '== marketplace после починки =='
SELECT marketplace, count(*) FROM products GROUP BY marketplace ORDER BY 1;

\echo '== товары, чей URL не опознан ни одним маркетплейсом (ярлык оставлен как был) =='
SELECT id, marketplace, left(url, 60) AS url
FROM products
WHERE url NOT LIKE '%wildberries.ru/catalog/%'
  AND url NOT LIKE '%ozon.ru/product/%'
  AND url NOT LIKE '%market.yandex.ru/%'
  AND url NOT LIKE '%aliexpress.ru/item/%'
  AND url NOT LIKE '%aliexpress.com/item/%';

-- ── 2. Склейка дублей ────────────────────────────────────────────────────────
--
-- products.url UNIQUE = де-факто ключ товара, а одна карточка приходит десятком
-- написаний → один товар превращался в несколько products, каждый со своей
-- историей, подписками и public_id. Код (internal/scraper/canonical_url.go)
-- теперь пишет канон; здесь схлопываем то, что уже накопилось.
--
-- Я.Маркет НЕ трогаем: он единственный реально фетчит сохранённый URL и достаёт
-- из его пути sku — канона для него пока нет (см. canonical_url.go).
--
-- Победитель группы — СТАРЕЙШИЙ (min(id)): у него настоящий created_at
-- («наблюдаем с»), самая длинная история и уже разошедшийся по ссылкам public_id.

CREATE TEMP TABLE dupe_map ON COMMIT DROP AS
WITH keyed AS (
    SELECT id, marketplace,
           CASE marketplace
               WHEN 'wildberries' THEN substring(url from '/catalog/(\d+)/')
               WHEN 'ozon'        THEN substring(url from 'ozon\.ru/product/(?:[^/?#]*-)?(\d+)')
               WHEN 'aliexpress'  THEN substring(url from '/item/(\d+)\.html')
           END AS ext_id
    FROM products
    WHERE marketplace IN ('wildberries', 'ozon', 'aliexpress')
)
SELECT k.id AS loser_id,
       min(k.id) OVER (PARTITION BY k.marketplace, k.ext_id) AS keeper_id,
       k.marketplace, k.ext_id
FROM keyed k
WHERE k.ext_id IS NOT NULL;

DELETE FROM dupe_map WHERE loser_id = keeper_id;   -- в карте только проигравшие

\echo '== что склеиваем =='
SELECT marketplace, count(*) AS losers, count(DISTINCT keeper_id) AS groups FROM dupe_map GROUP BY 1;

-- Порядок ниже продиктован ограничениями, а не вкусом:
--
-- 2a. subscriptions: UNIQUE(user_id, product_id). Если юзер подписан и на
--     победителя, и на проигравшего — переносить нельзя, строка-дубль
--     удаляется. Но notifications ссылается на subscriptions БЕЗ каскада, так
--     что сперва переподцепляем уведомления к выжившей подписке, иначе DELETE
--     упадёт по FK.
UPDATE notifications n SET subscription_id = keep.id
  FROM subscriptions dup
  JOIN dupe_map m ON m.loser_id = dup.product_id
  JOIN subscriptions keep ON keep.user_id = dup.user_id AND keep.product_id = m.keeper_id
 WHERE n.subscription_id = dup.id;

DELETE FROM subscriptions dup
 USING dupe_map m, subscriptions keep
 WHERE dup.product_id = m.loser_id
   AND keep.user_id = dup.user_id
   AND keep.product_id = m.keeper_id;

--     Остальные (юзер подписан только на дубль) просто переезжают.
UPDATE subscriptions s SET product_id = m.keeper_id
  FROM dupe_map m WHERE s.product_id = m.loser_id;

-- 2b. price_history: уникальности нет — просто переносим. Партиции по
--     recorded_at, product_id не в ключе партиционирования → UPDATE безопасен.
UPDATE price_history h SET product_id = m.keeper_id
  FROM dupe_map m WHERE h.product_id = m.loser_id;

-- 2c. search_results: PK (search_query_id, product_id) — при коллизии
--     оставляем строку победителя, строку дубля выкидываем.
DELETE FROM search_results r
 USING dupe_map m, search_results keep
 WHERE r.product_id = m.loser_id
   AND keep.search_query_id = r.search_query_id
   AND keep.product_id = m.keeper_id;

UPDATE search_results r SET product_id = m.keeper_id
  FROM dupe_map m WHERE r.product_id = m.loser_id;

-- 2d. search_subscription_products: PK (subscription_id, product_id) — та же
--     логика. Baseline победителя старше — он и остаётся.
DELETE FROM search_subscription_products p
 USING dupe_map m, search_subscription_products keep
 WHERE p.product_id = m.loser_id
   AND keep.subscription_id = p.subscription_id
   AND keep.product_id = m.keeper_id;

UPDATE search_subscription_products p SET product_id = m.keeper_id
  FROM dupe_map m WHERE p.product_id = m.loser_id;

-- 2e. search_notifications: уникальности нет — переносим.
UPDATE search_notifications n SET product_id = m.keeper_id
  FROM dupe_map m WHERE n.product_id = m.loser_id;

-- 2f. Дубли больше никем не удерживаются.
DELETE FROM products p USING dupe_map m WHERE p.id = m.loser_id;

-- ── 3. Победителям — канонический URL ────────────────────────────────────────
-- Ровно те формы, что строит CanonicalProductURL в Go. Делается ПОСЛЕ удаления
-- проигравших: канон мог принадлежать как раз дублю (url UNIQUE).
UPDATE products SET url = 'https://www.wildberries.ru/catalog/'
                        || substring(url from '/catalog/(\d+)/') || '/detail.aspx'
 WHERE marketplace = 'wildberries'
   AND substring(url from '/catalog/(\d+)/') IS NOT NULL
   AND url <> 'https://www.wildberries.ru/catalog/'
              || substring(url from '/catalog/(\d+)/') || '/detail.aspx';

UPDATE products SET url = 'https://www.ozon.ru/product/'
                        || substring(url from 'ozon\.ru/product/(?:[^/?#]*-)?(\d+)') || '/'
 WHERE marketplace = 'ozon'
   AND substring(url from 'ozon\.ru/product/(?:[^/?#]*-)?(\d+)') IS NOT NULL
   AND url <> 'https://www.ozon.ru/product/'
              || substring(url from 'ozon\.ru/product/(?:[^/?#]*-)?(\d+)') || '/';

UPDATE products SET url = 'https://aliexpress.ru/item/'
                        || substring(url from '/item/(\d+)\.html') || '.html'
 WHERE marketplace = 'aliexpress'
   AND substring(url from '/item/(\d+)\.html') IS NOT NULL
   AND url <> 'https://aliexpress.ru/item/'
              || substring(url from '/item/(\d+)\.html') || '.html';

\echo '== контроль: дублей не осталось =='
WITH keyed AS (
    SELECT marketplace,
           CASE marketplace
               WHEN 'wildberries' THEN substring(url from '/catalog/(\d+)/')
               WHEN 'ozon'        THEN substring(url from 'ozon\.ru/product/(?:[^/?#]*-)?(\d+)')
               WHEN 'aliexpress'  THEN substring(url from '/item/(\d+)\.html')
           END AS ext_id
    FROM products WHERE marketplace IN ('wildberries', 'ozon', 'aliexpress')
)
SELECT marketplace, count(*) AS still_dupes
FROM (SELECT marketplace, ext_id FROM keyed WHERE ext_id IS NOT NULL
      GROUP BY 1, 2 HAVING count(*) > 1) x
GROUP BY 1;

COMMIT;
