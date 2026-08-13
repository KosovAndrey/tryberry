package ru.tryberry.priceinsight.projection;

import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.stereotype.Component;
import ru.tryberry.priceinsight.stream.PriceVerdictMessage;

import java.util.List;

/**
 * Батчевый консьюмер {@code price-verdict}. Батч, а не по одному: вердикты
 * приезжают пачками после тика пунктуатора, и построчный round-trip до Postgres
 * на каждом был бы заметно дороже одного batchUpdate.
 */
@Component
public class VerdictListener {

    private static final Logger log = LoggerFactory.getLogger(VerdictListener.class);

    private final VerdictProjector projector;

    public VerdictListener(VerdictProjector projector) {
        this.projector = projector;
    }

    @KafkaListener(
            topics = "${price-insight.output-topic}",
            groupId = "${price-insight.projector-group}",
            batch = "true",
            containerFactory = "verdictListenerContainerFactory")
    public void onBatch(List<ConsumerRecord<String, PriceVerdictMessage>> records) {
        List<PriceVerdictMessage> values = records.stream()
                .map(ConsumerRecord::value)
                .filter(java.util.Objects::nonNull)
                .toList();
        projector.apply(values);
        log.debug("спроецировано вердиктов: {}", values.size());
    }
}
