-- Сводка промо-атрибуции: формат × площадка → старты, триалы, оплаты
-- (docs/PROMO-SHORTS-PLAN.md §6). Запуск на проде (PG-порт закрыт, изнутри):
--   docker exec -i pt_postgres psql -U user -d tryberrybot \
--     < scripts/sql/attribution-report.sql
--
-- starts  — юзеры, чьё ПЕРВОЕ касание было по промо-ссылке v_<формат>_<площадка>
-- trials  — из них активировали триал
-- paid    — из них хотя бы раз успешно оплатили
-- revenue — сумма их успешных платежей, ₽

-- Формат × площадка (сводная, канал суммарно)
SELECT
    a.format,
    a.platform,
    COUNT(*)                                            AS starts,
    COUNT(*) FILTER (WHERE u.trial_used)                AS trials,
    COUNT(*) FILTER (WHERE p.user_id IS NOT NULL)       AS paid,
    COALESCE(SUM(p.total_kopecks), 0) / 100             AS revenue_rub
FROM user_attribution a
JOIN users u ON u.id = a.user_id
LEFT JOIN (
    SELECT user_id, SUM(amount_kopecks) AS total_kopecks
    FROM payments
    WHERE status = 'succeeded'
    GROUP BY user_id
) p ON p.user_id = a.user_id
WHERE NOT u.is_synthetic
GROUP BY a.format, a.platform
ORDER BY starts DESC, a.format, a.platform;

-- Разбивка по каналам входа (tg/vk/max) — какой мессенджер выбирают
SELECT
    a.channel,
    a.format,
    a.platform,
    COUNT(*) AS starts
FROM user_attribution a
JOIN users u ON u.id = a.user_id
WHERE NOT u.is_synthetic
GROUP BY a.channel, a.format, a.platform
ORDER BY starts DESC;

-- Динамика по неделям (для еженедельной сводки из §6)
SELECT
    date_trunc('week', a.created_at)::date AS week,
    a.format,
    a.platform,
    COUNT(*) AS starts
FROM user_attribution a
JOIN users u ON u.id = a.user_id
WHERE NOT u.is_synthetic
GROUP BY week, a.format, a.platform
ORDER BY week DESC, starts DESC;
