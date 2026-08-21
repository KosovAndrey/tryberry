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

    /**
     * Прод-значение — 14 дней, как {@code honestMinAge} в Go. Значение обязано
     * совпадать с Go дословно: пока notifier читает Go-путь, а этот сервис
     * сверяется с ним, разный гейт дал бы расхождение вердиктов, не связанное с
     * логикой.
     *
     * <p>Поднято с 7 до 14 дней 21.08.2026. Семи дней хватает, чтобы вердикт был
     * посчитан, но не хватает, чтобы он что-то значил: недельное окно накрывает
     * один цикл распродаж маркетплейса, и «обычная цена» по нему — цена одной
     * акции. Повод — повторная сверка (docs/PRICE-INSIGHT-REVIEW.md §5.2): за
     * неделю ≈1796 товаров перешагнули семидневный гейт и начали получать
     * вердикты, не став информированнее — история у них 17 дней против ~60 у Go.
     */
    public static HonestPriceRules production() {
        return new HonestPriceRules(Duration.ofDays(14));
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
