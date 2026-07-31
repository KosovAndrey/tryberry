package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
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

// commitFlushInterval — как часто конкурентный консьюмер сбрасывает накопленные
// вотермарки оффсетов в Kafka. Батчим, чтобы не делать по commit-RPC на каждое
// сообщение под высокой пропускной. Потеря окна ≤ этого интервала при падении
// безопасна (at-least-once, handler идемпотентен).
const commitFlushInterval = 500 * time.Millisecond

// RunConcurrent — как Run, но обрабатывает до concurrency сообщений параллельно
// внутри ОДНОГО инстанса. Скрейп I/O-bound (ждём маркетплейс), CPU простаивает,
// поэтому пул горутин даёт ×N к пропускной без доп. реплик.
//
// Коммит оффсетов — вотермарком непрерывного префикса на партицию: оффсет
// коммитится, только когда ВСЕ предшествующие ему сообщения этой партиции
// завершили handler. Так ни одно сообщение не «перепрыгнет» коммитом ещё не
// обработанное. At-least-once сохраняется: при падении незакоммиченные
// сообщения перечитаются и обработаются повторно (апсерты идемпотентны).
//
// Ошибка handler'а НЕ стопорит партицию: оффсет всё равно продвигается (как и в
// последовательном Run, где после ошибки читается следующее сообщение и провал
// эффективно пропускается) — товар/задача перепланируются штатным кадансом.
// Порядок обработки в партиции не гарантируется — для идемпотентного скрейпа не важно.
//
// concurrency<=1 эквивалентно последовательному Run.
func (c *Consumer) RunConcurrent(ctx context.Context, handler HandlerFunc, concurrency int) error {
	if concurrency <= 1 {
		return c.Run(ctx, handler)
	}
	topic := c.reader.Config().Topic
	tracer := otel.Tracer("kafka.consumer")

	tr := newOffsetTracker()
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	// Коммиттер: периодически сбрасывает вотермарки партиций одним CommitMessages.
	commitStopped := make(chan struct{})
	go func() {
		defer close(commitStopped)
		t := time.NewTicker(commitFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.flushCommits(ctx, tr)
				metrics.KafkaPartitionStuck.WithLabelValues(topic).Set(tr.stuckSeconds(time.Now()))
			}
		}
	}()

	backoff := minBackoff
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			metrics.KafkaMessagesConsumed.WithLabelValues(topic, "error").Inc()
			fmt.Printf("kafka fetch error: %v, retrying in %s\n", err, backoff)
			if sleepWithCtx(ctx, backoff) {
				break
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = minBackoff

		// Инициализация курсора партиции первым (низшим) оффсетом — из читающей
		// горутины, где оффсеты партии идут по возрастанию.
		tr.register(msg.Partition, msg.Offset)

		sem <- struct{}{}
		wg.Add(1)
		metrics.KafkaInflight.WithLabelValues(topic).Inc()
		go func(msg kafka.Message) {
			defer wg.Done()
			defer func() { <-sem }()
			defer metrics.KafkaInflight.WithLabelValues(topic).Dec()

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
				fmt.Printf("handler error (offset advanced, task will be rescheduled): %v\n", handlerErr)
			} else {
				metrics.KafkaMessagesConsumed.WithLabelValues(topic, "success").Inc()
			}
			span.End()

			// Помечаем оффсет завершённым (успех ИЛИ ошибка) — вотермарк двигается
			// без head-of-line-стопа.
			tr.complete(msg)
		}(msg)
	}

	// Останов (ctx отменён): дождаться in-flight, дождаться выхода коммиттера,
	// финально сбросить вотермарки фоновым контекстом (основной уже отменён).
	wg.Wait()
	<-commitStopped
	c.flushCommits(context.Background(), tr)
	return nil
}

// flushCommits коммитит по одному сообщению-вотермарку на партицию (kafka-go
// коммитит offset+1, беря максимум по партиции).
func (c *Consumer) flushCommits(ctx context.Context, tr *offsetTracker) {
	msgs := tr.takeWatermarks()
	if len(msgs) == 0 {
		return
	}
	if err := c.reader.CommitMessages(ctx, msgs...); err != nil {
		if ctx.Err() != nil {
			return
		}
		fmt.Printf("kafka commit error: %v (messages will be redelivered)\n", err)
	}
}

