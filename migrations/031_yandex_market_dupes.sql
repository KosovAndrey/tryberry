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

\echo '== сколько строк схлопнется (юзер/выдача держат НЕСКОЛЬКО копий одного товара) =='
SELECT 'subscriptions' AS tbl, count(*) - count(DISTINCT (s.user_id, COALESCE(m.keeper_id, s.product_id))) AS dropped
  FROM subscriptions s LEFT JOIN ym_dupe_map m ON m.loser_id = s.product_id
UNION ALL
SELECT 'search_results', count(*) - count(DISTINCT (r.search_query_id, COALESCE(m.keeper_id, r.product_id)))
  FROM search_results r LEFT JOIN ym_dupe_map m ON m.loser_id = r.product_id
UNION ALL
SELECT 'search_subscription_products', count(*) - count(DISTINCT (p.subscription_id, COALESCE(m.keeper_id, p.product_id)))
  FROM search_subscription_products p LEFT JOIN ym_dupe_map m ON m.loser_id = p.product_id;

-- Победитель группы — СТАРЕЙШИЙ (min(id)): у него настоящий created_at
-- («наблюдаем с»), самая длинная история и уже разошедшийся по ссылкам public_id.
--
-- ⚠️ Схема «оставить ровно ОДНОГО выжившего в группе», а не «удалить коллизию с
-- победителем». Разница принципиальна: у YM в группе до 12 копий, и в одну
-- выдачу/подписку могут попасть ДВА дубля БЕЗ победителя — тогда наивный DELETE
-- ничего не находит, а UPDATE схлопывает обоих в один product_id и падает по
-- первичному ключу (проверено на проде: search_results_pkey (60, 6064927)).
-- row_number по (владелец, ЦЕЛЕВОЙ товар) устойчив к любому числу копий; строка
-- победителя выигрывает сортировку (m.loser_id IS NULL → первым).

-- ── subscriptions: UNIQUE (user_id, product_id) ──────────────────────────────
CREATE TEMP TABLE ym_sub_remap ON COMMIT DROP AS
SELECT s.id AS sub_id,
       s.user_id,
       COALESCE(m.keeper_id, s.product_id) AS target,
       (s.product_id <> COALESCE(m.keeper_id, s.product_id)) AS moves,
       row_number() OVER (
           PARTITION BY s.user_id, COALESCE(m.keeper_id, s.product_id)
           ORDER BY (m.loser_id IS NULL) DESC, s.id
       ) AS rn
FROM subscriptions s
LEFT JOIN ym_dupe_map m ON m.loser_id = s.product_id;

-- rn > 1 — это всегда строка-дубль: подписка на победителя сортируется первой, а
-- двух строк на один (user_id, keeper_id) быть не может (UNIQUE).
-- notifications висят на подписке БЕЗ каскада → переподцепляем к выжившей ДО
-- удаления, иначе падение по FK.
UPDATE notifications n SET subscription_id = surv.sub_id
  FROM ym_sub_remap dead
  JOIN ym_sub_remap surv ON surv.user_id = dead.user_id AND surv.target = dead.target AND surv.rn = 1
 WHERE dead.rn > 1 AND n.subscription_id = dead.sub_id;

DELETE FROM subscriptions s USING ym_sub_remap dead
 WHERE dead.rn > 1 AND s.id = dead.sub_id;

UPDATE subscriptions s SET product_id = r.target
  FROM ym_sub_remap r WHERE r.rn = 1 AND r.moves AND s.id = r.sub_id;

-- ── price_history: уникальности нет, product_id не в ключе партиционирования ──
UPDATE price_history h SET product_id = m.keeper_id
  FROM ym_dupe_map m WHERE h.product_id = m.loser_id;

-- ── search_results: PK (search_query_id, product_id) ─────────────────────────
CREATE TEMP TABLE ym_sr_remap ON COMMIT DROP AS
SELECT r.search_query_id,
       r.product_id,
       COALESCE(m.keeper_id, r.product_id) AS target,
       row_number() OVER (
           PARTITION BY r.search_query_id, COALESCE(m.keeper_id, r.product_id)
           ORDER BY (m.loser_id IS NULL) DESC, r.position, r.product_id
       ) AS rn
FROM search_results r
LEFT JOIN ym_dupe_map m ON m.loser_id = r.product_id;

DELETE FROM search_results r USING ym_sr_remap x
 WHERE x.rn > 1 AND r.search_query_id = x.search_query_id AND r.product_id = x.product_id;

UPDATE search_results r SET product_id = x.target
  FROM ym_sr_remap x
 WHERE x.rn = 1 AND x.product_id <> x.target
   AND r.search_query_id = x.search_query_id AND r.product_id = x.product_id;

-- ── search_subscription_products: PK (subscription_id, product_id) ───────────
-- Baseline победителя старше — он и остаётся (сортировка по first_seen_at).
CREATE TEMP TABLE ym_ssp_remap ON COMMIT DROP AS
SELECT p.subscription_id,
       p.product_id,
       COALESCE(m.keeper_id, p.product_id) AS target,
       row_number() OVER (
           PARTITION BY p.subscription_id, COALESCE(m.keeper_id, p.product_id)
           ORDER BY (m.loser_id IS NULL) DESC, p.first_seen_at, p.product_id
       ) AS rn
FROM search_subscription_products p
LEFT JOIN ym_dupe_map m ON m.loser_id = p.product_id;

DELETE FROM search_subscription_products p USING ym_ssp_remap x
 WHERE x.rn > 1 AND p.subscription_id = x.subscription_id AND p.product_id = x.product_id;

UPDATE search_subscription_products p SET product_id = x.target
  FROM ym_ssp_remap x
 WHERE x.rn = 1 AND x.product_id <> x.target
   AND p.subscription_id = x.subscription_id AND p.product_id = x.product_id;

-- ── search_notifications: уникальности нет ───────────────────────────────────
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
