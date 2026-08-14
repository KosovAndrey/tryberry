-- ─────────────────────────────────────────────────────────────────────────────
-- price-insight-explain.sql — разбор ОДНОГО товара до цифр: как Go строит
-- сегменты в 30-дневном окне и откуда берётся его медиана. Нужен, когда сверка
-- (price-insight-parity.sql) показала расхождение и надо понять причину, а не
-- угадывать её.
--
-- Только чтение. Запуск:
--   psql ... -v pid=8199607 -f scripts/sql/price-insight-explain.sql
-- или через compose:
--   docker compose ... exec -T postgres psql -U user -d tryberrybot \
--     -v pid=8199607 -f - < scripts/sql/price-insight-explain.sql
--
-- Почему это нужно именно так. Два первых объяснения остаточных расхождений
-- (OOS-семантика и события, потерянные при рестарте Kafka) не подтвердились
-- статистикой, причём проверка OOS была негодной по построению: при отсутствии
-- в наличии scraper строку в price_history НЕ пишет, поэтому «разрыв между
-- строками» этот механизм не производит и обнаружить его так нельзя. Остаётся
-- смотреть конкретный товар и сравнивать посегментно.
--
-- Историю OOS в Postgres не найти вообще (products.in_stock — только текущее
-- состояние, в notifications нет типа триггера). Признак живёт лишь в потоке:
--   kafka-console-consumer --topic price-events --from-beginning \
--     --property print.key=true | grep -F "<pid>{" | grep '"new_price":0'
-- ─────────────────────────────────────────────────────────────────────────────

\set ON_ERROR_STOP on

\echo ''
\echo '=== Что published сервис ==='
SELECT verdict, min_30, median_30, min_90, min_all,
       observed_since, computed_at
FROM price_insight WHERE product_id = :pid;

\echo ''
\echo '=== Сегменты Go в 30-дневном окне (окно от computed_at строки сервиса) ==='
\echo '(share_pct — доля времени; медиана Go = первая цена, где cum_pct >= 50)'
WITH ref AS (
    SELECT computed_at AS at, observed_since FROM price_insight WHERE product_id = :pid
),
src AS (
    SELECT price, recorded_at FROM price_history, ref
     WHERE product_id = :pid AND recorded_at >= ref.observed_since
    UNION ALL
    SELECT price, (SELECT observed_since FROM ref)
      FROM (SELECT price, recorded_at FROM price_history, ref
             WHERE product_id = :pid AND recorded_at < ref.observed_since
             ORDER BY recorded_at DESC LIMIT 1) anchor
),
seg AS (
    SELECT price, recorded_at AS t0,
           lead(recorded_at, 1, (SELECT at FROM ref)) OVER (ORDER BY recorded_at) AS t1
    FROM src
),
clipped AS (
    SELECT price,
           GREATEST(t0, (SELECT at FROM ref) - interval '30 days') AS s,
           LEAST(t1, (SELECT at FROM ref))                        AS e
    FROM seg
    WHERE t1 > (SELECT at FROM ref) - interval '30 days'
      AND t0 < (SELECT at FROM ref)
),
dur AS (
    SELECT price, s, e, EXTRACT(EPOCH FROM (e - s)) AS d FROM clipped WHERE e > s
)
SELECT price,
       to_char(s, 'MM-DD HH24:MI') AS начало,
       to_char(e, 'MM-DD HH24:MI') AS конец,
       round((d / 3600)::numeric, 1) AS часов,
       round((100 * d / SUM(d) OVER ())::numeric, 1) AS share_pct,
       round((100 * SUM(d) OVER (ORDER BY price) / SUM(d) OVER ())::numeric, 1) AS cum_pct
FROM dur
ORDER BY price;

\echo ''
\echo '=== Точки истории вокруг окна (последние 40) ==='
SELECT to_char(recorded_at, 'MM-DD HH24:MI:SS') AS записано, price
FROM price_history
WHERE product_id = :pid
ORDER BY recorded_at DESC
LIMIT 40;
