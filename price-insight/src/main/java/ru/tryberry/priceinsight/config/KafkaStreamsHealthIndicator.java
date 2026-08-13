package ru.tryberry.priceinsight.config;

import org.apache.kafka.streams.KafkaStreams;
import org.springframework.boot.actuate.health.Health;
import org.springframework.boot.actuate.health.HealthIndicator;
import org.springframework.stereotype.Component;

/**
 * Состояние потока в {@code /actuator/health}. Без него readiness отвечал бы UP
 * по факту поднятого Spring-контекста: топология могла умереть (state DEAD, все
 * потоки исчерпали ретраи), сервис не обрабатывал бы ни одного события, а
 * healthcheck в docker-compose продолжал бы рапортовать «здоров».
 *
 * <p>{@code REBALANCING} считаем живым: это штатное состояние при старте и при
 * восстановлении стейта из changelog, а не отказ.
 *
 * <p>Имя бина НЕ задаём явно: Spring снимает суффикс {@code HealthIndicator} и
 * зовёт контрибьютор {@code kafkaStreams} — ровно то имя, что перечислено в
 * группе readiness. Явное {@code @Component("kafkaStreams")} столкнулось бы с
 * бином самого потока и уронило контекст на старте.
 */
@Component
public class KafkaStreamsHealthIndicator implements HealthIndicator {

    private final KafkaStreams streams;

    public KafkaStreamsHealthIndicator(KafkaStreams streams) {
        this.streams = streams;
    }

    @Override
    public Health health() {
        KafkaStreams.State state = streams.state();
        Health.Builder builder = state.isRunningOrRebalancing() ? Health.up() : Health.down();
        return builder.withDetail("state", state.name()).build();
    }
}
