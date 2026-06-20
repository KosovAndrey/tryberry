package payment

import (
	"context"
	"errors"
	"testing"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
)

// ── фейки зависимостей Service ───────────────────────────────────────────────

type fakeProvider struct {
	name        string
	recurring   bool
	checkoutErr error
	res         CheckoutResult
	lastParams  CheckoutParams
	calls       int
}

func (f *fakeProvider) Name() string            { return f.name }
func (f *fakeProvider) SupportsRecurring() bool { return f.recurring }
func (f *fakeProvider) Checkout(_ context.Context, p CheckoutParams) (CheckoutResult, error) {
	f.calls++
	f.lastParams = p
	if f.checkoutErr != nil {
		return CheckoutResult{}, f.checkoutErr
	}
	return f.res, nil
}
func (f *fakeProvider) ChargeRecurring(_ context.Context, _ RecurringParams) error { return nil }

type fakeCreator struct {
	id          int64
	createErr   error
	setErr      error
	lastPayment domain.Payment
	createCalls int
	setCalls    int
}

func (f *fakeCreator) Create(_ context.Context, p domain.Payment) (int64, error) {
	f.createCalls++
	f.lastPayment = p
	if f.createErr != nil {
		return 0, f.createErr
	}
	return f.id, nil
}
func (f *fakeCreator) SetYKID(_ context.Context, _ int64, _ string) error {
	f.setCalls++
	return f.setErr
}

type fakeDiscountReader struct {
	d     redisrepo.PendingDiscount
	found bool
	err   error
}

func (f *fakeDiscountReader) Get(_ context.Context, _ int64) (redisrepo.PendingDiscount, bool, error) {
	return f.d, f.found, f.err
}

type fakeConsent struct {
	calls int
	err   error
	last  domain.SubscriptionConsent
}

func (f *fakeConsent) LogConsent(_ context.Context, c domain.SubscriptionConsent) error {
	f.calls++
	f.last = c
	return f.err
}

// ── тесты ────────────────────────────────────────────────────────────────────

// Разовая оплата без скидки: сумма = цена тарифа, строка payments заведена на ту же
// сумму без промо, провайдеру ушла та же сумма (не recurring), external id привязан.
func TestStart_OnetimeNoDiscount(t *testing.T) {
	prov := &fakeProvider{name: "robokassa", res: CheckoutResult{URL: "https://pay", ExternalID: "7"}}
	cr := &fakeCreator{id: 7}
	s := &Service{provider: prov, payments: cr, log: quietLog()} // discounts nil

	out, err := s.Start(context.Background(), &domain.User{ID: 1}, "pro", "e@x.ru")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := domain.PriceKopecks("pro")
	if out.AmountKopecks != want || out.DiscountPct != 0 || out.Recurring {
		t.Errorf("checkout=%+v; хотим amount=%d pct=0 recurring=false", out, want)
	}
	if out.ConfirmationURL != "https://pay" {
		t.Errorf("ConfirmationURL=%q", out.ConfirmationURL)
	}
	if cr.lastPayment.AmountKopecks != want || cr.lastPayment.PromoCodeID != nil {
		t.Errorf("строка payments: amount=%d promo=%v; хотим %d/nil", cr.lastPayment.AmountKopecks, cr.lastPayment.PromoCodeID, want)
	}
	if prov.lastParams.AmountKopecks != want || prov.lastParams.Recurring {
		t.Errorf("провайдеру: amount=%d recurring=%v", prov.lastParams.AmountKopecks, prov.lastParams.Recurring)
	}
	if cr.setCalls != 1 {
		t.Errorf("SetYKID вызван %d раз; хотим 1", cr.setCalls)
	}
}

// Ожидающая скидка применяется к первому платежу: сумма уменьшена, промо-код
// записан в строку payments, процент возвращён в Checkout.
func TestStart_WithPendingDiscount(t *testing.T) {
	prov := &fakeProvider{name: "robokassa", res: CheckoutResult{URL: "u", ExternalID: "7"}}
	cr := &fakeCreator{id: 7}
	dr := &fakeDiscountReader{found: true, d: redisrepo.PendingDiscount{CodeID: 55, Pct: 20}}
	s := &Service{provider: prov, payments: cr, discounts: dr, log: quietLog()}

	out, err := s.Start(context.Background(), &domain.User{ID: 1}, "pro", "e@x.ru")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantAmt := domain.DiscountedKopecks(domain.PriceKopecks("pro"), 20)
	if out.AmountKopecks != wantAmt || out.DiscountPct != 20 {
		t.Errorf("checkout amount=%d pct=%d; хотим %d/20", out.AmountKopecks, out.DiscountPct, wantAmt)
	}
	if cr.lastPayment.AmountKopecks != wantAmt {
		t.Errorf("payments.amount=%d; хотим %d", cr.lastPayment.AmountKopecks, wantAmt)
	}
	if cr.lastPayment.PromoCodeID == nil || *cr.lastPayment.PromoCodeID != 55 {
		t.Errorf("promo_code_id=%v; хотим 55", cr.lastPayment.PromoCodeID)
	}
}

