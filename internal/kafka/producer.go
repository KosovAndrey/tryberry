package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Producer struct {
	writer *kafka.Writer
}

func NewProducer(brokers []string, topic string) *Producer {
	return &Producer{
		writer: &kafka.Writer{
			Addr:         kafka.TCP(brokers...),
			Topic:        topic,
			Balancer:     &kafka.Hash{}, // партиционирование по ключу
			RequiredAcks: kafka.RequireOne,
			WriteTimeout: 10 * time.Second,
			Async:        false, // синхронная запись — знаем что сообщение дошло
		},
	}
}

// Send — отправить сообщение в топик. key используется для партиционирования.
func (p *Producer) Send(ctx context.Context, key string, value any) error {
	// Создаём span для отправки в Kafka
	tracer := otel.Tracer("kafka.producer")
	ctx, span := tracer.Start(ctx, "kafka.send",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.destination.name", p.writer.Topic),
			attribute.String("messaging.kafka.message.key", key),
		),
	)
	defer span.End()

	body, err := json.Marshal(value)
	if err != nil {
		metrics.KafkaMessagesProduced.WithLabelValues(p.writer.Topic, "error").Inc()
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal failed")
		return fmt.Errorf("marshal: %w", err)
	}

	// Инжектим trace context в headers
	var headers []kafka.Header
	injectTraceContext(ctx, &headers)

	err = p.writer.WriteMessages(ctx, kafka.Message{
		Key:     []byte(key),
		Value:   body,
		Headers: headers,
	})
	if err != nil {
		metrics.KafkaMessagesProduced.WithLabelValues(p.writer.Topic, "error").Inc()
		span.RecordError(err)
		span.SetStatus(codes.Error, "write failed")
		return fmt.Errorf("kafka write: %w", err)
	}

	metrics.KafkaMessagesProduced.WithLabelValues(p.writer.Topic, "success").Inc()
	return nil
}

func (p *Producer) Close() error {
	return p.writer.Close()
}
