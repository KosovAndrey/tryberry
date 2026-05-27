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

type Message struct {
	Key   []byte
	Value []byte
}

// HandlerFunc — функция обработки одного сообщения.
// Если вернула ошибку — offset не коммитится, сообщение будет перечитано.
type HandlerFunc func(ctx context.Context, msg Message) error

type Consumer struct {
	reader *kafka.Reader
}

func NewConsumer(brokers []string, topic, groupID string) *Consumer {
	return &Consumer{
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:        brokers,
			Topic:          topic,
			GroupID:        groupID,
			MinBytes:       1,
			MaxBytes:       10e6, // 10 MB
			CommitInterval: 0,    // явный commit после обработки
			StartOffset:    kafka.FirstOffset,
			MaxWait:        time.Second,
		}),
	}
}

// Параметры экспоненциального backoff'а для transient ошибок Kafka.
// Срабатывает при leader election, перезапуске координатора, временных сетевых
// проблемах. Backoff удваивается до maxBackoff, затем держит максимум.
const (
	minBackoff = 1 * time.Second
	maxBackoff = 30 * time.Second
)

// nextBackoff удваивает текущую паузу с capper на maxBackoff.
func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}

// sleepWithCtx ждёт d или отмену контекста — что наступит раньше.
// Возвращает true если контекст отменён (пора выходить).
func sleepWithCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

// Run — читает сообщения и вызывает handler. Коммитит offset только после
// успешной обработки (at-least-once семантика).
// При transient ошибках Kafka (leader election, coordinator down, network
// flaps) делает exponential backoff и продолжает работать — НЕ убивает процесс.
// Возвращается только при отмене контекста.
func (c *Consumer) Run(ctx context.Context, handler HandlerFunc) error {
	topic := c.reader.Config().Topic
	tracer := otel.Tracer("kafka.consumer")

	backoff := minBackoff

	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Transient ошибка — логируем, ждём, повторяем.
			// Reader сам переподключится при следующем вызове.
			metrics.KafkaMessagesConsumed.WithLabelValues(topic, "error").Inc()
			fmt.Printf("kafka fetch error: %v, retrying in %s\n", err, backoff)
			if sleepWithCtx(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}
		// Успешный fetch — сбрасываем backoff
		backoff = minBackoff

		// Извлекаем trace context из headers сообщения
		// Это связывает спан consumer'а с трейсом producer'а
		msgCtx := extractTraceContext(ctx, msg.Headers)

		msgCtx, span := tracer.Start(msgCtx, "kafka.receive",
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.system", "kafka"),
				attribute.String("messaging.destination.name", topic),
			),
		)

		start := time.Now()
		handlerErr := handler(msgCtx, Message{Key: msg.Key, Value: msg.Value})
		metrics.KafkaProcessingDuration.WithLabelValues(topic).Observe(time.Since(start).Seconds())

		if handlerErr != nil {
			metrics.KafkaMessagesConsumed.WithLabelValues(topic, "error").Inc()
			span.RecordError(handlerErr)
			span.SetStatus(codes.Error, "handler failed")
			span.End()
			fmt.Printf("handler error (will retry): %v\n", handlerErr)
			continue
		}

		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			span.End()
			if ctx.Err() != nil {
				return nil
			}
			// Commit упал по transient причине — при at-least-once это безопасно:
			// сообщение придёт ещё раз и обработается снова (idempotency на handler).
			// Логируем и идём дальше — не убиваем процесс.
			fmt.Printf("kafka commit error: %v (message will be redelivered)\n", err)
			continue
		}

		metrics.KafkaMessagesConsumed.WithLabelValues(topic, "success").Inc()
		span.End()
	}
}

func (c *Consumer) Close() error {
	return c.reader.Close()
}

// Decode — хелпер для десериализации тела сообщения
func Decode[T any](msg Message) (T, error) {
	var v T
	if err := json.Unmarshal(msg.Value, &v); err != nil {
		return v, fmt.Errorf("kafka decode: %w", err)
	}
	return v, nil
}
