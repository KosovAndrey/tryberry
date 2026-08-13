package ru.tryberry.priceinsight.domain;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.time.Duration;
import java.time.Instant;

import static org.assertj.core.api.Assertions.assertThat;

/**
 * Кейсы подобраны под ловушки change-only хранения. Их же надо прогнать против
 * Go-реализации ({@code honest_price_test.go}) на одинаковых входах перед
 * переключением notifier.
 */
class ProductStateTest {

    private static final Instant T0 = Instant.parse("2026-05-01T00:00:00Z");

    private static PriceEvent event(Instant at, double price) {
        return new PriceEvent(1L, "wb", 0, price, at, price > 0, true);
    }

    private static Instant plusDays(long days) {
        return T0.plus(Duration.ofDays(days));
    }

    @Test
    @DisplayName("медиана взвешена по времени, а не по числу записей")
    void medianIsTimeWeighted() {
        ProductState state = new ProductState();
        // 1000 ₽ держится 25 дней, потом три быстрых скачка по одному дню.
        state.apply(event(T0, 1000));
        state.apply(event(plusDays(25), 2000));
        state.apply(event(plusDays(26), 2100));
        state.apply(event(plusDays(27), 2200));

        PriceStats stats = state.stats(plusDays(28));

        // По числу записей медиана была бы 2100 — записей с высокой ценой втрое
        // больше. По времени товар провёл 25 дней из 28 на 1000 ₽.
        assertThat(stats.median30()).isEqualTo(1000);
    }

    @Test
    @DisplayName("отсутствие в наличии разрывает сегмент и не тянет медиану вниз")
    void outOfStockBreaksSegment() {
        ProductState state = new ProductState();
        state.apply(event(T0, 1000));
        state.apply(event(plusDays(5), 0));       // ушёл из наличия
        state.apply(event(plusDays(25), 3000));   // вернулся дороже

        PriceStats stats = state.stats(plusDays(28));

        // Двадцать дней отсутствия не засчитаны как время по 1000 ₽:
        // в окне 5 дней по 1000 против 3 дней по 3000, медиана — 1000,
        // но минимум не обнулён нулевой ценой.
        assertThat(stats.min30()).isEqualTo(1000);
        assertThat(stats.minAll()).isEqualTo(1000);
        assertThat(stats.median30()).isEqualTo(1000);
    }

    @Test
    @DisplayName("minAll переживает вычистку сегментов старше retention")
    void minAllSurvivesPrune() {
        ProductState state = new ProductState();
        state.apply(event(T0, 500));                  // самый дешёвый — далеко в прошлом
        state.apply(event(plusDays(1), 2000));
        state.apply(event(plusDays(120), 1800));      // прошло больше 90 дней

        PriceStats stats = state.stats(plusDays(121));

        assertThat(state.segments()).hasSizeLessThan(3);   // хвост вычищен
        assertThat(stats.minAll()).isEqualTo(500);          // но минимум помнится
        assertThat(stats.min90()).isEqualTo(1800);
    }

    @Test
    @DisplayName("опоздавшее событие не переписывает историю задним числом")
    void lateEventIgnored() {
        ProductState state = new ProductState();
        state.apply(event(plusDays(10), 1000));
        state.apply(event(plusDays(5), 100));   // пришло позже, а датировано раньше

        PriceStats stats = state.stats(plusDays(11));

        assertThat(stats.minAll()).isEqualTo(1000);
    }

    @Test
    @DisplayName("окно едет само: агрегаты меняются без новых событий")
    void windowMovesWithoutEvents() {
        ProductState state = new ProductState();
        state.apply(event(T0, 1000));
        state.apply(event(plusDays(1), 5000));

        // Через 40 дней дешёвый сегмент вышел из 30-дневного окна.
        assertThat(state.stats(plusDays(2)).min30()).isEqualTo(1000);
        assertThat(state.stats(plusDays(40)).min30()).isEqualTo(5000);
    }

