package scraper

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

// Брейкер по блокировкам — предохранитель против «долбёжки в бан».
//
// Зачем. 01-09-2026 SmartCaptcha пометил прод-IP целиком: Я.Маркет начал отдавать
// 302+капчу вообще на всё, включая главную. Скрейпер этого не замечал и продолжал
// стучаться прежним кадансом (~1700 блокировок в час), то есть держал флаг на IP
// горячим и заодно спамил алертом MarketplaceBlocked каждые три минуты. Полезной
// работы в этих запросах ноль: если площадка режет по IP, следующий такой же
// запрос из того же процесса тоже срежется.
//
// Как работает. Считаем блокировки ПОДРЯД по площадке. Threshold блокировок подряд
// размыкают цепь: Registry перестаёт звать скрейпер и сразу отдаёт
// ErrMarketplaceBlocked, не трогая сеть. По истечении паузы цепь полуоткрыта —
// пропускаем РОВНО ОДИН пробный запрос (остальные по-прежнему давятся). Прошёл —
// цепь закрывается и пауза сбрасывается к базовой; снова блокировка — цепь
// размыкается опять, а пауза удваивается до потолка MaxCooldown. Любой НЕ-blocked
// исход (успех, not_found, parse_error) считается признаком жизни и закрывает цепь:
// нас интересует ровно антибот, а не качество разбора.
//
// Состояние процессное, не общее: у трёх реплик скрейпера свои цепи, и каждая
// гасит свой трафик сама. Общий стейт в Redis тут не нужен — цена расхождения
// невелика (в худшем случае в бан летит по одному пробному запросу с реплики),
// а лишняя зависимость на горячем пути не нужна.
type BreakerConfig struct {
	// Threshold — сколько блокировок подряд размыкают цепь. 0 (или меньше)
	// полностью выключает брейкер: поведение как до его появления.
	Threshold int
	// Cooldown — базовая пауза после размыкания.
	Cooldown time.Duration
	// MaxCooldown — потолок паузы при повторных размыканиях (пауза удваивается,
	// пока пробный запрос продолжает ловить блокировку).
	MaxCooldown time.Duration
}

// Дефолты подобраны под каданс Я.Маркета (~1500 карточек в час на площадку):
// 10 блокировок подряд набегают примерно за минуту-полторы на реплику, дальше
// пробы идут не чаще раза в 5–60 минут — этого хватает, чтобы заметить снятие
// бана, и мало, чтобы его поддерживать.
const (
	defaultBreakerThreshold   = 10
	defaultBreakerCooldown    = 5 * time.Minute
	defaultBreakerMaxCooldown = 60 * time.Minute
)

// BreakerConfigFromEnv читает настройки брейкера из окружения. Вызывается внутри
// NewRegistry, чтобы предохранитель одинаково работал во ВСЕХ бинарях с реестром
// (scraper, bot-worker, search-worker, reseller-worker) без копипасты в cmd/*.
//
//	SCRAPE_BREAKER_BLOCKS        — Threshold, 0 выключает брейкер (дефолт 10)
//	SCRAPE_BREAKER_COOLDOWN      — Cooldown, формат time.ParseDuration (дефолт 5m)
//	SCRAPE_BREAKER_MAX_COOLDOWN  — MaxCooldown (дефолт 60m)
func BreakerConfigFromEnv() BreakerConfig {
	cfg := BreakerConfig{
		Threshold:   defaultBreakerThreshold,
		Cooldown:    defaultBreakerCooldown,
		MaxCooldown: defaultBreakerMaxCooldown,
	}
	if v := os.Getenv("SCRAPE_BREAKER_BLOCKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Threshold = n
		}
	}
	if d, err := time.ParseDuration(os.Getenv("SCRAPE_BREAKER_COOLDOWN")); err == nil && d > 0 {
		cfg.Cooldown = d
	}
	if d, err := time.ParseDuration(os.Getenv("SCRAPE_BREAKER_MAX_COOLDOWN")); err == nil && d > 0 {
		cfg.MaxCooldown = d
	}
	if cfg.MaxCooldown < cfg.Cooldown {
		cfg.MaxCooldown = cfg.Cooldown
	}
	return cfg
}

