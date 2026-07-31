package kafka

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

func msg(partition int, offset int64) kafka.Message {
	return kafka.Message{Partition: partition, Offset: offset}
}

// takeWatermarks коммитит offset+1; удобнее сверять «committed up to offset».
func committedOffsets(msgs []kafka.Message) map[int]int64 {
	out := make(map[int]int64)
	for _, m := range msgs {
		out[m.Partition] = m.Offset
	}
	return out
}

// Непрерывный префикс: вотермарк не уходит вперёд незавершённого оффсета.
func TestOffsetTracker_ContiguousPrefix(t *testing.T) {
	tr := newOffsetTracker()
	for o := int64(0); o <= 3; o++ {
		tr.register(0, o)
	}

	// Завершили 0 и 2, но НЕ 1 → вотермарк должен встать на 0, не на 2.
	tr.complete(msg(0, 0))
	tr.complete(msg(0, 2))
	if wm := committedOffsets(tr.takeWatermarks()); wm[0] != 0 {
		t.Fatalf("watermark should hold at 0 while offset 1 pending, got %d", wm[0])
	}
	// takeWatermarks очистил — повторный вызов без прогресса пуст.
	if got := tr.takeWatermarks(); len(got) != 0 {
		t.Fatalf("expected no watermark after take, got %v", got)
	}

	// Завершаем 1 → префикс 0,1,2 непрерывен, вотермарк прыгает на 2.
	tr.complete(msg(0, 1))
	if wm := committedOffsets(tr.takeWatermarks()); wm[0] != 2 {
		t.Fatalf("watermark should advance to 2 after gap filled, got %d", wm[0])
	}

	// Завершаем 3 → вотермарк 3.
	tr.complete(msg(0, 3))
	if wm := committedOffsets(tr.takeWatermarks()); wm[0] != 3 {
		t.Fatalf("watermark should advance to 3, got %d", wm[0])
	}
}

// Курсор партиции инициализируется первым увиденным оффсетом (не обязан быть 0).
func TestOffsetTracker_NonZeroStart(t *testing.T) {
	tr := newOffsetTracker()
	tr.register(0, 100)
	tr.register(0, 101)
	tr.complete(msg(0, 101)) // пришёл раньше 100
	if got := tr.takeWatermarks(); len(got) != 0 {
		t.Fatalf("watermark must wait for first offset 100, got %v", got)
	}
	tr.complete(msg(0, 100))
	if wm := committedOffsets(tr.takeWatermarks()); wm[0] != 101 {
		t.Fatalf("watermark should reach 101, got %d", wm[0])
	}
}

// Партиции независимы.
func TestOffsetTracker_MultiPartition(t *testing.T) {
	tr := newOffsetTracker()
	tr.register(0, 0)
	tr.register(1, 0)
	tr.complete(msg(0, 0))
	tr.complete(msg(1, 0))
	wm := committedOffsets(tr.takeWatermarks())
	if wm[0] != 0 || wm[1] != 0 {
		t.Fatalf("both partitions should commit offset 0, got %v", wm)
	}
}

// Конкурентные complete из многих горутин: итоговый вотермарк = последний оффсет,
// и гонок нет (go test -race).
func TestOffsetTracker_ConcurrentComplete(t *testing.T) {
	const n = 1000
	tr := newOffsetTracker()
	for o := int64(0); o < n; o++ {
		tr.register(0, o)
	}
	var wg sync.WaitGroup
	for o := int64(0); o < n; o++ {
		wg.Add(1)
		go func(o int64) {
			defer wg.Done()
			tr.complete(msg(0, o))
		}(o)
	}
	wg.Wait()
	if wm := committedOffsets(tr.takeWatermarks()); wm[0] != n-1 {
		t.Fatalf("final watermark should be %d, got %d", n-1, wm[0])
	}
}

