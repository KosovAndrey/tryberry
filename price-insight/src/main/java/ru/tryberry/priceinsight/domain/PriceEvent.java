package ru.tryberry.priceinsight.domain;

import com.fasterxml.jackson.annotation.JsonProperty;

import java.time.Instant;

/**
 * Сообщение топика {@code price-events}. Пишет Go-скрапер, см.
 * {@code internal/domain/product.go:PriceEvent} и {@code cmd/scraper/main.go}.
 *
 * <p>Поля аддитивны: старые сообщения без {@code in_stock} дают {@code false}.
 * Обработчик обязан это переживать — отсюда обёртки {@link Boolean} с
 * приведением к {@code false} в геттерах.
 *
 * <p>{@code newPrice == 0} означает «нет в наличии», а не «цена ноль». Такая
 * точка закрывает текущий сегмент и НЕ открывает новый — интервал отсутствия не
 * должен засчитываться как время по последней известной цене.
 */
public record PriceEvent(
        @JsonProperty("product_id") long productId,
        @JsonProperty("marketplace") String marketplace,
        @JsonProperty("old_price") double oldPrice,
        @JsonProperty("new_price") double newPrice,
        @JsonProperty("recorded_at") Instant recordedAt,
        @JsonProperty("in_stock") Boolean inStock,
        @JsonProperty("was_in_stock") Boolean wasInStock
) {

    /**
     * Цена наблюдаема. Критерий — только {@code newPrice > 0}, без проверки
     * {@code inStock}: у старых сообщений поле отсутствует и даёт {@code false},
     * и проверка по нему выкинула бы всю раннюю историю. Go-сторона выводит
     * наличие так же — «страховка для старых событий», см. cmd/scraper/main.go.
     */
    public boolean hasPrice() {
        return newPrice > 0;
    }
}