// offsetTracker считает непрерывный успешный префикс на партицию для безопасного
// коммита в конкурентном консьюмере. Потокобезопасен.
type offsetTracker struct {
	mu    sync.Mutex
	parts map[int]*partOffsets
}

type partOffsets struct {
	cursor    int64                   // низший ещё не закоммиченный оффсет
	inited    bool                    // курсор проинициализирован первым fetch'ем
	next      int64                   // ожидаемый следующий оффсет от FetchMessage
	done      map[int64]kafka.Message // завершённые оффсеты >= cursor, ждущие непрерывности
	watermark *kafka.Message          // наивысшее сообщение, готовое к коммиту (не сброшено)
	waitSince time.Time               // когда курсор в последний раз двигался (для метрики застревания)
}

func newOffsetTracker() *offsetTracker {
	return &offsetTracker{parts: make(map[int]*partOffsets)}
}

func (t *offsetTracker) part(partition int) *partOffsets {
	p, ok := t.parts[partition]
	if !ok {
		p = &partOffsets{done: make(map[int64]kafka.Message)}
		t.parts[partition] = p
	}
	return p
}

// register задаёт курсор партиции первым увиденным оффсетом и пересинхронизирует
// его при разрыве последовательности.
//
// Разрыв (оффсет не равен ожидаемому следующему) означает, что партиция пришла
// заново: ребаланс группы, переподключение ридера после fetch-ошибки, отзыв и
// возврат партиции. Оффсета `cursor` из прошлой генерации мы больше не увидим —
// его сообщения либо съела другая реплика, либо ридер начал с закоммиченной
// позиции. Без пересинхронизации `complete` навсегда упирался бы в отсутствующий
// `done[cursor]`, вотермарк переставал двигаться, и партиция морозилась при живом
// и работающем консьюмере (инциденты 14-07 и 31-07: lag растёт, ошибок нет,
// лечилось только рестартом, который обнулял этот трекер).
//
// In-flight сообщения прошлой генерации при сбросе бросаем осознанно: партия
// оффсетов больше не наша, коммитить её нельзя, а at-least-once сохраняется —
// новый владелец перечитает их со своей закоммиченной позиции.
func (t *offsetTracker) register(partition int, offset int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.part(partition)
	if !p.inited || offset != p.next {
		p.cursor = offset
		p.inited = true
		p.watermark = nil
		p.waitSince = time.Now()
		if len(p.done) > 0 {
			p.done = make(map[int64]kafka.Message)
		}
	}
	p.next = offset + 1
}

// complete помечает оффсет завершённым и продвигает вотермарк по непрерывному
// префиксу завершённых оффсетов.
func (t *offsetTracker) complete(msg kafka.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.part(msg.Partition)
	// Оффсет ниже курсора — хвост прошлой генерации (см. register). Он уже покрыт
	// коммитом или больше не наш; в done ему делать нечего, иначе map растёт вечно.
	if p.inited && msg.Offset < p.cursor {
		return
	}
	p.done[msg.Offset] = msg
	moved := false
	for {
		m, ok := p.done[p.cursor]
		if !ok {
			break
		}
		wm := m
		p.watermark = &wm
		delete(p.done, p.cursor)
		p.cursor++
		moved = true
	}
	if moved {
		p.waitSince = time.Now()
	}
}

// stuckSeconds — сколько секунд партиция копит завершённые оффсеты, не в силах
// сдвинуть курсор. В норме близко к нулю (курсор ходит за каждым сообщением).
// Устойчивый рост = вотермарк не двигается при живом консьюмере: либо обработчик
// висит на оффсете-курсоре, либо трекер разошёлся с реальными оффсетами. Ждущих
// оффсетов нет → 0: простаивающая партиция не должна светить фальшивое застревание.
func (t *offsetTracker) stuckSeconds(now time.Time) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var worst float64
	for _, p := range t.parts {
		if len(p.done) == 0 || p.waitSince.IsZero() {
			continue
		}
		if age := now.Sub(p.waitSince).Seconds(); age > worst {
			worst = age
		}
	}
	return worst
}

// takeWatermarks забирает по одному сообщению-вотермарку на партицию и очищает
// их, чтобы не коммитить повторно.
func (t *offsetTracker) takeWatermarks() []kafka.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []kafka.Message
	for _, p := range t.parts {
		if p.watermark != nil {
			out = append(out, *p.watermark)
			p.watermark = nil
		}
	}
	return out
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