type breakerState struct {
	consecutive int           // блокировок подряд (сбрасывается любым другим исходом)
	openUntil   time.Time     // до какого момента цепь разомкнута; нулевое = замкнута
	cooldown    time.Duration // текущая пауза (растёт вдвое при повторных размыканиях)
	probing     bool          // пробный запрос полуоткрытой цепи уже в полёте
}

type breaker struct {
	cfg BreakerConfig
	log *slog.Logger
	mu  sync.Mutex
	st  map[string]*breakerState
}

func newBreaker(cfg BreakerConfig, log *slog.Logger) *breaker {
	if log == nil {
		log = slog.Default()
	}
	return &breaker{cfg: cfg, log: log, st: make(map[string]*breakerState)}
}

func (b *breaker) enabled() bool { return b != nil && b.cfg.Threshold > 0 }

// allow решает, пускать ли запрос к площадке, и возвращает время, до которого
// цепь разомкнута (для внятной ошибки). Полуоткрытая цепь пропускает ровно один
// пробный запрос: остальные ждут его исхода, давясь без обращения к сети.
func (b *breaker) allow(mp string, now time.Time) (bool, time.Time) {
	if !b.enabled() {
		return true, time.Time{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	s := b.st[mp]
	if s == nil || s.openUntil.IsZero() {
		return true, time.Time{}
	}
	if now.Before(s.openUntil) {
		return false, s.openUntil
	}
	// Пауза вышла — цепь полуоткрыта.
	if s.probing {
		return false, s.openUntil
	}
	s.probing = true
	b.log.Info("scrape breaker: пробный запрос полуоткрытой цепи", "marketplace", mp)
	return true, time.Time{}
}

// record учитывает исход запроса: blocked двигает цепь к размыканию, любой другой
// исход считается признаком жизни площадки и замыкает цепь обратно.
func (b *breaker) record(mp string, blocked bool, now time.Time) {
	if !b.enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	s := b.st[mp]
	if s == nil {
		s = &breakerState{cooldown: b.cfg.Cooldown}
		b.st[mp] = s
	}

	if !blocked {
		s.consecutive = 0
		if s.probing || !s.openUntil.IsZero() {
			b.log.Info("scrape breaker: цепь замкнута, площадка отвечает", "marketplace", mp)
			metrics.ScrapeCircuitOpen.WithLabelValues(mp).Set(0)
		}
		s.probing = false
		s.openUntil = time.Time{}
		s.cooldown = b.cfg.Cooldown
		return
	}

	s.consecutive++
	switch {
	case s.probing:
		// Пробный запрос снова словил блокировку — размыкаем с удвоенной паузой.
		s.probing = false
		s.cooldown = min(s.cooldown*2, b.cfg.MaxCooldown)
		s.openUntil = now.Add(s.cooldown)
		b.log.Warn("scrape breaker: проба снова заблокирована, пауза удвоена",
			"marketplace", mp, "cooldown", s.cooldown, "open_until", s.openUntil.Format(time.RFC3339))
	case s.openUntil.IsZero() && s.consecutive >= b.cfg.Threshold:
		if s.cooldown <= 0 {
			s.cooldown = b.cfg.Cooldown
		}
		s.openUntil = now.Add(s.cooldown)
		metrics.ScrapeCircuitOpen.WithLabelValues(mp).Set(1)
		b.log.Warn("scrape breaker: цепь разомкнута — площадка блокирует, перестаём стучаться",
			"marketplace", mp, "blocks_in_row", s.consecutive,
			"cooldown", s.cooldown, "open_until", s.openUntil.Format(time.RFC3339))
	}
}

// errCircuitOpen — что видит вызывающий, пока цепь разомкнута. Обёрнут в
// ErrMarketplaceBlocked: боту/воркерам это тот же «площадка не пускает», их
// обработка ошибок менять не нужно.
func errCircuitOpen(mp string, until time.Time) error {
	return fmt.Errorf("%w: цепь %s разомкнута брейкером до %s", ErrMarketplaceBlocked, mp, until.Format(time.RFC3339))
}
