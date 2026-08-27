package telegram

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Регрессия: при сбое Kafka оффсет getUpdates не двигается, и апдейт приходит
// заново. До фикса publish глотал ошибку, оффсет уезжал вперёд — сообщение
// пользователя терялось навсегда (бот молчал, ретраить было некому).
func TestPollLoopKeepsOffsetOnIngestError(t *testing.T) {
	r := &Receiver{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Цикл крутим синхронно: третий полл отменяет ctx, и pollLoop выходит сам.
	var askedOffsets []int
	calls := 0
	getUpdates := func(u tgbotapi.UpdateConfig) ([]tgbotapi.Update, error) {
		askedOffsets = append(askedOffsets, u.Offset)
		calls++
		switch calls {
		case 1, 2:
			return []tgbotapi.Update{{UpdateID: 10}, {UpdateID: 11}}, nil
		default:
			cancel()
			return nil, nil
		}
	}

	var published []int
	failFirst := map[int]bool{11: true} // 11 падает только на первой попытке
	fn := func(_ context.Context, up tgbotapi.Update) error {
		if failFirst[up.UpdateID] {
			delete(failFirst, up.UpdateID)
			return errors.New("kafka write: broker unavailable")
		}
		published = append(published, up.UpdateID)
		return nil
	}

	if err := r.pollLoop(ctx, getUpdates, fn, 1, 0); err != nil {
		t.Fatalf("pollLoop: %v", err)
	}

	// Первый полл: 10 опубликован, 11 упал → оффсет остановился на 11.
	// Второй полл: спрашиваем с 11, повторно приходят 10 и 11 — 10 публикуется
	// снова (at-least-once), 11 доезжает.
	if len(askedOffsets) < 3 || askedOffsets[0] != 0 || askedOffsets[1] != 11 || askedOffsets[2] != 12 {
		t.Fatalf("оффсеты getUpdates = %v; ждали 0, 11, 12", askedOffsets[:min(3, len(askedOffsets))])
	}
	want := []int{10, 10, 11}
	if len(published) != len(want) {
		t.Fatalf("опубликовано %v; ждали %v", published, want)
	}
	for i := range want {
		if published[i] != want[i] {
			t.Fatalf("опубликовано %v; ждали %v", published, want)
		}
	}
}

// Пачка без ошибок подтверждается целиком: следующий полл идёт с offset+1
// последнего апдейта, повторов нет.
func TestPollLoopAdvancesOffsetOnSuccess(t *testing.T) {
	r := &Receiver{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var askedOffsets []int
	calls := 0
	getUpdates := func(u tgbotapi.UpdateConfig) ([]tgbotapi.Update, error) {
		askedOffsets = append(askedOffsets, u.Offset)
		calls++
		if calls == 1 {
			return []tgbotapi.Update{{UpdateID: 7}, {UpdateID: 8}}, nil
		}
		cancel()
		return nil, nil
	}

	var published []int
	fn := func(_ context.Context, up tgbotapi.Update) error {
		published = append(published, up.UpdateID)
		return nil
	}

	if err := r.pollLoop(ctx, getUpdates, fn, 1, 0); err != nil {
		t.Fatalf("pollLoop: %v", err)
	}

	if len(published) != 2 || published[0] != 7 || published[1] != 8 {
		t.Fatalf("опубликовано %v; ждали [7 8]", published)
	}
	if askedOffsets[0] != 0 || askedOffsets[1] != 9 {
		t.Fatalf("оффсеты %v; ждали 0, 9", askedOffsets[:2])
	}
}
