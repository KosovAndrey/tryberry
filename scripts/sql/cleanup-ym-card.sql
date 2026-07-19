-- Чистка исторических YM /card/-товаров: срезать протухший query-хвост у
-- сохранённых URL и схлопнуть дубли, возникшие из-за разного query у одной карточки.
--
-- Контекст: /card/<slug>/<id> — рекламная форма (юзеры шлют ссылки с cpc=/
-- sponsored=/showOriginalKmEmptyOffer=). Эти параметры протухают → повторный
-- скрейп сохранённого URL теряет цену, товар гниёт (возраст 1–30 дней против
-- 1 мин у /product/). Код теперь канонизирует /card/ срезанием query (коммит
-- 9ee6b5e), но это чинит только НОВЫЕ треки — исторические строки правим тут.
-- Свести /card/ к /product/<id> нельзя: проверено на прод-IP 2026-07-19, id из
-- /card/ не резолвится как /product/. Поэтому канон = тот же /card/-путь без query.
--
-- FK: price_history/subscriptions/notifications БЕЗ каскада (правим вручную),
-- search_* — с каскадом. UNIQUE(products.url) — поэтому дубли сначала схлопываем
-- (remap на keeper), а уже потом keeper'у ставим чистый URL.
--
-- ЗАПУСК:
--   dry-run:   docker exec -i pt_postgres psql -U user -d tryberrybot -v ON_ERROR_STOP=1 -f - < scripts/sql/cleanup-ym-card.sql
--   применить: docker exec -i pt_postgres psql -U user -d tryberrybot -v ON_ERROR_STOP=1 -v apply=1 -f - < scripts/sql/cleanup-ym-card.sql

BEGIN;

-- Целевые товары + чистый URL (всё до '?'). Совпадает с тем, что даёт код
-- (url.Parse + RawQuery=''): слаг у /card/ только ASCII/дефисы, реэнкодинга нет.
CREATE TEMP TABLE card ON COMMIT DROP AS
SELECT id,
       url,
       regexp_replace(url, '\?.*$', '') AS clean
FROM products
WHERE marketplace = 'yandex_market' AND url LIKE '%market.yandex.ru/card/%';

-- keeper на каждый чистый URL: самая длинная история, при равенстве — меньший id.
CREATE TEMP TABLE keep ON COMMIT DROP AS
SELECT c.id,
       c.clean,
       first_value(c.id) OVER (
         PARTITION BY c.clean
         ORDER BY (SELECT count(*) FROM price_history ph WHERE ph.product_id = c.id) DESC, c.id
       ) AS keeper
FROM card c;

-- Карта проигравших → keeper (в ней только те, кого схлопываем).
CREATE TEMP TABLE m ON COMMIT DROP AS
SELECT id AS loser, keeper FROM keep WHERE id <> keeper;

\echo ''
\echo '== /card/ товары и чистый URL =='
SELECT c.id,
       (SELECT count(*) FROM price_history ph WHERE ph.product_id = c.id) AS hist,
       left(c.clean, 70) AS clean_url
FROM card c ORDER BY c.id;

\echo ''
\echo '== дубли (схлопываем loser→keeper) =='
SELECT clean, count(*) AS losers FROM keep GROUP BY clean HAVING count(*) > 1;

-- ── Схлопывание дублей ────────────────────────────────────────────────────────
-- Подписки: у юзера могли быть обе (loser+keeper) → UNIQUE(user_id,product_id).
-- Оставляем одну (rn=1), уведомления перевешиваем на выжившую, лишние удаляем.
CREATE TEMP TABLE sub_remap ON COMMIT DROP AS
SELECT s.id AS sub_id, s.user_id,
       COALESCE(m.keeper, s.product_id) AS target,
       row_number() OVER (
         PARTITION BY s.user_id, COALESCE(m.keeper, s.product_id)
         ORDER BY (m.loser IS NULL) DESC, s.id
       ) AS rn
FROM subscriptions s
LEFT JOIN m ON m.loser = s.product_id
WHERE s.product_id IN (SELECT id FROM card);

UPDATE notifications n SET subscription_id = surv.sub_id
  FROM sub_remap dead
  JOIN sub_remap surv ON surv.user_id = dead.user_id AND surv.target = dead.target AND surv.rn = 1
 WHERE dead.rn > 1 AND n.subscription_id = dead.sub_id;
DELETE FROM subscriptions s USING sub_remap dead WHERE dead.rn > 1 AND s.id = dead.sub_id;
UPDATE subscriptions s SET product_id = r.target
  FROM sub_remap r WHERE r.rn = 1 AND s.id = r.sub_id AND s.product_id <> r.target;

-- История цен и outbox → keeper.
UPDATE price_history h SET product_id = m.keeper FROM m WHERE h.product_id = m.loser;
UPDATE pending_alerts a SET product_id = m.keeper FROM m WHERE a.product_id = m.loser;

-- search_* (на случай, если /card/ попал в выдачу) — снять дубль, затем перевесить.
DELETE FROM search_results r USING m
 WHERE r.product_id = m.loser
   AND EXISTS (SELECT 1 FROM search_results k WHERE k.search_query_id = r.search_query_id AND k.product_id = m.keeper);
UPDATE search_results r SET product_id = m.keeper FROM m WHERE r.product_id = m.loser;
DELETE FROM search_subscription_products p USING m
 WHERE p.product_id = m.loser
   AND EXISTS (SELECT 1 FROM search_subscription_products k WHERE k.subscription_id = p.subscription_id AND k.product_id = m.keeper);
UPDATE search_subscription_products p SET product_id = m.keeper FROM m WHERE p.product_id = m.loser;

-- Удалить проигравшие товары.
DELETE FROM products WHERE id IN (SELECT loser FROM m);

-- Keeper'ам — чистый URL (query срезан). Дублей уже нет, UNIQUE не нарушится.
UPDATE products p SET url = k.clean
  FROM keep k WHERE p.id = k.keeper AND p.url <> k.clean;

\echo ''
\echo '== остаток /card/ с query (ожидаем 0) =='
SELECT count(*) AS card_with_query
FROM products
WHERE marketplace = 'yandex_market' AND url LIKE '%market.yandex.ru/card/%?%';

\if :{?apply}
  COMMIT;
  \echo '>>> ПРИМЕНЕНО (COMMIT).'
\else
  ROLLBACK;
  \echo '>>> DRY-RUN: откачено. Повтори с -v apply=1, чтобы применить.'
\endif
