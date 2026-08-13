package ru.tryberry.priceinsight.config;

import org.apache.kafka.streams.state.RocksDBConfigSetter;
import org.rocksdb.BlockBasedTableConfig;
import org.rocksdb.Cache;
import org.rocksdb.LRUCache;
import org.rocksdb.Options;
import org.rocksdb.WriteBufferManager;

import java.util.Map;

/**
 * Потолок ВНЕ-heap памяти RocksDB, общий на все партиции.
 *
 * <p><b>Зачем.</b> По умолчанию Kafka Streams заводит отдельный экземпляр RocksDB
 * на КАЖДУЮ партицию входного топика, и у каждого — свой block cache (50 МБ) и
 * свои memtable (3 × 16 МБ). На десяти партициях price-events это около гигабайта
 * вне heap — ровно столько, сколько у контейнера выставлено лимитом целиком
 * ({@code mem_limit: 1g} в docker-compose.yml). Heap при этом живёт по
 * {@code MaxRAMPercentage} и о внешней памяти ничего не знает, так что процесс
 * молча упёрся бы в cgroup-лимит и был бы убит OOM-killer'ом — тем более обидно,
 * что стейт у нас крошечный (сегменты цен, а не события).
 *
 * <p><b>Как.</b> Один {@link LRUCache} и один {@link WriteBufferManager} на весь
 * процесс: {@code static}, потому что Streams создаёт по объекту этого класса на
 * каждый store и общий потолок иначе не выразить. Memtable учитываются в том же
 * кэше — так суммарный расход ограничен сверху одним числом.
 */
public class BoundedMemoryRocksDBConfig implements RocksDBConfigSetter {

    /** Суммарный потолок block cache + memtable на все партиции. */
    private static final long TOTAL_OFF_HEAP_BYTES = 128L * 1024 * 1024;

    /** Доля потолка под memtable (write buffer). Остальное — на чтение. */
    private static final long TOTAL_MEMTABLE_BYTES = 32L * 1024 * 1024;

    /** Доля кэша, зарезервированная под индексы и фильтры (high priority pool). */
    private static final double INDEX_FILTER_RATIO = 0.1;

    private static final Cache CACHE = new LRUCache(TOTAL_OFF_HEAP_BYTES, -1, false, INDEX_FILTER_RATIO);
    private static final WriteBufferManager WRITE_BUFFER_MANAGER =
            new WriteBufferManager(TOTAL_MEMTABLE_BYTES, CACHE);

    @Override
    public void setConfig(String storeName, Options options, Map<String, Object> configs) {
        BlockBasedTableConfig table = (BlockBasedTableConfig) options.tableFormatConfig();
        table.setBlockCache(CACHE);
        // Индексы и фильтры тоже через кэш — иначе они растут вне всякого потолка.
        table.setCacheIndexAndFilterBlocks(true);
        table.setCacheIndexAndFilterBlocksWithHighPriority(true);
        table.setPinTopLevelIndexAndFilter(true);
        options.setTableFormatConfig(table);

        options.setWriteBufferManager(WRITE_BUFFER_MANAGER);
        options.setWriteBufferSize(8L * 1024 * 1024);
        options.setMaxWriteBufferNumber(2);
    }

    @Override
    public void close(String storeName, Options options) {
        // CACHE и WRITE_BUFFER_MANAGER общие на процесс: закрыть их при остановке
        // одного store значит выдернуть память из-под остальных партиций.
    }
}
