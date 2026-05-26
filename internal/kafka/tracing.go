package kafka

import (
	"context"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// kafkaHeadersCarrier — адаптер чтобы OpenTelemetry мог писать/читать
// контекст трейса в Kafka headers (как в HTTP headers).
// Реализует интерфейс propagation.TextMapCarrier.
type kafkaHeadersCarrier struct {
	headers *[]kafka.Header
}

func (c kafkaHeadersCarrier) Get(key string) string {
	for _, h := range *c.headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func (c kafkaHeadersCarrier) Set(key, value string) {
	// Удаляем существующий чтобы не было дублей
	for i, h := range *c.headers {
		if h.Key == key {
			(*c.headers)[i].Value = []byte(value)
			return
		}
	}
	*c.headers = append(*c.headers, kafka.Header{Key: key, Value: []byte(value)})
}

func (c kafkaHeadersCarrier) Keys() []string {
	keys := make([]string, 0, len(*c.headers))
	for _, h := range *c.headers {
		keys = append(keys, h.Key)
	}
	return keys
}

// injectTraceContext кладёт trace context из ctx в headers Kafka сообщения
func injectTraceContext(ctx context.Context, headers *[]kafka.Header) {
	otel.GetTextMapPropagator().Inject(ctx, kafkaHeadersCarrier{headers: headers})
}

// extractTraceContext извлекает trace context из headers Kafka сообщения
// и возвращает обогащённый context
func extractTraceContext(ctx context.Context, headers []kafka.Header) context.Context {
	carrier := kafkaHeadersCarrier{headers: &headers}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// частный type assertion — пакет propagation возвращает интерфейс,
// убедимся что наш тип ему соответствует
var _ propagation.TextMapCarrier = kafkaHeadersCarrier{}
