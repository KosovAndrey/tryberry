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

func TestGroupByUser(t *testing.T) {
	rows := []domain.PendingAlert{
		{ID: 1, UserID: 10},
		{ID: 2, UserID: 20},
		{ID: 3, UserID: 10},
		{ID: 4, UserID: 30},
		{ID: 5, UserID: 20},
	}
	groups, order := groupByUser(rows)
	if want := []int64{10, 20, 30}; len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if len(groups[10]) != 2 || len(groups[20]) != 2 || len(groups[30]) != 1 {
		t.Fatalf("group sizes wrong: %d/%d/%d", len(groups[10]), len(groups[20]), len(groups[30]))
	}
}

// ── фейки ────────────────────────────────────────────────────────────────────

type fakeStore struct {
	sent       []int64 // одиночные MarkSent (битый payload)
	sentBatch  [][]int64
	failedIDs  [][]int64
	failedNext []time.Time
}

func (f *fakeStore) FetchDue(context.Context, int, int) ([]domain.PendingAlert, error) {
	return nil, nil
}
func (f *fakeStore) MarkSent(_ context.Context, id int64) error {
	f.sent = append(f.sent, id)
	return nil
}
func (f *fakeStore) MarkSentBatch(_ context.Context, ids []int64) error {
	f.sentBatch = append(f.sentBatch, ids)
	return nil
}
func (f *fakeStore) MarkFailedBatch(_ context.Context, ids []int64, _ string, next time.Time) error {
	f.failedIDs = append(f.failedIDs, ids)
	f.failedNext = append(f.failedNext, next)
	return nil
}
func (f *fakeStore) CountUnsent(context.Context, int) (int, error)              { return 0, nil }
func (f *fakeStore) DeleteSentBefore(context.Context, time.Time) (int64, error) { return 0, nil }
func (f *fakeStore) DeleteAbandonedBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type fakeSender struct {
	err     error
	single  []telegram.PriceAlert
	bundles []telegram.BundledAlert
}

func (s *fakeSender) SendPriceAlert(_ context.Context, a telegram.PriceAlert) error {
	if s.err != nil {
		return s.err
	}
	s.single = append(s.single, a)
	return nil
}
func (s *fakeSender) SendBundledAlert(_ context.Context, a telegram.BundledAlert) error {
	if s.err != nil {
		return s.err
	}
	s.bundles = append(s.bundles, a)
	return nil
}

func mustRow(t *testing.T, id, user int64, attempts int, a telegram.PriceAlert) domain.PendingAlert {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return domain.PendingAlert{ID: id, UserID: user, Attempts: attempts, Payload: b}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDeliverGroup_Single_Rich(t *testing.T) {
	store := &fakeStore{}
	sender := &fakeSender{}
	rows := []domain.PendingAlert{mustRow(t, 7, 1, 0, telegram.PriceAlert{ChatID: 1, NewPrice: 99})}

	deliverGroup(context.Background(), discardLog(), store, sender, rows, 10)

	if len(sender.single) != 1 || sender.single[0].NewPrice != 99 {
		t.Fatalf("expected one rich send, got %+v", sender.single)
	}
	if len(sender.bundles) != 0 {
		t.Fatalf("must not bundle a single alert")
	}
	if len(store.sentBatch) != 1 || len(store.sentBatch[0]) != 1 || store.sentBatch[0][0] != 7 {
		t.Fatalf("expected MarkSentBatch([7]), got %v", store.sentBatch)
	}
}

func TestDeliverGroup_Multi_Bundle(t *testing.T) {
	store := &fakeStore{}
	sender := &fakeSender{}
	rows := []domain.PendingAlert{
		mustRow(t, 1, 5, 0, telegram.PriceAlert{ChatID: 9, ProductName: "A", OldPrice: 100, NewPrice: 80}),
		mustRow(t, 2, 5, 0, telegram.PriceAlert{ChatID: 9, ProductName: "B", OldPrice: 200, NewPrice: 150}),
	}

	deliverGroup(context.Background(), discardLog(), store, sender, rows, 10)

	if len(sender.bundles) != 1 || len(sender.bundles[0].Items) != 2 {
		t.Fatalf("expected one bundle of 2, got %+v", sender.bundles)
	}
	if sender.bundles[0].ChatID != 9 {
		t.Fatalf("bundle ChatID = %d, want 9", sender.bundles[0].ChatID)
	}
	if len(sender.single) != 0 {
		t.Fatalf("must not send rich for a bundle")
	}
	if len(store.sentBatch) != 1 || len(store.sentBatch[0]) != 2 {
		t.Fatalf("expected MarkSentBatch of 2, got %v", store.sentBatch)
	}
}

func TestDeliverGroup_SendFails_Backoff(t *testing.T) {
	store := &fakeStore{}
	sender := &fakeSender{err: errors.New("egress down")}
	rows := []domain.PendingAlert{
		mustRow(t, 1, 5, 1, telegram.PriceAlert{ChatID: 9}),
		mustRow(t, 2, 5, 1, telegram.PriceAlert{ChatID: 9}),
	}

	before := time.Now()
	deliverGroup(context.Background(), discardLog(), store, sender, rows, 10)

	if len(store.sentBatch) != 0 {
		t.Fatalf("must not MarkSent on failure")
	}
	if len(store.failedIDs) != 1 || len(store.failedIDs[0]) != 2 {
		t.Fatalf("expected MarkFailedBatch of 2, got %v", store.failedIDs)
	}
	// maxAtt=1 → бэкофф 10с.
	if d := store.failedNext[0].Sub(before); d < 9*time.Second || d > 12*time.Second {
		t.Fatalf("backoff delay = %v, want ~10s", d)
	}
}

func TestDeliverGroup_BadPayload_Skipped(t *testing.T) {
	store := &fakeStore{}
	sender := &fakeSender{}
	rows := []domain.PendingAlert{
		{ID: 3, UserID: 5, Payload: []byte("{not json")},
		mustRow(t, 4, 5, 0, telegram.PriceAlert{ChatID: 9, NewPrice: 50}),
	}

	deliverGroup(context.Background(), discardLog(), store, sender, rows, 10)

	// Битая строка погашена отдельным MarkSent; валидная доставлена (одна → rich).
	if len(store.sent) != 1 || store.sent[0] != 3 {
		t.Fatalf("bad payload must be gassed via MarkSent(3), got %v", store.sent)
	}
	if len(sender.single) != 1 || sender.single[0].NewPrice != 50 {
		t.Fatalf("valid row must still deliver, got %+v", sender.single)
	}
	if len(store.sentBatch) != 1 || len(store.sentBatch[0]) != 1 || store.sentBatch[0][0] != 4 {
		t.Fatalf("expected MarkSentBatch([4]), got %v", store.sentBatch)
	}
}
