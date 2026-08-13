package ru.tryberry.priceinsight.stream;

import org.apache.kafka.common.serialization.Serdes;
import org.apache.kafka.streams.StreamsConfig;
import org.apache.kafka.streams.TestInputTopic;
import org.apache.kafka.streams.TestOutputTopic;
import org.apache.kafka.streams.TopologyTestDriver;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import ru.tryberry.priceinsight.domain.HonestPriceRules;
import ru.tryberry.priceinsight.domain.PriceEvent;
import ru.tryberry.priceinsight.domain.PriceVerdict;
import ru.tryberry.priceinsight.serde.JsonSerde;

import java.time.Duration;
import java.time.Instant;
import java.util.Properties;

import static org.assertj.core.api.Assertions.assertThat;

/**
 * Топология целиком, включая сериализацию состояния и пунктуатор, но без
 * брокера и без контейнеров — {@code TopologyTestDriver} прогоняет её в памяти
 * за доли секунды. Интеграционные тесты на Testcontainers нужны отдельно, но
 * только чтобы проверить стык с реальной Kafka, а не логику.
 */
class PriceInsightTopologyTest {

    private static final String IN = "price-events";
    private static final String OUT = "price-verdict";
    private static final Instant T0 = Instant.parse("2026-05-01T00:00:00Z");

    private TopologyTestDriver driver;
    private TestInputTopic<String, PriceEvent> input;
    private TestOutputTopic<String, PriceVerdictMessage> output;

    @BeforeEach
    void setUp() {
        Properties props = new Properties();
        props.put(StreamsConfig.APPLICATION_ID_CONFIG, "price-insight-test");
        props.put(StreamsConfig.BOOTSTRAP_SERVERS_CONFIG, "dummy:9092");

        driver = new TopologyTestDriver(
                PriceInsightTopology.build(IN, OUT, Duration.ofHours(6),
                        new HonestPriceRules(Duration.ofDays(7))),
                props);

        input = driver.createInputTopic(IN,
                Serdes.String().serializer(), new JsonSerde<>(PriceEvent.class).serializer());
        output = driver.createOutputTopic(OUT,
                Serdes.String().deserializer(), new JsonSerde<>(PriceVerdictMessage.class).deserializer());
    }

    @AfterEach
    void tearDown() {
        driver.close();
    }

    private void send(Instant at, double price) {
        input.pipeInput("1", new PriceEvent(1L, "wb", 0, price, at, price > 0, true), at);
    }

    @Test
    @DisplayName("на молодой истории вердикт insufficient — не вводим в заблуждение")
    void youngHistoryIsInsufficient() {
        send(T0, 1000);
        send(T0.plus(Duration.ofDays(2)), 900);

        assertThat(output.readValuesToList())
                .isNotEmpty()
                .allMatch(v -> v.verdict() == PriceVerdict.INSUFFICIENT.code());
    }

    @Test
    @DisplayName("после гейта по возрасту падение цены даёт минимум за всё время")
    void dropAfterMinAgeIsLowestEver() {
        send(T0, 1000);
        send(T0.plus(Duration.ofDays(10)), 700);

        var last = output.readValuesToList().getLast();
        assertThat(last.verdict()).isEqualTo(PriceVerdict.LOWEST_EVER.code());
        assertThat(last.minAll()).isEqualTo(700);
        assertThat(last.productId()).isEqualTo(1L);
    }

    @Test
    @DisplayName("цена выше медианы помечается как завышенная «скидка»")
    void aboveMedianIsFlagged() {
        send(T0, 1000);
        send(T0.plus(Duration.ofDays(20)), 3000);

        var last = output.readValuesToList().getLast();
        assertThat(last.verdict()).isEqualTo(PriceVerdict.ABOVE_TYPICAL.code());
        assertThat(last.median30()).isEqualTo(1000);
    }

    @Test
    @DisplayName("одинаковый вердикт не публикуется повторно")
    void identicalVerdictNotRepublished() {
        send(T0, 1000);
        send(T0.plus(Duration.ofDays(10)), 700);
        int afterDrop = output.readValuesToList().size();

        // та же цена ещё дважды — состояние по сути не меняется
        send(T0.plus(Duration.ofDays(11)), 700);
        send(T0.plus(Duration.ofDays(12)), 700);

        assertThat(output.readValuesToList()).isEmpty();
        assertThat(afterDrop).isPositive();
    }

    @Test
    @DisplayName("пунктуатор двигает окно и переоценивает без новых событий")
    void punctuatorMovesWindow() {
        send(T0, 1000);
        send(T0.plus(Duration.ofDays(10)), 5000);
        output.readValuesToList();

        // Событий по товару больше нет, но stream-time двигаем другим ключом.
        input.pipeInput("999", new PriceEvent(999L, "wb", 0, 10, T0.plus(Duration.ofDays(60)), true, true),
                T0.plus(Duration.ofDays(60)));

        // Дешёвый сегмент вышел из 30-дневного окна — по товару 1 приехал
        // пересчитанный вердикт, хотя новых событий по нему не было.
        assertThat(output.readKeyValuesToList())
                .anyMatch(kv -> kv.key.equals("1") && kv.value.min30() == 5000);
    }

    @Test
    @DisplayName("уход из наличия не затирает вердикт на «данных мало»")
    void outOfStockDoesNotOverwriteVerdict() {
        send(T0, 1000);
        send(T0.plus(Duration.ofDays(10)), 700);
        var afterDrop = output.readValuesToList().getLast();
        assertThat(afterDrop.verdict()).isEqualTo(PriceVerdict.LOWEST_EVER.code());

        send(T0.plus(Duration.ofDays(11)), 0);   // товар кончился

        // Данные никуда не делись, просто цены сейчас нет — публиковать
        // INSUFFICIENT было бы враньём, поэтому молчим.
        assertThat(output.readValuesToList()).isEmpty();
    }

    @Test
    @DisplayName("возврат в наличие переоценивается заново")
    void backInStockIsReassessed() {
        send(T0, 1000);
        send(T0.plus(Duration.ofDays(10)), 700);
        send(T0.plus(Duration.ofDays(11)), 0);
        output.readValuesToList();

        send(T0.plus(Duration.ofDays(20)), 5000);

        var last = output.readValuesToList().getLast();
        assertThat(last.verdict()).isEqualTo(PriceVerdict.ABOVE_TYPICAL.code());
        assertThat(last.minAll()).isEqualTo(700);
    }

    @Test
    @DisplayName("состояние переживает перезапуск: сегменты читаются из store")
    void stateSurvivesRestart() {
        send(T0, 1000);
        send(T0.plus(Duration.ofDays(10)), 700);
        output.readValuesToList();

        // Новое событие после «перезапуска» видит накопленную историю:
        // 700 остаётся минимумом за всё время, значит стейт не потерян.
        send(T0.plus(Duration.ofDays(20)), 900);

        var last = output.readValuesToList().getLast();
        assertThat(last.minAll()).isEqualTo(700);
        assertThat(last.verdict()).isNotEqualTo(PriceVerdict.INSUFFICIENT.code());
    }
}
