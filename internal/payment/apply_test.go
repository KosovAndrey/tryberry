package payment

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
)

// ── фейки зависимостей Applier ───────────────────────────────────────────────

type fakeMarker struct {
	applied postgres.AppliedPayment
	ok      bool
	err     error
	calls   int
}

func (f *fakeMarker) MarkSucceeded(_ context.Context, _ int64, _ time.Time) (postgres.AppliedPayment, bool, error) {
	f.calls++
	return f.applied, f.ok, f.err
}

type fakeUsers struct{ byID map[int64]*domain.User }

func (f *fakeUsers) GetByID(_ context.Context, id int64) (*domain.User, error) {
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, errors.New("user not found")
}

type fakeBilling struct {
	activateCalls int
	renewCalls    int
	lastActivate  domain.BillingSubscription
}

func (f *fakeBilling) Activate(_ context.Context, s domain.BillingSubscription) (int64, error) {
	f.activateCalls++
	f.lastActivate = s
	return 42, nil
}

func (f *fakeBilling) MarkRenewed(_ context.Context, _, _ int64, _ time.Time) error {
	f.renewCalls++
	return nil
}

type fakePromos struct{ redeemCalls int }

func (f *fakePromos) RedeemDiscount(_ context.Context, _, _ int64) error {
	f.redeemCalls++
	return nil
}

type fakeReferrals struct {
	granted   bool
	calls     int
	lastEvent string
}

func (f *fakeReferrals) GrantReward(_ context.Context, _, _ int64, event string, _ int, _ int, _ bool, _ string, _ time.Time) (bool, error) {
	f.calls++
	f.lastEvent = event
	return f.granted, nil
}

type fakeDiscounts struct{ delCalls int }

func (f *fakeDiscounts) Del(_ context.Context, _ int64) error {
	f.delCalls++
	return nil
}

type fakeNotifier struct {
	paymentSucceeded int
	referralPaid     int
	lastPlan         string
	lastExpiresAt    time.Time
}

