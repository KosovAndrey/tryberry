package ru.tryberry.priceinsight.serde;

import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.datatype.jsr310.JavaTimeModule;
import org.apache.kafka.common.errors.SerializationException;
import org.apache.kafka.common.serialization.Deserializer;
import org.apache.kafka.common.serialization.Serde;
import org.apache.kafka.common.serialization.Serializer;

/**
 * JSON-сериализация для сообщений и для состояния.
 *
 * <p>Выбран JSON, а не Avro со Schema Registry: Go-сторона уже пишет в
 * price-events обычный JSON, а Schema Registry — ещё один компонент в
 * инфраструктуре ради одного потребителя. Пересмотреть, когда появится второй.
 * См. docs/PRICE-INSIGHT-JAVA.md §12.
 *
 * <p>{@code FAIL_ON_UNKNOWN_PROPERTIES} выключен намеренно: поля события
 * аддитивны, Go-сторона может дописать новое поле и выкатиться раньше нас.
 */
public final class JsonSerde<T> implements Serde<T> {

    private static final ObjectMapper MAPPER = new ObjectMapper()
            .registerModule(new JavaTimeModule())
            .disable(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES)
            .disable(com.fasterxml.jackson.databind.SerializationFeature.WRITE_DATES_AS_TIMESTAMPS);

    private final Class<T> type;

    public JsonSerde(Class<T> type) {
        this.type = type;
    }

    public static ObjectMapper mapper() {
        return MAPPER;
    }

    @Override
    public Serializer<T> serializer() {
        return (topic, data) -> {
            if (data == null) {
                return null;
            }
            try {
                return MAPPER.writeValueAsBytes(data);
            } catch (Exception e) {
                throw new SerializationException("не удалось сериализовать " + type.getSimpleName(), e);
            }
        };
    }

    @Override
    public Deserializer<T> deserializer() {
        return (topic, bytes) -> {
            if (bytes == null || bytes.length == 0) {
                return null;
            }
            try {
                return MAPPER.readValue(bytes, type);
            } catch (Exception e) {
                throw new SerializationException("не удалось разобрать " + type.getSimpleName(), e);
            }
        };
    }
}
