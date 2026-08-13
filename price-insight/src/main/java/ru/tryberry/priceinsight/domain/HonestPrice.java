package ru.tryberry.priceinsight.domain;

import java.util.Objects;

/**
 * Результат оценки: вердикт плюс опорные числа. Уходит в топик
 * {@code price-verdict} и в таблицу {@code price_insight}.
 */
public record HonestPrice(PriceVerdict verdict, PriceStats stats) {

    /**
     * Совпадает ли с уже опубликованным по ТОМУ, ЧТО ВИДИТ ПОТРЕБИТЕЛЬ.
     *
     * <p>Сравнивать записи целиком нельзя: в {@link PriceStats} есть {@code seg30}
     * — счётчик сегментов в окне. Это внутренняя диагностика, в сообщение она не
     * попадает, но растёт с каждым событием. Сравнение по ней публиковало бы
     * одинаковый вердикт на каждом обновлении цены — что и поймал тест
     * {@code identicalVerdictNotRepublished}.
     */
    public boolean sameAsPublished(HonestPrice other) {
        return other != null
                && verdict == other.verdict
                && stats.min30() == other.stats().min30()
                && stats.median30() == other.stats().median30()
                && stats.min90() == other.stats().min90()
                && stats.minAll() == other.stats().minAll()
                && Objects.equals(stats.since(), other.stats().since());
    }
}
