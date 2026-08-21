package ru.tryberry.priceinsight.domain;

import static org.assertj.core.api.Assertions.assertThat;

import java.time.Duration;
import java.time.Instant;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Гейт достаточности. Значение обязано совпадать с {@code honestMinAge} из
 * {@code internal/domain/honest_price.go}: пока notifier читает Go-путь, а этот
 * сервис сверяется с ним ({@code scripts/sql/price-insight-parity.sql}), разный
 * гейт дал бы расхождение вердиктов, не связанное с логикой. Тест — сторож
 * против правки в одной реализации из двух.
 */
class HonestPriceRulesTest {

    private static final Instant NOW = Instant.parse("2026-08-21T12:00:00Z");

    private static PriceStats stats(Instant since) {
        return new PriceStats(100, 150, 90, 80, 4, since, true);
    }

    @Test
    @DisplayName("прод-гейт — 14 дней, как honestMinAge в Go")
    void productionGateMatchesGo() {
        assertThat(HonestPriceRules.production().assess(170, stats(NOW.minus(Duration.ofDays(13))), NOW).verdict())
                .isEqualTo(PriceVerdict.INSUFFICIENT);
        assertThat(HonestPriceRules.production().assess(170, stats(NOW.minus(Duration.ofDays(15))), NOW).verdict())
                .isEqualTo(PriceVerdict.ABOVE_TYPICAL);
    }

    @Test
    @DisplayName("ровно на границе гейта вердикт уже выдаётся")
    void exactlyAtGateIsEnough() {
        assertThat(HonestPriceRules.production().assess(170, stats(NOW.minus(Duration.ofDays(14))), NOW).verdict())
                .isEqualTo(PriceVerdict.ABOVE_TYPICAL);
    }
}
