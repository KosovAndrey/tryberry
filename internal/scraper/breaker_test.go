package scraper

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// stubScraper — управляемый скрейпер: отдаёт то, что положили в err/result, и
// считает РЕАЛЬНЫЕ обращения (именно их и должен резать брейкер).
type stubScraper struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (s *stubScraper) Marketplace() Marketplace { return MarketplaceYandexMarket }
func (s *stubScraper) Matches(string) bool      { return true }
func (s *stubScraper) Scrape(context.Context, string) (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &Result{Name: "товар", Price: 100, InStock: true}, nil
}
func (s *stubScraper) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func testRegistry(t *testing.T, cfg BreakerConfig) (*Registry, *stubScraper) {
	t.Helper()
	stub := &stubScraper{}
	r := &Registry{scrapers: []MarketplaceScraper{stub}, breaker: newBreaker(cfg, nil)}
	return r, stub
}

const testURL = "https://market.yandex.ru/card/x/1"

func TestBreakerOpensAfterThresholdAndSuppresses(t *testing.T) {
	cfg := BreakerConfig{Threshold: 3, Cooldown: 5 * time.Minute, MaxCooldown: time.Hour}
	r, stub := testRegistry(t, cfg)
	stub.err = ErrMarketplaceBlocked

	for i := 0; i < 3; i++ {
		if _, _, err := r.Scrape(context.Background(), testURL); !errors.Is(err, ErrMarketplaceBlocked) {
			t.Fatalf("запрос %d: ждём ErrMarketplaceBlocked, got %v", i, err)
		}
	}
	if stub.callCount() != 3 {
		t.Fatalf("до размыкания должно уйти ровно 3 запроса, ушло %d", stub.callCount())
	}

	// Цепь разомкнута: следующие запросы давятся БЕЗ обращения к площадке,
	// но наружу отдают ту же ошибку — обработка у бота/воркеров не меняется.
	for i := 0; i < 10; i++ {
		if _, _, err := r.Scrape(context.Background(), testURL); !errors.Is(err, ErrMarketplaceBlocked) {
			t.Fatalf("подавленный запрос %d: ждём ErrMarketplaceBlocked, got %v", i, err)
		}
	}
	if stub.callCount() != 3 {
		t.Fatalf("после размыкания запросы к площадке идти не должны, ушло %d", stub.callCount())
	}
}

func TestBreakerHalfOpenLetsExactlyOneProbe(t *testing.T) {
	cfg := BreakerConfig{Threshold: 2, Cooldown: time.Minute, MaxCooldown: time.Hour}
	r, stub := testRegistry(t, cfg)
	stub.err = ErrMarketplaceBlocked

	for i := 0; i < 2; i++ {
		r.Scrape(context.Background(), testURL) //nolint:errcheck // исход проверен выше
	}
	// Пауза вышла — руками отматываем openUntil в прошлое.
	r.breaker.mu.Lock()
	r.breaker.st[string(MarketplaceYandexMarket)].openUntil = time.Now().Add(-time.Second)
	r.breaker.mu.Unlock()

	before := stub.callCount()
	// Пробный запрос уходит и снова ловит блок → цепь размыкается заново,
	// пауза удваивается, следующие запросы опять давятся.
	if _, _, err := r.Scrape(context.Background(), testURL); !errors.Is(err, ErrMarketplaceBlocked) {
		t.Fatalf("проба: ждём ErrMarketplaceBlocked, got %v", err)
	}
	if got := stub.callCount() - before; got != 1 {
		t.Fatalf("полуоткрытая цепь должна пропустить ровно одну пробу, прошло %d", got)
	}
	for i := 0; i < 5; i++ {
		r.Scrape(context.Background(), testURL) //nolint:errcheck
	}
	if got := stub.callCount() - before; got != 1 {
		t.Fatalf("после неудачной пробы цепь должна снова давить запросы, прошло %d", got)
	}

	st := r.breaker.st[string(MarketplaceYandexMarket)]
	if st.cooldown != 2*time.Minute {
		t.Fatalf("пауза должна удвоиться до 2m, стала %v", st.cooldown)
	}
}

func TestBreakerCoolsDownNoFurtherThanMax(t *testing.T) {
	cfg := BreakerConfig{Threshold: 1, Cooldown: 30 * time.Minute, MaxCooldown: 45 * time.Minute}
	r, stub := testRegistry(t, cfg)
	stub.err = ErrMarketplaceBlocked

	r.Scrape(context.Background(), testURL) //nolint:errcheck
	for i := 0; i < 4; i++ {
		r.breaker.mu.Lock()
		r.breaker.st[string(MarketplaceYandexMarket)].openUntil = time.Now().Add(-time.Second)
		r.breaker.mu.Unlock()
		r.Scrape(context.Background(), testURL) //nolint:errcheck
	}
	if got := r.breaker.st[string(MarketplaceYandexMarket)].cooldown; got != 45*time.Minute {
		t.Fatalf("пауза должна упереться в потолок 45m, стала %v", got)
	}
}

