-- Сводка промо-атрибуции: формат × площадка → старты, триалы, оплаты
-- (docs/PROMO-SHORTS-PLAN.md §6). Запуск на проде (PG-порт закрыт, изнутри):
--   docker exec -i pt_postgres psql -U user -d tryberrybot \
--     < scripts/sql/attribution-report.sql
--
-- starts  — юзеры, чьё ПЕРВОЕ касание было по промо-ссылке v_<формат>_<площадка>
-- trials  — из них активировали триал
-- paid    — из них хотя бы раз успешно оплатили
-- revenue — сумма их успешных платежей, ₽
--
-- Конвенция меток: формат = семейство + номер ролика через дефис (f1-03 =
-- ролик №3 формата F1), площадка = yt/vkclips/ig/tt/...; каталоги — dir_*
-- (v_dir_tgstat). Каждый ролик получает СВОЮ ссылку → первая секция ниже
-- сама даёт разрез «какой ролик приводит», вторая сворачивает до семейства.

-- ── 0. Контроль: рилсы или органика ─────────────────────────────────────────
-- Главный вопрос при малых объёмах: пришли ли люди ПО ССЫЛКЕ или сами.
-- Живые (не синтетика) регистрации за 30 дней, разложенные на «с меткой» и
-- «без метки». Старты без метки промо не приписываем — это органика, переход
-- из профиля/шапки или прямой поиск бота.
SELECT
    CASE WHEN a.user_id IS NULL THEN 'без метки (органика)'
         ELSE 'по промо-ссылке' END              AS source,
    COUNT(*)                                     AS users,
    COUNT(*) FILTER (WHERE u.trial_used)         AS trials,
    COUNT(*) FILTER (WHERE pay.user_id IS NOT NULL) AS paid,
    min(u.created_at)::date                      AS first_seen,
    max(u.created_at)::date                      AS last_seen
FROM users u
LEFT JOIN user_attribution a ON a.user_id = u.id
LEFT JOIN (
    SELECT DISTINCT user_id FROM payments WHERE status = 'succeeded'
) pay ON pay.user_id = u.id
WHERE NOT u.is_synthetic
  AND u.created_at >= now() - interval '30 days'
GROUP BY 1
ORDER BY users DESC;

-- Ролик (формат) × площадка (сводная, канал суммарно)
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

-- Семейство формата (f1-03 → f1) × площадка — что снимать дальше
SELECT
    split_part(a.format, '-', 1) AS format_family,
    a.platform,
    COUNT(*)                                      AS starts,
    COUNT(*) FILTER (WHERE u.trial_used)          AS trials,
    COUNT(DISTINCT a.format)                      AS videos -- сколько роликов внесло вклад
FROM user_attribution a
JOIN users u ON u.id = a.user_id
WHERE NOT u.is_synthetic
GROUP BY format_family, a.platform
ORDER BY starts DESC;

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