    @Test
    @DisplayName("опоздавшее событие отсекается и когда открытого сегмента нет")
    void lateEventIgnoredAcrossGap() {
        ProductState state = new ProductState();
        state.apply(event(T0, 1000));
        state.apply(event(plusDays(5), 0));        // ушёл из наличия, открытого сегмента нет
        state.apply(event(plusDays(3), 100));      // опоздавшее, датировано внутри разрыва

        // Раньше гард смотрел на открытый сегмент, которого при отсутствии
        // в наличии просто нет, и такое событие вставлялось задним числом.
        assertThat(state.stats(plusDays(6)).minAll()).isEqualTo(1000);
        assertThat(state.currentPrice()).isZero();
    }

    @Test
    @DisplayName("prune сообщает, менял ли он состояние")
    void pruneReportsChange() {
        ProductState state = new ProductState();
        state.apply(event(T0, 1000));
        state.apply(event(plusDays(1), 2000));

        assertThat(state.prune(plusDays(2))).isFalse();    // ничего не устарело
        assertThat(state.prune(plusDays(200))).isTrue();   // хвост вышел за retention
    }

    @Test
    @DisplayName("текущая цена ноль, когда товара нет в наличии")
    void currentPriceZeroWhenOutOfStock() {
        ProductState state = new ProductState();
        state.apply(event(T0, 1000));
        assertThat(state.currentPrice()).isEqualTo(1000);

        state.apply(event(plusDays(1), 0));
        assertThat(state.currentPrice()).isZero();
    }

    @Test
    @DisplayName("пустое состояние не притворяется, что данные есть")
    void emptyState() {
        assertThat(new ProductState().stats(T0).hasData()).isFalse();
    }

    @Test
    @DisplayName("повтор той же цены не плодит сегменты — как change-only в price_history")
    void repeatedPriceIsCoalesced() {
        ProductState state = new ProductState();
        state.apply(event(T0, 1000));
        // price-events публикуется на КАЖДОМ скрейпе, а price_history пишется
        // только при смене цены. Двадцать одинаковых событий = одна строка в Go.
        for (int day = 1; day <= 20; day++) {
            state.apply(event(plusDays(day), 1000));
        }
        state.apply(event(plusDays(21), 800));

        assertThat(state.segments()).hasSize(2);
        assertThat(state.segments().getFirst().from()).isEqualTo(T0); // начало не потеряно
        assertThat(state.stats(plusDays(22)).median30()).isEqualTo(1000);
        assertThat(state.stats(plusDays(22)).min30()).isEqualTo(800);
    }

    @Test
    @DisplayName("копеечная разница ценой не считается — критерий тот же, что у скрапера")
    void subKopeckDifferenceIsSamePrice() {
        ProductState state = new ProductState();
        state.apply(event(T0, 1499.99));
        state.apply(event(plusDays(1), 1499.994)); // тот же ярлык, другой хвост float

        assertThat(state.segments()).hasSize(1);
    }

    @Test
    @DisplayName("на равенстве половин медиана нижняя — как в SQL cum >= tot/2")
    void medianTakesLowerHalfOnTie() {
        ProductState state = new ProductState();
        state.apply(event(T0, 1000));
        state.apply(event(plusDays(10), 2000));

        // Ровно 10 дней по 1000 и ровно 10 дней по 2000. Go-запрос отдаёт 1000
        // (первая цена, где накопленное время достигло половины). Строгое
        // «больше половины» дало бы 2000 и перевернуло вердикт с «обычная цена»
        // на «выше обычной».
        assertThat(state.stats(plusDays(20)).median30()).isEqualTo(1000);
    }

    @Test
    @DisplayName("apply сообщает, изменил ли он состояние")
    void applyReportsChange() {
        ProductState state = new ProductState();
        assertThat(state.apply(event(T0, 1000))).isTrue();
        assertThat(state.apply(event(plusDays(1), 1000))).isTrue();   // сдвинулся lastEventAt
        assertThat(state.apply(event(T0, 500))).isFalse();            // опоздавшее — ничего не меняет
    }
}
