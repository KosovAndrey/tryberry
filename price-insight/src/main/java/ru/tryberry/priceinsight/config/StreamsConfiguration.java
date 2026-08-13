package ru.tryberry.priceinsight.config;

import io.micrometer.core.instrument.MeterRegistry;
import io.micrometer.core.instrument.binder.kafka.KafkaStreamsMetrics;
import org.apache.kafka.clients.admin.NewTopic;
import org.apache.kafka.clients.consumer.ConsumerConfig;
import org.apache.kafka.common.config.TopicConfig;
import org.apache.kafka.streams.KafkaStreams;
import org.apache.kafka.streams.StreamsConfig;
import org.apache.kafka.streams.Topology;
import org.apache.kafka.streams.errors.LogAndContinueExceptionHandler;
import org.apache.kafka.streams.errors.StreamsUncaughtExceptionHandler;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.context.properties.EnableConfigurationProperties;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.kafka.annotation.EnableKafka;
import org.springframework.kafka.config.ConcurrentKafkaListenerContainerFactory;
import org.springframework.kafka.core.ConsumerFactory;
import org.springframework.kafka.core.DefaultKafkaConsumerFactory;
import org.springframework.kafka.config.TopicBuilder;
import ru.tryberry.priceinsight.domain.HonestPriceRules;
import ru.tryberry.priceinsight.serde.JsonSerde;
import ru.tryberry.priceinsight.stream.PriceInsightTopology;
import ru.tryberry.priceinsight.stream.PriceVerdictMessage;

import java.util.HashMap;
import java.util.Map;
import java.util.Properties;

@Configuration
@EnableKafka
@EnableConfigurationProperties(PriceInsightProperties.class)
public class StreamsConfiguration {

    private static final Logger log = LoggerFactory.getLogger(StreamsConfiguration.class);

    @Value("${spring.kafka.bootstrap-servers}")
    private String bootstrapServers;

    /**
     * Выходной топик заводим сами: автосоздание топиков в проде может быть
     * выключено, а compaction тут не опция — в price-verdict нужен ровно
     * последний вердикт по товару, история не нужна и только жрала бы диск.
     */
    @Bean
    public NewTopic priceVerdictTopic(PriceInsightProperties props) {
        return TopicBuilder.name(props.outputTopic())
                .partitions(10)   // как у price-events: тот же ключ, то же деление
                .replicas(1)
                .config(TopicConfig.CLEANUP_POLICY_CONFIG, TopicConfig.CLEANUP_POLICY_COMPACT)
                .build();
    }

    @Bean
    public Topology topology(PriceInsightProperties props) {
        return PriceInsightTopology.build(
                props.inputTopic(),
                props.outputTopic(),
                props.punctuateInterval(),
                new HonestPriceRules(props.minAge()));
    }

    @Bean(destroyMethod = "close")
    public KafkaStreams kafkaStreams(Topology topology,
                                     PriceInsightProperties props,
                                     MeterRegistry registry) {
        Properties cfg = new Properties();
        cfg.put(StreamsConfig.APPLICATION_ID_CONFIG, props.applicationId());
        cfg.put(StreamsConfig.BOOTSTRAP_SERVERS_CONFIG, bootstrapServers);
        cfg.put(StreamsConfig.STATE_DIR_CONFIG, props.stateDir());

        // Запись в выходной топик, обновление changelog стейта и коммит оффсета
        // входного топика — одна транзакция Kafka. При падении между ними
        // инстанс поднимается на согласованном состоянии: без задвоенных
        // вердиктов и без пропущенных событий.
        cfg.put(StreamsConfig.PROCESSING_GUARANTEE_CONFIG, StreamsConfig.EXACTLY_ONCE_V2);

        // Читаем только закоммиченное — иначе транзакционный продюсер выше по
        // потоку мог бы подсунуть нам откатившиеся записи.
        cfg.put(StreamsConfig.consumerPrefix(ConsumerConfig.ISOLATION_LEVEL_CONFIG), "read_committed");

        // С начала: топик price-events для нас — источник для пересборки стейта,
        // а не «слушаем новое». При первом запуске надо прожевать всю историю.
        cfg.put(StreamsConfig.consumerPrefix(ConsumerConfig.AUTO_OFFSET_RESET_CONFIG), "earliest");

        // Битое сообщение пропускаем, а не роняем поток. По умолчанию Streams
        // валит поток на ошибке десериализации, а наш обработчик поднимает его
        // заново (REPLACE_THREAD) — и всё встаёт в бесконечный цикл на одной и
        // той же записи. Продукт от пропуска не страдает: источник правды в
        // price_history, вердикт по товару догонит следующее событие.
        cfg.put(StreamsConfig.DEFAULT_DESERIALIZATION_EXCEPTION_HANDLER_CLASS_CONFIG,
                LogAndContinueExceptionHandler.class);

        // Потолок вне-heap памяти RocksDB — см. BoundedMemoryRocksDBConfig.
        // Без него десять партиций съедают около гигабайта мимо heap и упираются
        // в mem_limit контейнера.
        cfg.put(StreamsConfig.ROCKSDB_CONFIG_SETTER_CLASS_CONFIG, BoundedMemoryRocksDBConfig.class);

        KafkaStreams streams = new KafkaStreams(topology, cfg);

        // Метрики Streams уезжают в тот же Prometheus, что и остальной проект:
        // lag по партициям, размер стейта, время обработки записи.
        new KafkaStreamsMetrics(streams).bindTo(registry);

        streams.setUncaughtExceptionHandler(e -> {
            log.error("необработанное исключение в streams-потоке", e);
            // REPLACE_THREAD, а не SHUTDOWN_CLIENT: единичный сбой потока не
            // должен ронять сервис — стейт восстановится из changelog.
            return StreamsUncaughtExceptionHandler.StreamThreadExceptionResponse.REPLACE_THREAD;
        });
        streams.start();
        return streams;
    }

    /** Отдельная фабрика для батчевого консьюмера проекции в Postgres. */
    @Bean
    public ConcurrentKafkaListenerContainerFactory<String, PriceVerdictMessage>
    verdictListenerContainerFactory(PriceInsightProperties props) {
        Map<String, Object> cfg = new HashMap<>();
        cfg.put(ConsumerConfig.BOOTSTRAP_SERVERS_CONFIG, bootstrapServers);
        cfg.put(ConsumerConfig.GROUP_ID_CONFIG, props.projectorGroup());
        cfg.put(ConsumerConfig.AUTO_OFFSET_RESET_CONFIG, "earliest");
        cfg.put(ConsumerConfig.ISOLATION_LEVEL_CONFIG, "read_committed");
        cfg.put(ConsumerConfig.MAX_POLL_RECORDS_CONFIG, 500);

        ConsumerFactory<String, PriceVerdictMessage> factory = new DefaultKafkaConsumerFactory<>(
                cfg,
                new org.apache.kafka.common.serialization.StringDeserializer(),
                new JsonSerde<>(PriceVerdictMessage.class).deserializer());

        var containerFactory = new ConcurrentKafkaListenerContainerFactory<String, PriceVerdictMessage>();
        containerFactory.setConsumerFactory(factory);
        containerFactory.setBatchListener(true);
        return containerFactory;
    }
}
