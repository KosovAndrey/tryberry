package ru.tryberry.priceinsight.projection;

import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Component;

import java.sql.Timestamp;
import java.util.List;
import ru.tryberry.priceinsight.stream.PriceVerdictMessage;

/**
 * Проекция топика {@code price-verdict} в таблицу {@code price_insight}.
 *
 * <p><b>Почему отдельным консьюмером, а не узлом топологии.</b> Запись во
 * внешнюю систему нельзя втянуть в транзакцию Kafka: коммит оффсета и INSERT в
 * Postgres — два разных ресурса, и {@code exactly_once_v2} на них не
 * распространяется. Держа проекцию снаружи, мы оставляем потоковую часть
 * свободной от побочных эффектов: exactly-once внутри Kafka остаётся честным, а
 * писатель в Postgres падает и ретраится независимо, не останавливая обработку.
 *
 * <p>Отсюда требование: <b>upsert обязан быть идемпотентным</b>. Гарантия здесь
 * at-least-once, одно и то же сообщение может приехать дважды —
 * {@code ON CONFLICT DO UPDATE} делает повтор безвредным.
 */
@Component
public class VerdictProjector {

    private static final String UPSERT = """
            INSERT INTO price_insight
                (product_id, verdict, min_30, median_30, min_90, min_all, observed_since, computed_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT (product_id) DO UPDATE SET
                verdict        = EXCLUDED.verdict,
                min_30         = EXCLUDED.min_30,
                median_30      = EXCLUDED.median_30,
                min_90         = EXCLUDED.min_90,
                min_all        = EXCLUDED.min_all,
                observed_since = EXCLUDED.observed_since,
                computed_at    = EXCLUDED.computed_at
            """;

    private final JdbcTemplate jdbc;

    public VerdictProjector(JdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    /**
     * Пишет пачку одним batch, В ПОРЯДКЕ ЧТЕНИЯ ТОПИКА — он и есть истина.
     *
     * <p><b>Почему нет защиты «не затирать более свежий computed_at».</b> Такое
     * условие тут стояло и оказалось вредным. {@code computed_at} — это НЕ
     * версия строки: у вердикта из события это время события, у вердикта из
     * пунктуатора — stream-time, и второе легко больше первого. Поймано на
     * локальном прогоне: тик пунктуатора отдал вердикт с {@code computed_at}
     * «сегодня», следом приехало настоящее событие о падении цены с
     * {@code computed_at} «вчера» — и guard отбросил его НАВСЕГДА. В таблице
     * осталось min_all=1000 вместо 700, то есть ровно то враньё, ради борьбы с
     * которым сервис и написан.
     *
     * <p>Порядок обеспечивает Kafka: ключ — product_id, товар всегда живёт в
     * одной партиции, значит его вердикты приходят строго в порядке появления.
     * Последний выигрывает. Перечитывание топика с начала проигрывает ту же
     * последовательность и сходится к тому же значению — расхождение при этом
     * временное и самозаживающее, в отличие от вечно замороженной строки.
     */
    public void apply(List<PriceVerdictMessage> batch) {
        if (batch == null || batch.isEmpty()) {
            return;
        }
        jdbc.batchUpdate(UPSERT, batch, batch.size(), (ps, m) -> {
            ps.setLong(1, m.productId());
            ps.setInt(2, m.verdict());
            ps.setDouble(3, m.min30());
            ps.setDouble(4, m.median30());
            ps.setDouble(5, m.min90());
            ps.setDouble(6, m.minAll());
            ps.setTimestamp(7, m.observedSince() == null ? null : Timestamp.from(m.observedSince()));
            ps.setTimestamp(8, Timestamp.from(m.computedAt()));
        });
    }
}
