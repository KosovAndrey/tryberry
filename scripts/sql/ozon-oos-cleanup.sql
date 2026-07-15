-- ozon-oos-cleanup.sql — разовая чистка price_history от «чужих» цен Ozon.
--
-- ЗАЧЕМ. До фикса (388dac5) скрейпер Ozon на карточке товара НЕ В ПРОДАЖЕ брал
-- цену из полки «с этим покупают»: своего ценового виджета на такой странице нет,
-- а ярус «любой виджет со знаком ₽» дотягивался до соседей. Полка меняется от
-- запроса к запросу → в price_history одного товара смешаны его настоящие цены и
-- цены разных чужих товаров (Palit RTX 5070 Ti: 2 211 ₽ ↔ 405 801 ₽ при реальных
-- ~90 617 ₽). Мусор ломает вердикты honest_price («дешевле не было за всё время»)
-- и светится на публичных /p/.
--
-- ПОЧЕМУ ЦЕЛИКОМ, А НЕ ТОЧЕЧНО. Отличить мусорную точку от настоящей внутри
-- истории товара нечем: маркера принадлежности в price_history не осталось.
-- Поэтому у затронутых товаров историю сносим целиком — она накопится заново
-- (для Ozon история всё равно только форвард-накопительная, бэкфилла нет).
--
-- ОТБОР. MAX(price)/MIN(price) > 3 у одного товара. Детектор грубый: реальная
-- скидка 70%+ тоже даёт 3×. Порог выбран осознанно (2026-07-15) — товаров в зоне
-- 3–5 всего 79, а мусор в вердиктах вреднее пустого графика; реальных юзеров на
-- момент чистки нет.
--
-- ПОРЯДОК ЗАПУСКА (на проде, руками — PG-порт закрыт):
--   psql -U user -d tryberrybot -f scripts/sql/ozon-oos-cleanup.sql
-- затем ОБЯЗАТЕЛЬНО почистить Redis-кэш последних цен (см. §4 ниже), иначе
-- следующий скрейп сравнит цену с мусорной из кэша и разошлёт ложные алерты.

BEGIN;

-- 1. Снимок отобранных товаров: аудит + источник id для Redis-чистки.
--    Таблица остаётся в БД намеренно — по ней видно, что и почему удалили.
CREATE TABLE IF NOT EXISTS ozon_oos_cleanup_20260715 AS
WITH r AS (
    SELECT p.id,
           p.name,
           COUNT(*)                                   AS points,
           MIN(h.price)                               AS min_price,
           MAX(h.price)                               AS max_price,
           MAX(h.price) / NULLIF(MIN(h.price), 0)     AS ratio
      FROM products p
      JOIN price_history h ON h.product_id = p.id
     WHERE p.marketplace = 'ozon'
     GROUP BY p.id, p.name
)
SELECT *, NOW() AS cleaned_at
  FROM r
 WHERE ratio > 3;

-- 2. Сколько отобрали (ожидаем ~578 на 2026-07-15).
SELECT COUNT(*) AS products_to_clean,
       SUM(points) AS history_points_to_delete
  FROM ozon_oos_cleanup_20260715;

-- 3. Собственно чистка истории отобранных товаров.
DELETE FROM price_history
 WHERE product_id IN (SELECT id FROM ozon_oos_cleanup_20260715);

COMMIT;

-- 4. Redis (ОТДЕЛЬНО, вне psql): ключи price:<product_id> держат последнюю цену.
--    Не удалить их = ложные алерты «цена упала» на первом же скрейпе.
--
--    docker exec -i pt_postgres psql -U user -d tryberrybot -tA \
--      -c "SELECT id FROM ozon_oos_cleanup_20260715;" \
--      | sed 's/^/price:/' | xargs -r docker exec -i pt_redis redis-cli DEL
