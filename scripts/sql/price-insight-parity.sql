-- ─────────────────────────────────────────────────────────────────────────────
-- price-insight-parity.sql — сверка вердиктов price-insight (JVM) со старым
-- путём Go: PriceHistoryRepo.Stats + domain.AssessHonestPrice.
--
-- Одноразовый диагностический скрипт, ТОЛЬКО ЧТЕНИЕ. Ничего не меняет.
-- Запуск на проде:
--   docker compose exec -T postgres psql -U user -d tryberrybot -f - < scripts/sql/price-insight-parity.sql
-- либо скопировать содержимое в psql.
--
-- Зачем два разных сравнения. Go считает по ВСЕЙ price_history (месяцы, плюс
-- бэкфилл WB через PrependOlder), а price-insight знает только то, что проехало
-- через топик price-events — у него retention 72 часа. Поэтому «в лоб» вердикты
-- обязаны расходиться, пока сервис не накопит историю вперёд, и такое
-- расхождение ничего не говорит о правильности агрегации.
--
--   Срез А (честный тест ЛОГИКИ) — Go считает по тому же окну наблюдения, что
--   и Java: только строки price_history НЕ СТАРШЕ observed_since. Тут вердикты
--   обязаны совпадать; расхождение = настоящий дефект.
--
--   Срез Б (что увидит notifier) — Go считает по всей своей истории. Показывает
--   ЦЕНУ ПЕРЕКЛЮЧЕНИЯ: насколько вердикт из price_insight сегодня отличается от
--   того, что бот показывает пользователю сейчас.
--
-- Сравниваем только товары В НАЛИЧИИ: при OOS у price-insight нет открытого
-- сегмента и он намеренно молчит (последний вердикт остаётся старым), а Go
-- посчитал бы по последней известной цене — сравнивать нечего.
-- ─────────────────────────────────────────────────────────────────────────────

\set ON_ERROR_STOP on

-- Гейт достаточности из honest_price.go (honestMinAge). В проде 7 дней.
\set min_age '7 days'

DROP TABLE IF EXISTS pg_temp.parity_base;

-- Момент оценки берём ИЗ СТРОКИ price_insight (computed_at): Go надо считать на
-- тот же момент, иначе разъедется само окно, а не логика.
CREATE TEMP TABLE parity_base AS
SELECT pi.product_id,
       pi.verdict        AS java_verdict,
       pi.min_30         AS java_min30,
       pi.median_30      AS java_median30,
       pi.min_90         AS java_min90,
       pi.min_all        AS java_min_all,
       pi.observed_since,
       pi.computed_at    AS at,
       (SELECT ph.price FROM price_history ph
         WHERE ph.product_id = pi.product_id
         ORDER BY ph.recorded_at DESC LIMIT 1) AS current_price
FROM price_insight pi
JOIN products p ON p.id = pi.product_id
WHERE p.in_stock;

-- ── Агрегаты Go. Дословный порт PriceHistoryRepo.Stats, но с параметром
--    from_ts: NULL = вся история (срез Б), observed_since = срез А.
CREATE OR REPLACE FUNCTION pg_temp.go_stats(p_id bigint, p_now timestamptz, p_from timestamptz)
RETURNS TABLE (min30 numeric, median30 numeric, min90 numeric, min_all numeric,
               since timestamptz, has_data boolean)
LANGUAGE sql STABLE AS $$
    WITH src AS (
        SELECT price, recorded_at
        FROM price_history
        WHERE product_id = p_id
          AND (p_from IS NULL OR recorded_at >= p_from)
    ),
    seg AS (
        SELECT price, recorded_at AS t0,
               lead(recorded_at, 1, p_now) OVER (ORDER BY recorded_at) AS t1
        FROM src
    ),
    seg30 AS (
        SELECT price,
               GREATEST(t0, p_now - interval '30 days') AS s,
               LEAST(t1, p_now)                         AS e
        FROM seg
        WHERE t1 > p_now - interval '30 days' AND t0 < p_now
    ),
    dur30 AS (
        SELECT price, EXTRACT(EPOCH FROM (e - s)) AS d FROM seg30 WHERE e > s
    ),
    wmed AS (
        SELECT price,
               SUM(d) OVER (ORDER BY price) AS cum,
               SUM(d) OVER ()               AS tot
        FROM dur30
    )
    SELECT
        (SELECT min(price) FROM dur30),
        (SELECT price FROM wmed WHERE tot > 0 AND cum >= tot / 2.0 ORDER BY price LIMIT 1),
        (SELECT min(price) FROM seg WHERE t1 > p_now - interval '90 days'),
        (SELECT min(price) FROM src),
        (SELECT min(recorded_at) FROM src),
        (SELECT count(*) > 0 FROM src);
$$;

-- ── Вердикт Go. Дословный порт AssessHonestPrice: порядок веток и пороги.
CREATE OR REPLACE FUNCTION pg_temp.go_verdict(
    p_current numeric, p_min30 numeric, p_median30 numeric, p_min90 numeric,
    p_min_all numeric, p_since timestamptz, p_has_data boolean, p_now timestamptz,
    p_min_age interval)