func (f *fakeNotifier) PaymentSucceeded(_ context.Context, _ *domain.User, plan string, expiresAt time.Time) {
	f.paymentSucceeded++
	f.lastPlan = plan
	f.lastExpiresAt = expiresAt
}
func (f *fakeNotifier) ReferralPaid(_ context.Context, _ *domain.User, _ string, _ int, _ bool) {
	f.referralPaid++
}
func (f *fakeNotifier) SubscriptionChargeUpcoming(_ context.Context, _ *domain.User, _ string, _ int64, _ time.Time) {
}
func (f *fakeNotifier) SubscriptionPaymentFailed(_ context.Context, _ *domain.User, _ string, _ bool) {
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newApplier собирает Applier из фейков (NewApplier берёт конкретные репо — в тестах
// заполняем поля напрямую, они одного пакета). billing/discounts можно передать nil.
func newApplier(pm paymentMarker, us userGetter, bl subscriptionApplier, pr promoRedeemer, rf referralGranter, dc discountClearer, nf Notifier) *Applier {
	a := &Applier{payments: pm, users: us, promos: pr, referrals: rf, notify: nf, log: quietLog()}
	if bl != nil {
		a.billing = bl
	}
	if dc != nil {
		a.discounts = dc
	}
	return a
}

// ── тесты ────────────────────────────────────────────────────────────────────

// Идемпотентность: повторно доставленное событие (MarkSucceeded ok=false) НЕ должно
// продлевать план/слать уведомление/начислять награду — иначе двойное применение.
func TestApply_IdempotentSkip(t *testing.T) {
	pm := &fakeMarker{ok: false}
	bl := &fakeBilling{}
	rf := &fakeReferrals{}
	nf := &fakeNotifier{}
	a := newApplier(pm, &fakeUsers{}, bl, &fakePromos{}, rf, &fakeDiscounts{}, nf)

	if err := a.Apply(context.Background(), ConfirmedEvent{PaymentID: 7}); err != nil {
		t.Fatalf("Apply вернул ошибку на уже применённом платеже: %v", err)
	}
	if pm.calls != 1 {
		t.Errorf("MarkSucceeded вызван %d раз; хотим 1", pm.calls)
	}
	if nf.paymentSucceeded != 0 || bl.activateCalls != 0 || rf.calls != 0 {
		t.Errorf("на повторном событии не должно быть побочных эффектов: notify=%d activate=%d referral=%d",
			nf.paymentSucceeded, bl.activateCalls, rf.calls)
	}
}

// Сбой БД при MarkSucceeded → ошибка пробрасывается (Kafka переотправит), без эффектов.
func TestApply_MarkerErrorPropagates(t *testing.T) {
	boom := errors.New("db down")
	pm := &fakeMarker{err: boom}
	nf := &fakeNotifier{}
	a := newApplier(pm, &fakeUsers{}, &fakeBilling{}, &fakePromos{}, &fakeReferrals{}, &fakeDiscounts{}, nf)

	if err := a.Apply(context.Background(), ConfirmedEvent{PaymentID: 7}); !errors.Is(err, boom) {
		t.Fatalf("ожидали проброс ошибки БД; got %v", err)
	}
	if nf.paymentSucceeded != 0 {
		t.Error("при ошибке БД уведомление слаться не должно")
	}
}

// Разовая оплата: план продлён, уведомление ушло, подписка/реферал не трогаются,
// nil billing/discounts не паникуют.
func TestApply_OnetimeHappyPath(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour)
	pm := &fakeMarker{ok: true, applied: postgres.AppliedPayment{
		PaymentID: 7, UserID: 1, Plan: "pro", Kind: domain.PayKindOnetime, ExpiresAt: exp,
	}}
	us := &fakeUsers{byID: map[int64]*domain.User{1: {ID: 1, Username: "buyer"}}}
	rf := &fakeReferrals{}
	nf := &fakeNotifier{}
	// billing=nil, discounts=nil намеренно
	a := newApplier(pm, us, nil, &fakePromos{}, rf, nil, nf)

	if err := a.Apply(context.Background(), ConfirmedEvent{PaymentID: 7}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if nf.paymentSucceeded != 1 || nf.lastPlan != "pro" || !nf.lastExpiresAt.Equal(exp) {
		t.Errorf("уведомление об оплате неверно: count=%d plan=%q exp=%v", nf.paymentSucceeded, nf.lastPlan, nf.lastExpiresAt)
	}
	if rf.calls != 0 {
		t.Errorf("без реферера награда не начисляется; calls=%d", rf.calls)
	}
}

// Первый платёж подписки активирует рекуррент: next_charge_at = expires − lead time,
// ключ рекуррента = id первого платежа.
func TestApply_SubInitialActivates(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour)
	pm := &fakeMarker{ok: true, applied: postgres.AppliedPayment{
		PaymentID: 100, UserID: 1, Plan: "pro", Kind: domain.PayKindSubInitial, ExpiresAt: exp,
	}}
	us := &fakeUsers{byID: map[int64]*domain.User{1: {ID: 1}}}
	bl := &fakeBilling{}
	a := newApplier(pm, us, bl, &fakePromos{}, &fakeReferrals{}, nil, &fakeNotifier{})

	if err := a.Apply(context.Background(), ConfirmedEvent{PaymentID: 100}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if bl.activateCalls != 1 {
		t.Fatalf("Activate вызван %d раз; хотим 1", bl.activateCalls)
	}
	wantNext := exp.Add(-domain.SubChargeLeadTime)
	if !bl.lastActivate.NextChargeAt.Equal(wantNext) {
		t.Errorf("next_charge_at=%v; хотим %v", bl.lastActivate.NextChargeAt, wantNext)
	}
	if bl.lastActivate.RecurringInvoiceID != 100 {
		t.Errorf("RecurringInvoiceID=%d; хотим 100 (id первого платежа)", bl.lastActivate.RecurringInvoiceID)
	}
}

// Автосписание продлевает существующую подписку; без её id — не падаем и не продлеваем.
func TestApply_SubRenewal(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour)
	subID := int64(42)
	us := &fakeUsers{byID: map[int64]*domain.User{1: {ID: 1}}}

	t.Run("with id renews", func(t *testing.T) {
		pm := &fakeMarker{ok: true, applied: postgres.AppliedPayment{
			PaymentID: 101, UserID: 1, Plan: "pro", Kind: domain.PayKindSubRenewal,
			ExpiresAt: exp, BillingSubscriptionID: &subID,
		}}
		bl := &fakeBilling{}
		a := newApplier(pm, us, bl, &fakePromos{}, &fakeReferrals{}, nil, &fakeNotifier{})
		if err := a.Apply(context.Background(), ConfirmedEvent{PaymentID: 101}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if bl.renewCalls != 1 {
			t.Errorf("MarkRenewed вызван %d раз; хотим 1", bl.renewCalls)
		}
	})

	t.Run("missing id does not renew", func(t *testing.T) {
		pm := &fakeMarker{ok: true, applied: postgres.AppliedPayment{
			PaymentID: 102, UserID: 1, Plan: "pro", Kind: domain.PayKindSubRenewal, ExpiresAt: exp,
		}}
		bl := &fakeBilling{}
		a := newApplier(pm, us, bl, &fakePromos{}, &fakeReferrals{}, nil, &fakeNotifier{})
		if err := a.Apply(context.Background(), ConfirmedEvent{PaymentID: 102}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if bl.renewCalls != 0 {
			t.Errorf("без BillingSubscriptionID продлевать нельзя; renewCalls=%d", bl.renewCalls)
		}
	})
}

// Реферальная награда: при наличии реферера зовём GrantReward с событием paid и
// уведомляем только когда награда реально начислена (granted=true).
func TestApply_ReferralReward(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour)
	referrer := int64(2)
	mk := func() (*fakeMarker, *fakeUsers) {
		pm := &fakeMarker{ok: true, applied: postgres.AppliedPayment{
			PaymentID: 7, UserID: 1, Plan: "pro", Kind: domain.PayKindOnetime, ExpiresAt: exp,
		}}
		us := &fakeUsers{byID: map[int64]*domain.User{
			1: {ID: 1, Username: "buyer", ReferredBy: &referrer},
			2: {ID: 2, Username: "ref"},
		}}
		return pm, us
	}

	t.Run("granted notifies", func(t *testing.T) {
		pm, us := mk()
		rf := &fakeReferrals{granted: true}
		nf := &fakeNotifier{}
		a := newApplier(pm, us, nil, &fakePromos{}, rf, nil, nf)
		if err := a.Apply(context.Background(), ConfirmedEvent{PaymentID: 7}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if rf.calls != 1 || rf.lastEvent != domain.ReferralEventPaid {
			t.Errorf("GrantReward calls=%d event=%q; хотим 1/%q", rf.calls, rf.lastEvent, domain.ReferralEventPaid)
		}
		if nf.referralPaid != 1 {
			t.Errorf("при granted=true должно быть уведомление рефереру; got %d", nf.referralPaid)
		}
	})

	t.Run("not granted is silent", func(t *testing.T) {
		pm, us := mk()
		rf := &fakeReferrals{granted: false}
		nf := &fakeNotifier{}
		a := newApplier(pm, us, nil, &fakePromos{}, rf, nil, nf)
		if err := a.Apply(context.Background(), ConfirmedEvent{PaymentID: 7}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if nf.referralPaid != 0 {
			t.Errorf("при granted=false (потолок/повтор) уведомления быть не должно; got %d", nf.referralPaid)
		}
	})
}

// Платёж со скидкой: гасим discount-код и снимаем «ожидающую» скидку.
func TestApply_PromoRedeemAndDiscountCleared(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour)
	codeID := int64(55)
	pm := &fakeMarker{ok: true, applied: postgres.AppliedPayment{
		PaymentID: 7, UserID: 1, Plan: "pro", Kind: domain.PayKindOnetime, ExpiresAt: exp, PromoCodeID: &codeID,
	}}
	us := &fakeUsers{byID: map[int64]*domain.User{1: {ID: 1}}}
	pr := &fakePromos{}
	dc := &fakeDiscounts{}
	a := newApplier(pm, us, nil, pr, &fakeReferrals{}, dc, &fakeNotifier{})

	if err := a.Apply(context.Background(), ConfirmedEvent{PaymentID: 7}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if pr.redeemCalls != 1 {
		t.Errorf("RedeemDiscount вызван %d раз; хотим 1", pr.redeemCalls)
	}
	if dc.delCalls != 1 {
		t.Errorf("ожидающая скидка не снята: delCalls=%d", dc.delCalls)
	}
}