// Непокупаемый план (цена 0) → ошибка ещё до создания платежа.
func TestStart_NonPurchasablePlan(t *testing.T) {
	prov := &fakeProvider{name: "robokassa"}
	cr := &fakeCreator{}
	s := &Service{provider: prov, payments: cr, log: quietLog()}

	if _, err := s.Start(context.Background(), &domain.User{ID: 1}, "free", "e@x.ru"); err == nil {
		t.Fatal("ожидали ошибку для непокупаемого плана")
	}
	if cr.createCalls != 0 || prov.calls != 0 {
		t.Errorf("при непокупаемом плане платёж не создаётся: create=%d provider=%d", cr.createCalls, prov.calls)
	}
}

// Сбой привязки external id НЕ должен валить оплату для пользователя (ссылка уже есть).
func TestStart_SetExternalIDFailureIsNonFatal(t *testing.T) {
	prov := &fakeProvider{name: "robokassa", res: CheckoutResult{URL: "https://pay", ExternalID: "7"}}
	cr := &fakeCreator{id: 7, setErr: errors.New("set ykid failed")}
	s := &Service{provider: prov, payments: cr, log: quietLog()}

	out, err := s.Start(context.Background(), &domain.User{ID: 1}, "pro", "e@x.ru")
	if err != nil {
		t.Fatalf("сбой SetYKID не должен возвращать ошибку юзеру; got %v", err)
	}
	if out.ConfirmationURL != "https://pay" {
		t.Errorf("ссылка на оплату должна вернуться; got %q", out.ConfirmationURL)
	}
}

// Первый платёж подписки: согласие залогировано на сумму автопродления, Checkout
// помечен recurring с суммой продления, провайдеру ушёл recurring=true.
func TestStartSubscription_HappyPath(t *testing.T) {
	prov := &fakeProvider{name: "robokassa", recurring: true, res: CheckoutResult{URL: "u", ExternalID: "9"}}
	cr := &fakeCreator{id: 9}
	cons := &fakeConsent{}
	s := &Service{provider: prov, payments: cr, consents: cons, log: quietLog()}

	out, err := s.StartSubscription(context.Background(), &domain.User{ID: 1}, "pro", "e@x.ru", "tg")
	if err != nil {
		t.Fatalf("StartSubscription: %v", err)
	}
	sub := domain.SubPriceKopecks("pro")
	if !out.Recurring || out.RenewalKopecks != sub || out.AmountKopecks != sub {
		t.Errorf("checkout=%+v; хотим recurring=true renewal=%d amount=%d", out, sub, sub)
	}
	if cons.calls != 1 || cons.last.AmountKopecks != sub || cons.last.Platform != "tg" {
		t.Errorf("согласие: calls=%d amount=%d platform=%q; хотим 1/%d/tg", cons.calls, cons.last.AmountKopecks, cons.last.Platform, sub)
	}
	if prov.lastParams.Recurring != true {
		t.Error("провайдеру должен уйти recurring=true")
	}
}

// Если согласие не записалось — оплату не начинаем (защита от чарджбэка).
func TestStartSubscription_ConsentFailureAborts(t *testing.T) {
	prov := &fakeProvider{name: "robokassa", recurring: true}
	cr := &fakeCreator{}
	cons := &fakeConsent{err: errors.New("consent log failed")}
	s := &Service{provider: prov, payments: cr, consents: cons, log: quietLog()}

	if _, err := s.StartSubscription(context.Background(), &domain.User{ID: 1}, "pro", "e@x.ru", "tg"); err == nil {
		t.Fatal("ожидали ошибку при несохранённом согласии")
	}
	if cr.createCalls != 0 || prov.calls != 0 {
		t.Errorf("без согласия платёж не создаётся: create=%d provider=%d", cr.createCalls, prov.calls)
	}
}

// Провайдер без автосписаний не должен заводить подписку.
func TestStartSubscription_ProviderUnsupported(t *testing.T) {
	prov := &fakeProvider{name: "yookassa", recurring: false}
	cr := &fakeCreator{}
	s := &Service{provider: prov, payments: cr, log: quietLog()}

	if _, err := s.StartSubscription(context.Background(), &domain.User{ID: 1}, "pro", "e@x.ru", "tg"); err == nil {
		t.Fatal("ожидали ошибку: провайдер не умеет рекуррент")
	}
	if cr.createCalls != 0 {
		t.Errorf("платёж не должен создаваться; create=%d", cr.createCalls)
	}
}
