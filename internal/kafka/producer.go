package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
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
	body, err := json.Marshal(value)
	if err != nil {
		metrics.KafkaMessagesProduced.WithLabelValues(p.writer.Topic, "error").Inc()
		return fmt.Errorf("marshal: %w", err)
	}

	err = p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(key),
		Value: body,
	})
	if err != nil {
		metrics.KafkaMessagesProduced.WithLabelValues(p.writer.Topic, "error").Inc()
		return fmt.Errorf("kafka write: %w", err)
	}

	metrics.KafkaMessagesProduced.WithLabelValues(p.writer.Topic, "success").Inc()
	return nil
}

func (p *Producer) Close() error {
	return p.writer.Close()
}
