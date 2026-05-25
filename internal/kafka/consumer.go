package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
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

// Run — читает сообщения и вызывает handler. Коммитит offset только после
// успешной обработки (at-least-once семантика).
// Блокирует до отмены контекста.
func (c *Consumer) Run(ctx context.Context, handler HandlerFunc) error {
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // graceful shutdown
			}
			return fmt.Errorf("fetch message: %w", err)
		}

		if err := handler(ctx, Message{Key: msg.Key, Value: msg.Value}); err != nil {
			// Логируем но не коммитим — сообщение будет перечитано
			fmt.Printf("handler error (will retry): %v\n", err)
			continue
		}

		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("commit message: %w", err)
		}
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
