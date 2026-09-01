package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Registry — реестр всех зарегистрированных скрейперов.
// Автоматически выбирает нужный по URL товара.
type Registry struct {
	scrapers []MarketplaceScraper
	breaker  *breaker // предохранитель против долбёжки в бан, см. breaker.go
}

func NewRegistry(scrapers ...MarketplaceScraper) *Registry {
	return &Registry{
		scrapers: scrapers,
		breaker:  newBreaker(BreakerConfigFromEnv(), nil),
	}
}

// SetLogger направляет сообщения брейкера (размыкание/проба/замыкание цепи) в
// логгер сервиса. Без вызова используется slog.Default().
func (r *Registry) SetLogger(log *slog.Logger) {
	if log != nil && r.breaker != nil {
		r.breaker.log = log
	}
}

// FindByURL возвращает скрейпер для данного URL или ошибку если ни один не подошёл
func (r *Registry) FindByURL(url string) (MarketplaceScraper, error) {
	for _, s := range r.scrapers {
		if s.Matches(url) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: no scraper matches URL %s", ErrInvalidURL, url)
}

// Scrape — удобный хелпер: найти скрейпер и сразу выполнить скрейпинг
func (r *Registry) Scrape(ctx context.Context, url string) (*Result, Marketplace, error) {
	s, err := r.FindByURL(url)
	if err != nil {
		return nil, "", err
	}

	tracer := otel.Tracer("scraper.registry")
	ctx, span := tracer.Start(ctx, "scraper.scrape",
		trace.WithAttributes(
			attribute.String("marketplace", string(s.Marketplace())),
			attribute.String("url", url),
		),
	)
	defer span.End()

	mp := string(s.Marketplace())
	start := time.Now()

	// Цепь разомкнута — площадка нас режет, и следующий запрос из этого процесса
	// срежется тоже. Не ходим в сеть: так флаг на IP остывает, а не подогревается.
	if ok, until := r.breaker.allow(mp, start); !ok {
		metrics.ScrapeSuppressed.WithLabelValues(mp).Inc()
		err := errCircuitOpen(mp, until)
		span.RecordError(err)
		span.SetStatus(codes.Error, "circuit_open")
		return nil, s.Marketplace(), err
	}

	result, err := s.Scrape(ctx, url)
	r.breaker.record(mp, errors.Is(err, ErrMarketplaceBlocked), time.Now())

	metrics.ScrapeDuration.WithLabelValues(mp).Observe(time.Since(start).Seconds())

	status := "success"
	switch {
	case err == nil:
	case errors.Is(err, ErrProductNotFound):
		status = "not_found"
	case errors.Is(err, ErrMarketplaceBlocked):
		status = "blocked"
	case errors.Is(err, ErrAuthExpired):
		// аккаунт-сессия протухла — отдельный статус для громкого алерта.
		status = "auth"
	case errors.Is(err, ErrNotImplemented):
		status = "disabled"
	case errors.Is(err, ErrDeadURLForm):
		// Ссылка в форме, которую площадка не обслуживает. Отдельный статус, а не
		// blocked: сети не было, брейкер не трогаем, а на дашборде видно, сколько
		// пользователей приходит со старыми ссылками (нужно ли ленивый резолв).
		status = "dead_url"
	case errors.Is(err, ErrParseFailed):
		// антибот пройден (200), но цену не достали — дрейф вёрстки / нет офферов.
		status = "parse_error"
	case isProxyError(err):
		// прокси отверг/недоступен (407/502 и т.п.) — НЕ антибот: отдельный статус,
		// чтобы алерт отличал «истёк/сломался прокси» от блокировки маркетплейсом.
		status = "proxy"
	default:
		status = "error"
	}
	metrics.ScrapeRequests.WithLabelValues(mp, status).Inc()

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, status)
		return nil, s.Marketplace(), err
	}

	span.SetAttributes(
		attribute.String("product.name", result.Name),
		attribute.Float64("product.price", result.Price),
	)

	return result, s.Marketplace(), nil
}

// isProxyError распознаёт ошибки уровня прокси (истёк/недоступен), а не маркетплейса:
// например "Proxy responded with non 200 code: 407 Proxy Authentication Required".
func isProxyError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "proxy responded") ||
		strings.Contains(s, "proxy authentication") ||
		strings.Contains(s, "407")
}

// SupportedMarketplaces — список всех зарегистрированных маркетплейсов
func (r *Registry) SupportedMarketplaces() []Marketplace {
	out := make([]Marketplace, 0, len(r.scrapers))
	for _, s := range r.scrapers {
		out = append(out, s.Marketplace())
	}
	return out
}