RETURNS smallint
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE
        WHEN p_current IS NULL OR p_current <= 0 OR NOT p_has_data
             OR p_since IS NULL OR (p_now - p_since) < p_min_age THEN 0::smallint
        WHEN COALESCE(p_min_all,0)   > 0 AND p_current <= p_min_all   THEN 1::smallint
        WHEN COALESCE(p_min90,0)     > 0 AND p_current <= p_min90     THEN 2::smallint
        WHEN COALESCE(p_min30,0)     > 0 AND p_current <= p_min30     THEN 3::smallint
        WHEN COALESCE(p_median30,0)  > 0 AND p_current <= p_median30  THEN 4::smallint
        ELSE 5::smallint
    END;
$$;

DROP TABLE IF EXISTS pg_temp.parity;
CREATE TEMP TABLE parity AS
SELECT b.*,
       a.min30 AS a_min30, a.median30 AS a_median30, a.min90 AS a_min90, a.min_all AS a_min_all,
       pg_temp.go_verdict(b.current_price, a.min30, a.median30, a.min90, a.min_all,
                          a.since, a.has_data, b.at, :'min_age'::interval) AS a_verdict,
       f.min30 AS b_min30, f.median30 AS b_median30, f.min90 AS b_min90, f.min_all AS b_min_all,
       pg_temp.go_verdict(b.current_price, f.min30, f.median30, f.min90, f.min_all,
                          f.since, f.has_data, b.at, :'min_age'::interval) AS b_verdict
FROM parity_base b
CROSS JOIN LATERAL pg_temp.go_stats(b.product_id, b.at, b.observed_since) a
CROSS JOIN LATERAL pg_temp.go_stats(b.product_id, b.at, NULL)             f;

\echo ''
\echo '=== Объём выборки ==='
SELECT count(*) AS товаров_сравнено,
       count(*) FILTER (WHERE current_price IS NULL) AS без_истории_в_pg
FROM parity;

\echo ''
\echo '=== СРЕЗ А: одинаковое окно наблюдения — проверка ЛОГИКИ ==='
\echo '(расхождение здесь = настоящий дефект, разбирать поштучно)'
SELECT count(*) FILTER (WHERE java_verdict = a_verdict) AS вердикт_совпал,
       count(*) FILTER (WHERE java_verdict <> a_verdict) AS вердикт_разошёлся,
       count(*) FILTER (WHERE abs(java_min30    - COALESCE(a_min30,0))    > 0.01) AS min30_разошёлся,
       count(*) FILTER (WHERE abs(java_median30 - COALESCE(a_median30,0)) > 0.01) AS median30_разошёлся,
       count(*) FILTER (WHERE abs(java_min90    - COALESCE(a_min90,0))    > 0.01) AS min90_разошёлся,
       count(*) FILTER (WHERE abs(java_min_all  - COALESCE(a_min_all,0))  > 0.01) AS min_all_разошёлся
FROM parity;

\echo ''
\echo '=== СРЕЗ А: поштучный разбор расхождений (до 50) ==='
SELECT product_id, current_price AS цена, observed_since::date AS наблюдаем_с,
       java_verdict AS j_v, a_verdict AS go_v,
       java_min30 AS j_min30, a_min30 AS go_min30,
       java_median30 AS j_med, a_median30 AS go_med,
       java_min_all AS j_all, a_min_all AS go_all
FROM parity
WHERE java_verdict <> a_verdict
   OR abs(java_min30    - COALESCE(a_min30,0))    > 0.01
   OR abs(java_median30 - COALESCE(a_median30,0)) > 0.01
   OR abs(java_min90    - COALESCE(a_min90,0))    > 0.01
   OR abs(java_min_all  - COALESCE(a_min_all,0))  > 0.01
ORDER BY product_id
LIMIT 50;

\echo ''
\echo '=== СРЕЗ Б: цена переключения (Go по ВСЕЙ истории) ==='
\echo '(расхождение ожидаемо, пока сервис не накопил историю: retention price-events 72ч)'
SELECT count(*) FILTER (WHERE java_verdict = b_verdict)  AS совпал,
       count(*) FILTER (WHERE java_verdict <> b_verdict) AS разошёлся,
       round(100.0 * count(*) FILTER (WHERE java_verdict = b_verdict) / NULLIF(count(*),0), 1) AS процент_совпадений
FROM parity;

\echo ''
\echo '=== СРЕЗ Б: матрица переходов вердиктов (Go → Java) ==='
SELECT b_verdict AS go_вердикт, java_verdict AS java_вердикт, count(*)
FROM parity
WHERE java_verdict <> b_verdict
GROUP BY 1, 2 ORDER BY 3 DESC;

\echo ''
\echo '=== Насколько глубже история у Go ==='
SELECT round(avg(EXTRACT(EPOCH FROM (observed_since - go_since)) / 86400)::numeric, 1) AS в_среднем_дней_глубже,
       max(EXTRACT(EPOCH FROM (observed_since - go_since)) / 86400)::int AS максимум_дней
FROM parity p
CROSS JOIN LATERAL (
    SELECT min(recorded_at) AS go_since FROM price_history WHERE product_id = p.product_id
) g
WHERE go_since IS NOT NULL;
