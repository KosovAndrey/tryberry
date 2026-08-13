package ru.tryberry.priceinsight.domain;

import java.time.Instant;

/**
 * Агрегаты по истории наблюдений. Зеркало {@code domain.PriceStats} из Go
 * ({@code internal/domain/honest_price.go}) — поля совпадают, чтобы тесты можно
 * было сверять с {@code honest_price_test.go} один в один.
 *
 * @param min30     минимум за 30 дней
 * @param median30  медиана за 30 дней, взвешенная по длительности
 * @param min90     минимум за 90 дней
 * @param minAll    минимум за всё наблюдение
 * @param seg30     сегментов, пересекающих 30-дневное окно
 * @param since     когда начали наблюдать товар
 * @param hasData   есть хоть одна запись
 */
public record PriceStats(
        double min30,
        double median30,
        double min90,
        double minAll,
        int seg30,
        Instant since,
        boolean hasData
) {
    public static PriceStats empty() {
        return new PriceStats(0, 0, 0, 0, 0, null, false);
    }
}