// Ребаланс: партиция ушла и вернулась с оффсетами ВЫШЕ старого курсора (их
// продвинула другая реплика). Курсор обязан пересинхронизироваться, иначе
// вотермарк ждёт оффсет, которого здесь уже не будет, и партиция морозится.
func TestOffsetTracker_ResyncAfterRebalanceForward(t *testing.T) {
	tr := newOffsetTracker()
	tr.register(0, 100)
	tr.register(0, 101)
	tr.complete(msg(0, 100))
	tr.takeWatermarks()

	// Партиция вернулась: следующий оффсет — 500, а не ожидаемые 102.
	tr.register(0, 500)
	tr.register(0, 501)
	tr.complete(msg(0, 500))
	tr.complete(msg(0, 501))
	// Хвост прошлой генерации завершился уже после сброса — коммит не откатываем.
	tr.complete(msg(0, 101))

	if wm := committedOffsets(tr.takeWatermarks()); wm[0] != 501 {
		t.Fatalf("watermark should resync to 501 after rebalance, got %d", wm[0])
	}
}

// Переподключение ридера: оффсеты пошли НАЗАД (перечитывание с закоммиченной
// позиции). Курсор садится на новый оффсет, старые done не блокируют.
func TestOffsetTracker_ResyncAfterRewind(t *testing.T) {
	tr := newOffsetTracker()
	for o := int64(200); o <= 203; o++ {
		tr.register(0, o)
	}
	tr.complete(msg(0, 203)) // висит в done, курсор на 200

	tr.register(0, 150) // откат
	tr.register(0, 151)
	tr.complete(msg(0, 150))
	tr.complete(msg(0, 151))
	if wm := committedOffsets(tr.takeWatermarks()); wm[0] != 151 {
		t.Fatalf("watermark should follow rewind to 151, got %d", wm[0])
	}
}

// Метрика застревания: растёт, только пока есть завершённые оффсеты, которые не
// может пропустить курсор; простаивающая партиция даёт ноль.
func TestOffsetTracker_StuckSeconds(t *testing.T) {
	tr := newOffsetTracker()
	now := time.Now()

	tr.register(0, 0)
	tr.register(0, 1)
	if got := tr.stuckSeconds(now.Add(time.Hour)); got != 0 {
		t.Fatalf("nothing completed above cursor → 0, got %v", got)
	}

	tr.complete(msg(0, 1)) // 0 ещё в работе → 1 ждёт в done
	if got := tr.stuckSeconds(now.Add(10 * time.Second)); got < 9 {
		t.Fatalf("stuck age should grow while cursor is blocked, got %v", got)
	}

	tr.complete(msg(0, 0)) // префикс закрылся, done пуст
	if got := tr.stuckSeconds(now.Add(time.Hour)); got != 0 {
		t.Fatalf("drained partition must report 0, got %v", got)
	}
}

// Вотермарки, снятые несколькими сбросами, монотонно растут и покрывают все оффсеты.
func TestOffsetTracker_IncrementalFlush(t *testing.T) {
	tr := newOffsetTracker()
	for o := int64(0); o < 5; o++ {
		tr.register(0, o)
	}
	var seen []int64
	flush := func() {
		for _, m := range tr.takeWatermarks() {
			seen = append(seen, m.Offset)
		}
	}
	tr.complete(msg(0, 0))
	flush()
	tr.complete(msg(0, 1))
	tr.complete(msg(0, 2))
	flush()
	tr.complete(msg(0, 4)) // разрыв: 3 ещё нет
	flush()
	tr.complete(msg(0, 3))
	flush()

	last := committedOffsets([]kafka.Message{{Partition: 0, Offset: seen[len(seen)-1]}})
	if last[0] != 4 {
		t.Fatalf("last watermark should be 4, got %d", last[0])
	}
	sort.Slice(seen, func(i, j int) bool { return seen[i] < seen[j] })
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("watermarks must be monotonic, got %v", seen)
		}
	}
}
