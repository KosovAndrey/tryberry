-- Чистка «битых» товаров Я.Маркета с неразвёрнутым шортом /cc/<код>.
--
-- Причина: короткая ссылка не развернулась (SmartCaptcha у YM-шортов) → в
-- products.url лёг сырой market.yandex.ru/cc/… → ymExtractSKU не нашёл числового
-- сегмента → sku='' → ymStatePrice взял ПЕРВУЮ цену на странице (чужого товара
-- из рекомендаций). Тот же класс, что ozon-oos-foreign-price-bug.
--
-- Скрейпер теперь такие URL ОТВЕРГАЕТ (YandexMarketScraper.Matches, коммит
-- 30057ad) — новые /cc/ не заводятся и фоновый скрейп по ним больше не пишет
-- чужую цену. Здесь вычищаем ИСТОРИЧЕСКИЕ записи: сами товары, их подписки,
-- историю цен и уведомления. search_*-строки уйдут каскадом при DELETE products.
--
-- FK-порядок важен: price_history/subscriptions/notifications ссылаются на
-- products/subscriptions БЕЗ ON DELETE CASCADE (001_init), поэтому детей удаляем
-- вручную и раньше родителей. search_results/search_subscription_products/
-- search_notifications — с каскадом (003_search), снесутся при DELETE products.
--
-- ЗАПУСК на проде (PG-порт закрыт — только через контейнер):
--   dry-run (показать, что затронется, и откатить):
--     docker exec -i pt_postgres psql -U user -d tryberrybot -v ON_ERROR_STOP=1 -f - < scripts/sql/cleanup-ym-cc.sql
--   применить:
--     docker exec -i pt_postgres psql -U user -d tryberrybot -v ON_ERROR_STOP=1 -v apply=1 -f - < scripts/sql/cleanup-ym-cc.sql

BEGIN;

-- Целевые товары. url ~ якорим на market.yandex.ru/cc/, чтобы не задеть чужие
-- маркетплейсы и легитимные пути (/product/, /card/, /business--).
CREATE TEMP TABLE cc_products ON COMMIT DROP AS
SELECT id, url, public_id
FROM products
WHERE marketplace = 'yandex_market'
  AND url ~ 'market\.yandex\.ru/cc/';

\echo ''
\echo '=== ТОВАРЫ ПОД УДАЛЕНИЕ ==='
SELECT p.id, p.url,
       (SELECT count(*) FROM price_history ph WHERE ph.product_id = p.id)               AS price_points,
       (SELECT count(*) FROM subscriptions s  WHERE s.product_id  = p.id)               AS subs,
       (SELECT count(*) FROM subscriptions s  WHERE s.product_id  = p.id AND s.active)  AS active_subs
FROM products p
WHERE p.id IN (SELECT id FROM cc_products)
ORDER BY p.id;

-- Удаление в порядке FK.
DELETE FROM notifications
 WHERE subscription_id IN (
   SELECT id FROM subscriptions WHERE product_id IN (SELECT id FROM cc_products)
 );
-- pending_alerts — durable outbox БЕЗ FK (плоские BIGINT). Не блокирует delete и
-- не каскадит, но недоставленные строки флашер ещё отправит (payload — самодостаточный
-- JSON), т.е. прилетела бы чужая /cc/-цена. Чистим по product_id.
DELETE FROM pending_alerts WHERE product_id IN (SELECT id FROM cc_products);
DELETE FROM subscriptions  WHERE product_id IN (SELECT id FROM cc_products);
DELETE FROM price_history   WHERE product_id IN (SELECT id FROM cc_products);
DELETE FROM products        WHERE id         IN (SELECT id FROM cc_products);

\echo ''
\echo '=== ОСТАТОК /cc/ ПОСЛЕ ЧИСТКИ (ожидаем 0) ==='
SELECT count(*) AS remaining_cc
FROM products
WHERE marketplace = 'yandex_market' AND url ~ 'market\.yandex\.ru/cc/';

-- Dry-run по умолчанию: без -v apply=1 всё откатывается, числа DELETE выше —
-- это ПРЕДПРОСМОТР. С -v apply=1 — коммитим.
\if :{?apply}
  COMMIT;
  \echo '>>> ПРИМЕНЕНО (COMMIT).'
\else
  ROLLBACK;
  \echo '>>> DRY-RUN: откачено. Повтори с -v apply=1, чтобы применить.'
\endif
