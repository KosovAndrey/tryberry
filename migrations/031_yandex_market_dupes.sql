-- Миграция 031: канон URL и склейка дублей для Я.Маркета.
--
-- ⚠️ ПОРЯДОК: СНАЧАЛА выкатить код (canonical_url.go знает YM), ПОТОМ эту
-- миграцию. Наоборот — нельзя: выдача YM активно апсертит ~14k товаров, и старый
-- код, не знающий канона, тут же заведёт их заново со слагами (на 030 так и
-- случилось: окно между миграцией и деплоем дало 96 кривых строк).
--
-- Прод (PG-порт закрыт, goose снаружи не ходит):
--   docker exec -i pt_postgres psql -U tryberry_app -d tryberrybot -v ON_ERROR_STOP=1 \
--     < migrations/031_yandex_market_dupes.sql
--
-- Идемпотентна. Разрушительна в одном месте (удаление строк-дублей) — поэтому
-- всё в ОДНОЙ транзакции.
--
-- Почему YM не вошёл в 030: он ЕДИНСТВЕННЫЙ реально фетчит сохранённый URL и
-- берёт из его пути sku (ymExtractSKU → ymStatePrice), поэтому канон нельзя было
-- вывести из общих соображений — его ПРОВЕРИЛИ живым скрейпером
-- (cmd/ym-canon-probe, 2026-07-16): 8 из 8 карточек с живой ценой дали на
-- /product/<id> ту же цену и то же имя, что на /product--<slug>/<id>.
--
-- Масштаб (замер 2026-07-16): 14524 строки на 13460 товаров → ~1057 лишних,
-- у одного товара 12 копий. Слаг гуляет. Для сравнения: у Ozon дубль был ОДИН,
-- т.е. настоящая проблема с дублями всё это время сидела там, куда отчёт не смотрел.

BEGIN;

CREATE TEMP TABLE ym_dupe_map ON COMMIT DROP AS
WITH keyed AS (
    -- Регулярка ОБЯЗАНА совпадать с ymProductRe/ExtractYandexMarketID в Go.
    SELECT id, substring(url from 'market\.yandex\.ru/product(?:--[^/?#]*)?/(\d+)') AS ext_id
    FROM products
    WHERE marketplace = 'yandex_market'
)
SELECT k.id AS loser_id,
       min(k.id) OVER (PARTITION BY k.ext_id) AS keeper_id,
       k.ext_id
FROM keyed k
WHERE k.ext_id IS NOT NULL;

DELETE FROM ym_dupe_map WHERE loser_id = keeper_id;   -- в карте только проигравшие

\echo '== что склеиваем =='
SELECT count(*) AS losers, count(DISTINCT keeper_id) AS groups FROM ym_dupe_map;

\echo '== коллизии: юзер подписан на оба дубля (одну подписку придётся убить) =='
SELECT count(*) AS collisions
FROM subscriptions dup
JOIN ym_dupe_map m ON m.loser_id = dup.product_id
JOIN subscriptions keep ON keep.user_id = dup.user_id AND keep.product_id = m.keeper_id;

-- Победитель группы — СТАРЕЙШИЙ (min(id)): у него настоящий created_at
-- («наблюдаем с»), самая длинная история и уже разошедшийся по ссылкам public_id.
--
-- Порядок продиктован ограничениями, а не вкусом (см. 030):
-- notifications висят на подписке БЕЗ каскада → уведомления переподцепляем к
-- выжившей подписке ДО удаления, иначе падение по FK.
UPDATE notifications n SET subscription_id = keep.id
  FROM subscriptions dup
  JOIN ym_dupe_map m ON m.loser_id = dup.product_id
  JOIN subscriptions keep ON keep.user_id = dup.user_id AND keep.product_id = m.keeper_id
 WHERE n.subscription_id = dup.id;

DELETE FROM subscriptions dup
 USING ym_dupe_map m, subscriptions keep
 WHERE dup.product_id = m.loser_id
   AND keep.user_id = dup.user_id
   AND keep.product_id = m.keeper_id;

UPDATE subscriptions s SET product_id = m.keeper_id
  FROM ym_dupe_map m WHERE s.product_id = m.loser_id;

-- price_history: уникальности нет, product_id не в ключе партиционирования.
UPDATE price_history h SET product_id = m.keeper_id
  FROM ym_dupe_map m WHERE h.product_id = m.loser_id;

-- search_results: PK (search_query_id, product_id) — при коллизии оставляем
-- строку победителя.
DELETE FROM search_results r
 USING ym_dupe_map m, search_results keep
 WHERE r.product_id = m.loser_id
   AND keep.search_query_id = r.search_query_id
   AND keep.product_id = m.keeper_id;

UPDATE search_results r SET product_id = m.keeper_id
  FROM ym_dupe_map m WHERE r.product_id = m.loser_id;

-- search_subscription_products: PK (subscription_id, product_id). Baseline
-- победителя старше — он и остаётся.
DELETE FROM search_subscription_products p
 USING ym_dupe_map m, search_subscription_products keep
 WHERE p.product_id = m.loser_id
   AND keep.subscription_id = p.subscription_id
   AND keep.product_id = m.keeper_id;

UPDATE search_subscription_products p SET product_id = m.keeper_id
  FROM ym_dupe_map m WHERE p.product_id = m.loser_id;

-- search_notifications: уникальности нет.
UPDATE search_notifications n SET product_id = m.keeper_id
  FROM ym_dupe_map m WHERE n.product_id = m.loser_id;

DELETE FROM products p USING ym_dupe_map m WHERE p.id = m.loser_id;

-- Победителям — канонический URL. Ровно та форма, что строит CanonicalProductURL.
-- ПОСЛЕ удаления проигравших: канон мог принадлежать как раз дублю (url UNIQUE).
UPDATE products SET url = 'https://market.yandex.ru/product/'
                        || substring(url from 'market\.yandex\.ru/product(?:--[^/?#]*)?/(\d+)')
 WHERE marketplace = 'yandex_market'
   AND substring(url from 'market\.yandex\.ru/product(?:--[^/?#]*)?/(\d+)') IS NOT NULL
   AND url <> 'https://market.yandex.ru/product/'
              || substring(url from 'market\.yandex\.ru/product(?:--[^/?#]*)?/(\d+)');

\echo '== ссылки YM, из которых id не достаётся (оставлены КАК ЕСТЬ) =='
SELECT id, left(url, 70) AS url
FROM products
WHERE marketplace = 'yandex_market'
  AND substring(url from 'market\.yandex\.ru/product(?:--[^/?#]*)?/(\d+)') IS NULL;

\echo '== контроль: дублей YM не осталось =='
WITH keyed AS (
    SELECT substring(url from 'market\.yandex\.ru/product(?:--[^/?#]*)?/(\d+)') AS ext_id
    FROM products WHERE marketplace = 'yandex_market'
)
SELECT count(*) AS still_dupes
FROM (SELECT ext_id FROM keyed WHERE ext_id IS NOT NULL
      GROUP BY ext_id HAVING count(*) > 1) x;

COMMIT;
