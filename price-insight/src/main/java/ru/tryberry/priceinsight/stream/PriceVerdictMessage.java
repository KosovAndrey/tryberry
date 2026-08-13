package ru.tryberry.priceinsight.stream;

import com.fasterxml.jackson.annotation.JsonProperty;
import ru.tryberry.priceinsight.domain.HonestPrice;

import java.time.Instant;

/**
 * Сообщение топика {@code price-verdict} и строка таблицы {@code price_insight}.
 * Читает Go-сторона, поэтому имена полей в snake_case, как в остальных
 * контрактах проекта, а вердикт едет числовым кодом — см.
 * {@link ru.tryberry.priceinsight.domain.PriceVerdict}.
 */
public record PriceVerdictMessage(
        @JsonProperty("product_id") long productId,
        @JsonProperty("verdict") int verdict,
        @JsonProperty("min_30") double min30,
        @JsonProperty("median_30") double median30,
        @JsonProperty("min_90") double min90,
        @JsonProperty("min_all") double minAll,
        @JsonProperty("observed_since") Instant observedSince,
        @JsonProperty("computed_at") Instant computedAt
) {

    public static PriceVerdictMessage of(long productId, HonestPrice price, Instant computedAt) {
        var s = price.stats();
        return new PriceVerdictMessage(
                productId,
                price.verdict().code(),
                s.min30(),
                s.median30(),
                s.min90(),
                s.minAll(),
                s.since(),
                computedAt);
    }
}
