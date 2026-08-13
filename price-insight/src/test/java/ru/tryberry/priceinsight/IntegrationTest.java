package ru.tryberry.priceinsight;

import org.apache.kafka.clients.producer.KafkaProducer;
import org.apache.kafka.clients.producer.ProducerConfig;
import org.apache.kafka.clients.producer.ProducerRecord;
import org.apache.kafka.common.serialization.StringSerializer;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;
import org.testcontainers.containers.KafkaContainer;
import org.testcontainers.containers.PostgreSQLContainer;
import org.testcontainers.junit.jupiter.Container;
import org.testcontainers.junit.jupiter.Testcontainers;
import org.testcontainers.utility.DockerImageName;
import ru.tryberry.priceinsight.domain.PriceEvent;
import ru.tryberry.priceinsight.domain.PriceVerdict;
import ru.tryberry.priceinsight.serde.JsonSerde;
import ru.tryberry.priceinsight.stream.PriceVerdictMessage;

import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.Properties;
import java.util.UUID;
import java.util.concurrent.TimeUnit;

import static org.assertj.core.api.Assertions.assertThat;
import static org.awaitility.Awaitility.await;

/**
 * Сквозной прогон: событие в Kafka → топология → топик вердиктов → проекция в
 * Postgres. Проверяет стыки, а не логику — логика уже закрыта
 * {@code TopologyTestDriver}-тестами, которым не нужны ни брокер, ни Docker.
 *
 * <p>Пропускается, если Docker недоступен (в WSL нужно включить интеграцию в
 * настройках Docker Desktop). Такой тест не должен ронять сборку там, где
 * демона нет.
 */
@SpringBootTest
@Testcontainers(disabledWithoutDocker = true)
class IntegrationTest {

    @Container
    static final KafkaContainer KAFKA =
            new KafkaContainer(DockerImageName.parse("confluentinc/cp-kafka:7.6.1"));

    @Container
    static final PostgreSQLContainer<?> POSTGRES =
            new PostgreSQLContainer<>(DockerImageName.parse("postgres:16-alpine"));

    @DynamicPropertySource
    static void properties(DynamicPropertyRegistry registry) {
        registry.add("spring.kafka.bootstrap-servers", KAFKA::getBootstrapServers);
        registry.add("spring.datasource.url", POSTGRES::getJdbcUrl);
        registry.add("spring.datasource.username", POSTGRES::getUsername);
        registry.add("spring.datasource.password", POSTGRES::getPassword);
        // Каталог стейта — свой на каждый прогон. Общий путь в /tmp пережил бы
        // тест, и следующий запуск поднялся бы на чекпоинтах от ДРУГОГО брокера:
        // оффсеты чужие, стейт «уже обработан», вердикт не публикуется, тест
        // падает по таймауту на ровном месте.
        registry.add("price-insight.state-dir",
                () -> System.getProperty("java.io.tmpdir") + "/price-insight-it-" + UUID.randomUUID());
        registry.add("price-insight.min-age", () -> "1s"); // не ждать 7 дней в тесте
    }

    @Autowired
    JdbcTemplate jdbc;

    @Autowired
    ru.tryberry.priceinsight.projection.VerdictProjector projector;

    /**
     * Схему берём из боевой миграции, а не из копии в тесте: копия разъедется с
     * оригиналом молча, и тест продолжит зеленеть на схеме, которой нет в проде.
     */
    private void applyMigration() throws Exception {
        String sql = Files.readString(Path.of("../migrations/032_price_insight.sql"));
        String up = sql.substring(sql.indexOf("-- +goose Up"), sql.indexOf("-- +goose Down"))
                .replaceAll("(?m)^-- \\+goose Statement(Begin|End)$", "");
        jdbc.execute(up);
    }

    @Test
    @DisplayName("событие доезжает до таблицы price_insight")
    void endToEnd() throws Exception {
        applyMigration();

        Properties cfg = new Properties();
        cfg.put(ProducerConfig.BOOTSTRAP_SERVERS_CONFIG, KAFKA.getBootstrapServers());
        try (var producer = new KafkaProducer<String, PriceEvent>(
                cfg, new StringSerializer(), new JsonSerde<>(PriceEvent.class).serializer())) {

            Instant start = Instant.now().minus(Duration.ofDays(30));
            producer.send(new ProducerRecord<>("price-events", "42",
                    new PriceEvent(42L, "wb", 0, 1000, start, true, true))).get(10, TimeUnit.SECONDS);
            producer.send(new ProducerRecord<>("price-events", "42",
                    new PriceEvent(42L, "wb", 1000, 700, Instant.now(), true, true))).get(10, TimeUnit.SECONDS);
        }

        await().atMost(Duration.ofSeconds(60)).untilAsserted(() -> {
            var rows = jdbc.queryForList("SELECT verdict, min_all FROM price_insight WHERE product_id = 42");
            assertThat(rows).isNotEmpty();
            assertThat(((Number) rows.getFirst().get("verdict")).intValue())
                    .isEqualTo(PriceVerdict.LOWEST_EVER.code());
            assertThat(((Number) rows.getFirst().get("min_all")).doubleValue()).isEqualTo(700.0);
        });
    }

    @Test
    @DisplayName("порядок топика важнее computed_at: свежий вердикт не отбрасывается")
    void laterRecordWinsEvenWithEarlierComputedAt() throws Exception {
        applyMigration();

        Instant late = Instant.parse("2026-08-13T00:00:00Z");
        Instant early = Instant.parse("2026-08-12T00:00:00Z");

        // Так это и выглядит вживую: сначала тик пунктуатора со stream-time
        // «сегодня», следом — настоящее событие о падении цены, датированное
        // «вчера». Прежний guard (computed_at <= EXCLUDED.computed_at) отбрасывал
        // второе навсегда, и в таблице оставался устаревший минимум.
        projector.apply(List.of(
                new PriceVerdictMessage(4242L, PriceVerdict.TYPICAL.code(), 1000, 1000, 1000, 1000, early, late),
                new PriceVerdictMessage(4242L, PriceVerdict.LOWEST_EVER.code(), 700, 1000, 700, 700, early, early)));

        var row = jdbc.queryForList("SELECT verdict, min_all FROM price_insight WHERE product_id = 4242").getFirst();
        assertThat(((Number) row.get("verdict")).intValue()).isEqualTo(PriceVerdict.LOWEST_EVER.code());
        assertThat(((Number) row.get("min_all")).doubleValue()).isEqualTo(700.0);
    }
}
