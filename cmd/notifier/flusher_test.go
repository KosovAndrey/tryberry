package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/telegram"
)

func TestFlusherBackoff(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{100, 30 * time.Minute}, // потолок
	}
	for _, c := range cases {
		if got := flusherBackoff(c.attempts); got != c.want {
			t.Errorf("flusherBackoff(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

// ── фейки ────────────────────────────────────────────────────────────────────

type fakeStore struct {
	sent       []int64
	failed     []int64
	failedNext []time.Time
}

func (f *fakeStore) FetchDue(context.Context, int, int) ([]domain.PendingAlert, error) {
	return nil, nil
}
func (f *fakeStore) MarkSent(_ context.Context, id int64) error {
	f.sent = append(f.sent, id)
	return nil
}
func (f *fakeStore) MarkFailed(_ context.Context, id int64, _ string, next time.Time) error {
	f.failed = append(f.failed, id)
	f.failedNext = append(f.failedNext, next)
	return nil
}
func (f *fakeStore) CountUnsent(context.Context) (int, error)                   { return 0, nil }
func (f *fakeStore) DeleteSentBefore(context.Context, time.Time) (int64, error) { return 0, nil }

type fakeSender struct {
	err  error
	sent []telegram.PriceAlert
}

func (s *fakeSender) SendPriceAlert(_ context.Context, a telegram.PriceAlert) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, a)
	return nil
}

func mustPayload(t *testing.T, a telegram.PriceAlert) []byte {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDeliverOne_Success(t *testing.T) {
	store := &fakeStore{}
	sender := &fakeSender{}
	row := domain.PendingAlert{ID: 7, Payload: mustPayload(t, telegram.PriceAlert{ChatID: 1, NewPrice: 99})}

	deliverOne(context.Background(), discardLog(), store, sender, row, 10)

	if len(sender.sent) != 1 || sender.sent[0].NewPrice != 99 {
		t.Fatalf("expected one send, got %+v", sender.sent)
	}
	if len(store.sent) != 1 || store.sent[0] != 7 {
		t.Fatalf("expected MarkSent(7), got %v", store.sent)
	}
	if len(store.failed) != 0 {
		t.Fatalf("unexpected MarkFailed: %v", store.failed)
	}
}

func TestDeliverOne_SendFails_Backoff(t *testing.T) {
	store := &fakeStore{}
	sender := &fakeSender{err: errors.New("egress down")}
	row := domain.PendingAlert{ID: 9, Attempts: 1, Payload: mustPayload(t, telegram.PriceAlert{ChatID: 1})}

	before := time.Now()
	deliverOne(context.Background(), discardLog(), store, sender, row, 10)

	if len(store.sent) != 0 {
		t.Fatalf("must not MarkSent on failure, got %v", store.sent)
	}
	if len(store.failed) != 1 || store.failed[0] != 9 {
		t.Fatalf("expected MarkFailed(9), got %v", store.failed)
	}
	// attempts=1 → бэкофф 10с: next ≈ now+10s.
	gotDelay := store.failedNext[0].Sub(before)
	if gotDelay < 9*time.Second || gotDelay > 12*time.Second {
		t.Fatalf("backoff delay = %v, want ~10s", gotDelay)
	}
}

func TestDeliverOne_BadPayload_GivesUp(t *testing.T) {
	store := &fakeStore{}
	sender := &fakeSender{}
	row := domain.PendingAlert{ID: 3, Payload: []byte("{not json")}

	deliverOne(context.Background(), discardLog(), store, sender, row, 10)

	if len(sender.sent) != 0 {
		t.Fatalf("must not attempt send on bad payload")
	}
	if len(store.sent) != 1 || store.sent[0] != 3 {
		t.Fatalf("bad payload must be gassed via MarkSent, got %v", store.sent)
	}
}
