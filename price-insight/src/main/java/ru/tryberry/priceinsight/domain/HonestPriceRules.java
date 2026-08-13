package ru.tryberry.priceinsight.domain;

import java.time.Duration;
import java.time.Instant;

/**
 * Правила вердикта. Порт {@code AssessHonestPrice} из
 * {@code internal/domain/honest_price.go} без изменения семантики — порядок
 * проверок, пороги и гейт по возрасту наблюдения совпадают дословно.
 *
 * <p>Логика переехала вместе с агрегацией сознательно: держать вычисление
 * агрегатов здесь, а трактовку — в Go значило бы разрезать один домен по
 * границе сервисов и синхронизировать пороги в двух местах.
 */
public final class HonestPriceRules {

    /**
     * Минимальный срок наблюдения, прежде чем делать выводы. Гейт именно по
     * ВОЗРАСТУ, а не по числу точек: при change-only хранении стабильный товар
     * может иметь одну запись за сорок дней, и счётчик записей ничего не говорит
     * о достаточности выборки.
     */
    private final Duration minAge;

    public HonestPriceRules(Duration minAge) {
        this.minAge = minAge;
    }

    /** Прод-значение — 7 дней, как {@code honestMinAge} в Go. */
    public static HonestPriceRules production() {
        return new HonestPriceRules(Duration.ofDays(7));
    }

    public HonestPrice assess(double current, PriceStats s, Instant now) {
        boolean tooYoung = s.since() == null || Duration.between(s.since(), now).compareTo(minAge) < 0;
        if (current <= 0 || !s.hasData() || tooYoung) {
            return new HonestPrice(PriceVerdict.INSUFFICIENT, s);
        }
        PriceVerdict verdict;
        if (s.minAll() > 0 && current <= s.minAll()) {
            verdict = PriceVerdict.LOWEST_EVER;
        } else if (s.min90() > 0 && current <= s.min90()) {
            verdict = PriceVerdict.LOWEST_90;
        } else if (s.min30() > 0 && current <= s.min30()) {
            verdict = PriceVerdict.LOWEST_30;
        } else if (s.median30() > 0 && current <= s.median30()) {
            verdict = PriceVerdict.TYPICAL;
        } else {
            verdict = PriceVerdict.ABOVE_TYPICAL;
        }
        return new HonestPrice(verdict, s);
    }
}