func TestBreakerClosesOnLiveAnswer(t *testing.T) {
	cfg := BreakerConfig{Threshold: 2, Cooldown: time.Minute, MaxCooldown: time.Hour}
	r, stub := testRegistry(t, cfg)
	stub.err = ErrMarketplaceBlocked
	for i := 0; i < 2; i++ {
		r.Scrape(context.Background(), testURL) //nolint:errcheck
	}
	r.breaker.mu.Lock()
	r.breaker.st[string(MarketplaceYandexMarket)].openUntil = time.Now().Add(-time.Second)
	r.breaker.mu.Unlock()

	// Площадка ожила — проба проходит, цепь замыкается и пауза сбрасывается.
	stub.err = nil
	if _, _, err := r.Scrape(context.Background(), testURL); err != nil {
		t.Fatalf("проба по живой площадке должна пройти: %v", err)
	}
	before := stub.callCount()
	for i := 0; i < 5; i++ {
		if _, _, err := r.Scrape(context.Background(), testURL); err != nil {
			t.Fatalf("после замыкания запросы должны идти: %v", err)
		}
	}
	if got := stub.callCount() - before; got != 5 {
		t.Fatalf("после замыкания к площадке должны уйти все 5 запросов, ушло %d", got)
	}
	st := r.breaker.st[string(MarketplaceYandexMarket)]
	if !st.openUntil.IsZero() || st.cooldown != time.Minute || st.consecutive != 0 {
		t.Fatalf("состояние должно сброситься, стало %+v", *st)
	}
}

// Любой НЕ-blocked исход считается признаком жизни: цепь не размыкается, даже
// если площадка стабильно отдаёт parse_error (дрейф вёрстки — не бан).
func TestBreakerIgnoresNonBlockedFailures(t *testing.T) {
	cfg := BreakerConfig{Threshold: 2, Cooldown: time.Minute, MaxCooldown: time.Hour}
	r, stub := testRegistry(t, cfg)
	stub.err = ErrParseFailed

	for i := 0; i < 10; i++ {
		r.Scrape(context.Background(), testURL) //nolint:errcheck
	}
	if stub.callCount() != 10 {
		t.Fatalf("parse_error не должен размыкать цепь, к площадке ушло %d из 10", stub.callCount())
	}
}

// Блокировки вперемешку с успехами не копятся: считаем именно ПОДРЯД.
func TestBreakerCountsConsecutiveOnly(t *testing.T) {
	cfg := BreakerConfig{Threshold: 3, Cooldown: time.Minute, MaxCooldown: time.Hour}
	r, stub := testRegistry(t, cfg)

	for i := 0; i < 12; i++ {
		if i%3 == 2 {
			stub.err = nil // каждый третий — успех, серия рвётся
		} else {
			stub.err = ErrMarketplaceBlocked
		}
		r.Scrape(context.Background(), testURL) //nolint:errcheck
	}
	if stub.callCount() != 12 {
		t.Fatalf("цепь не должна размыкаться при рваной серии, ушло %d из 12", stub.callCount())
	}
}

func TestBreakerDisabledByZeroThreshold(t *testing.T) {
	r, stub := testRegistry(t, BreakerConfig{Threshold: 0, Cooldown: time.Minute, MaxCooldown: time.Hour})
	stub.err = ErrMarketplaceBlocked
	for i := 0; i < 20; i++ {
		r.Scrape(context.Background(), testURL) //nolint:errcheck
	}
	if stub.callCount() != 20 {
		t.Fatalf("с Threshold=0 брейкер должен молчать, ушло %d из 20", stub.callCount())
	}
}

// Обёртка сохраняет errors.Is(ErrMarketplaceBlocked) — на это завязаны ответы
// бота (telegram/track.go, vk/track.go).
func TestCircuitOpenErrorWrapsBlocked(t *testing.T) {
	err := errCircuitOpen("yandex_market", time.Now().Add(time.Minute))
	if !errors.Is(err, ErrMarketplaceBlocked) {
		t.Fatalf("ошибка брейкера должна оставаться ErrMarketplaceBlocked: %v", err)
	}
}

// Гонки: параллельные запросы на полуоткрытой цепи не должны пропустить больше
// одной пробы (go test -race).
func TestBreakerHalfOpenConcurrent(t *testing.T) {
	cfg := BreakerConfig{Threshold: 1, Cooldown: time.Minute, MaxCooldown: time.Hour}
	r, stub := testRegistry(t, cfg)
	stub.err = ErrMarketplaceBlocked
	r.Scrape(context.Background(), testURL) //nolint:errcheck
	r.breaker.mu.Lock()
	r.breaker.st[string(MarketplaceYandexMarket)].openUntil = time.Now().Add(-time.Second)
	r.breaker.mu.Unlock()

	before := stub.callCount()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Scrape(context.Background(), testURL) //nolint:errcheck
		}()
	}
	wg.Wait()
	if got := stub.callCount() - before; got != 1 {
		t.Fatalf("на полуоткрытой цепи должна пройти ровно одна проба, прошло %d", got)
	}
}

func TestBreakerConfigFromEnv(t *testing.T) {
	t.Setenv("SCRAPE_BREAKER_BLOCKS", "4")
	t.Setenv("SCRAPE_BREAKER_COOLDOWN", "90s")
	t.Setenv("SCRAPE_BREAKER_MAX_COOLDOWN", "30s") // меньше базовой — подтягиваем до неё
	cfg := BreakerConfigFromEnv()
	if cfg.Threshold != 4 || cfg.Cooldown != 90*time.Second || cfg.MaxCooldown != 90*time.Second {
		t.Fatalf("неверный разбор env: %+v", cfg)
	}

	t.Setenv("SCRAPE_BREAKER_BLOCKS", "")
	t.Setenv("SCRAPE_BREAKER_COOLDOWN", "не длительность")
	t.Setenv("SCRAPE_BREAKER_MAX_COOLDOWN", "")
	def := BreakerConfigFromEnv()
	want := BreakerConfig{Threshold: defaultBreakerThreshold, Cooldown: defaultBreakerCooldown, MaxCooldown: defaultBreakerMaxCooldown}
	if def != want {
		t.Fatalf("при пустом/битом env ждём дефолты %+v, got %+v", want, def)
	}
}
