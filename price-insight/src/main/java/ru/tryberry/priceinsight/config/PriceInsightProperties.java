package ru.tryberry.priceinsight.config;

import org.springframework.boot.context.properties.ConfigurationProperties;

import java.time.Duration;

/**
 * Настройки сервиса. Значения по умолчанию совпадают с прод-конфигурацией:
 * {@code minAge} — 7 дней, как {@code honestMinAge} в Go.
 */
@ConfigurationProperties(prefix = "price-insight")
public record PriceInsightProperties(
        String applicationId,
        String inputTopic,
        String outputTopic,
        String projectorGroup,
        Duration punctuateInterval,
        Duration minAge,
        String stateDir
) {
    public PriceInsightProperties {
        applicationId = applicationId == null ? "price-insight" : applicationId;
        inputTopic = inputTopic == null ? "price-events" : inputTopic;
        outputTopic = outputTopic == null ? "price-verdict" : outputTopic;
        projectorGroup = projectorGroup == null ? "price-insight-projector" : projectorGroup;
        punctuateInterval = punctuateInterval == null ? Duration.ofHours(6) : punctuateInterval;
        minAge = minAge == null ? Duration.ofDays(7) : minAge;
        stateDir = stateDir == null ? "/var/lib/price-insight" : stateDir;
    }
}
