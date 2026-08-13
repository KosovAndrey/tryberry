package ru.tryberry.priceinsight.stream;

import org.apache.kafka.streams.processor.PunctuationType;
import org.apache.kafka.streams.processor.api.Processor;
import org.apache.kafka.streams.processor.api.ProcessorContext;
import org.apache.kafka.streams.processor.api.Record;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.apache.kafka.streams.state.KeyValueIterator;
import org.apache.kafka.streams.state.KeyValueStore;
import ru.tryberry.priceinsight.domain.HonestPrice;
import ru.tryberry.priceinsight.domain.HonestPriceRules;
import ru.tryberry.priceinsight.domain.PriceEvent;
import ru.tryberry.priceinsight.domain.PriceStats;
import ru.tryberry.priceinsight.domain.ProductState;

import java.time.Duration;
import java.time.Instant;

/**
 * Держит по каждому товару сегментное состояние и материализует вердикт.
 *
 * <p><b>Почему Processor API, а не Streams DSL.</b> DSL агрегирует по tumbling /
 * hopping / session окнам операциями вида count, sum, reduce. Нужная нам медиана
 * взвешена по ДЛИТЕЛЬНОСТИ сегментов, а при change-only хранении длительность
 * никак не выводится из числа записей — стабильный товар даёт одну запись за
 * сорок дней. Такую агрегацию через DSL не выразить, отсюда собственный store и
 * ручное управление состоянием.
 *
 * <p><b>Зачем пунктуатор.</b> Окно едет по времени само: товар может не
 * обновляться неделями, а его 30-дневный минимум за это время выйдет из окна и
 * вердикт устареет. Событий, которые бы это заметили, не приходит — значит нужен
 * тик по времени.
 */
public class PriceInsightProcessor implements Processor<String, PriceEvent, String, PriceVerdictMessage> {

    private static final Logger log = LoggerFactory.getLogger(PriceInsightProcessor.class);

    private final String storeName;
    private final Duration punctuateInterval;
    private final HonestPriceRules rules;

    private ProcessorContext<String, PriceVerdictMessage> context;
    private KeyValueStore<String, ProductState.Snapshot> store;

    public PriceInsightProcessor(String storeName, Duration punctuateInterval, HonestPriceRules rules) {
        this.storeName = storeName;
        this.punctuateInterval = punctuateInterval;
        this.rules = rules;
    }

    @Override
    public void init(ProcessorContext<String, PriceVerdictMessage> context) {
        this.context = context;
        this.store = context.getStateStore(storeName);
        // STREAM_TIME, а не WALL_CLOCK_TIME: при переигрывании топика с начала
        // (пересборка стейта) настенное время дало бы один тик на всю историю,
        // а stream-time идёт вместе с данными и воспроизводит ход окна.
        context.schedule(punctuateInterval, PunctuationType.STREAM_TIME, this::punctuate);
    }

    @Override
    public void process(Record<String, PriceEvent> record) {
        PriceEvent event = record.value();
        if (event == null || record.key() == null) {
            return; // tombstone или отсутствующий ключ — пропускаем
        }
        Long productId = parseProductId(record.key());
        if (productId == null) {
            return;
        }
        ProductState state = ProductState.fromSnapshot(store.get(record.key()));
        // Событие может ничего не изменить (опоздавшее или дубль) — тогда и писать
        // обратно нечего: при exactly_once_v2 каждый put уезжает в changelog-топик.
        if (!state.apply(event)) {
            return;
        }

        // Оцениваем по текущей цене состояния, а не по цене события: событие
        // «нет в наличии» несёт ноль, и оценивать по нему нечего.
        emitIfChanged(record.key(), productId, state, event.recordedAt());
        store.put(record.key(), state.toSnapshot());
    }

    /**
     * Проходит по всем товарам, двигает окно и переоценивает.
     *
     * <p>Полный обход store — O(числа товаров) на тик. При текущем масштабе это
     * дешёвый range scan по RocksDB, а интервал берётся часами, не секундами.
     * Если товаров станет заметно больше, обход надо будет заменить очередью
     * «когда следующий раз пересчитать» — но городить её заранее незачем.
     *
     * <p>Обратно в store пишем ТОЛЬКО при фактическом изменении: при
     * {@code exactly_once_v2} каждая запись уходит в changelog-топик, и
     * безусловный put по всему каталогу раз в тик — это трафик на ровном месте.
     */
    private void punctuate(long streamTimeMs) {
        Instant now = Instant.ofEpochMilli(streamTimeMs);
        try (KeyValueIterator<String, ProductState.Snapshot> it = store.all()) {
            while (it.hasNext()) {
                var entry = it.next();
                Long productId = parseProductId(entry.key);
                if (productId == null) {
                    continue;
                }
                ProductState state = ProductState.fromSnapshot(entry.value);
                boolean pruned = state.prune(now);
                boolean emitted = emitIfChanged(entry.key, productId, state, now);
                if (pruned || emitted) {
                    store.put(entry.key, state.toSnapshot());
                }
            }
        }
    }

    /**
     * Считает вердикт и публикует, если он изменился.
     *
     * <p>Когда товара нет в наличии, текущей цены не существует и вердикт
     * посчитать не из чего. Публиковать в этом случае {@code INSUFFICIENT} было
     * бы неверно: это значение означает «данных мало», а данные у нас есть —
     * просто товар временно недоступен. Молчим и оставляем последний известный
     * вердикт, он снова обновится, когда товар вернётся.
     */
    private boolean emitIfChanged(String key, long productId, ProductState state, Instant now) {
        double current = state.currentPrice();
        if (current <= 0) {
            return false;
        }
        PriceStats stats = state.stats(now);
        HonestPrice assessed = rules.assess(current, stats, now);
        if (!state.shouldEmit(assessed)) {
            return false;
        }
        context.forward(new Record<>(key, PriceVerdictMessage.of(productId, assessed, now), now.toEpochMilli()));
        return true;
    }

    /** Ключ приходит из Go как strconv.FormatInt — см. cmd/scraper/main.go. */
    private Long parseProductId(String key) {
        try {
            return Long.parseLong(key);
        } catch (NumberFormatException e) {
            // Не роняем поток из-за одной битой записи, но и не прячем её.
            log.warn("ключ не разбирается как product_id, запись пропущена: {}", key);
            return null;
        }
    }
}
