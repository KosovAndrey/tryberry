package ru.tryberry.priceinsight.stream;

import org.apache.kafka.common.serialization.Serdes;
import org.apache.kafka.streams.Topology;
import org.apache.kafka.streams.state.KeyValueStore;
import org.apache.kafka.streams.state.StoreBuilder;
import org.apache.kafka.streams.state.Stores;
import ru.tryberry.priceinsight.domain.HonestPriceRules;
import ru.tryberry.priceinsight.domain.PriceEvent;
import ru.tryberry.priceinsight.domain.ProductState;
import ru.tryberry.priceinsight.serde.JsonSerde;

import java.time.Duration;

/**
 * Сборка топологии. Вынесена отдельно от Spring-конфигурации, чтобы её можно
 * было гонять на {@code TopologyTestDriver} — без брокера, без контейнеров и
 * без поднятия контекста приложения.
 */
public final class PriceInsightTopology {

    public static final String STORE = "product-state";

    private PriceInsightTopology() {
    }

    public static Topology build(String inputTopic,
                                 String outputTopic,
                                 Duration punctuateInterval,
                                 HonestPriceRules rules) {

        StoreBuilder<KeyValueStore<String, ProductState.Snapshot>> store =
                Stores.keyValueStoreBuilder(
                        Stores.persistentKeyValueStore(STORE),
                        Serdes.String(),
                        new JsonSerde<>(ProductState.Snapshot.class));

        Topology topology = new Topology();
        topology
                .addSource("price-events",
                        Serdes.String().deserializer(),
                        new JsonSerde<>(PriceEvent.class).deserializer(),
                        inputTopic)
                .addProcessor("assess",
                        () -> new PriceInsightProcessor(STORE, punctuateInterval, rules),
                        "price-events")
                .addStateStore(store, "assess")
                .addSink("price-verdict",
                        outputTopic,
                        Serdes.String().serializer(),
                        new JsonSerde<>(PriceVerdictMessage.class).serializer(),
                        "assess");
        return topology;
    }
}
