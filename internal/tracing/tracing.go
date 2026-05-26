package tracing

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Init настраивает глобальный TracerProvider.
// Вызвать ОДИН раз при старте сервиса в main().
// Возвращает shutdown функцию — её нужно вызвать при graceful shutdown.
func Init(ctx context.Context, serviceName, otlpEndpoint string) (func(context.Context) error, error) {
	// OTLP gRPC экспортёр — стандартный способ слать спаны в Jaeger/Tempo/etc
	// WithInsecure() — без TLS, нормально внутри docker-сети
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(otlpEndpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	// Resource — атрибуты которые добавляются КО ВСЕМ спанам
	// service.name — то по чему Jaeger группирует спаны в UI
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		// Batcher отправляет спаны пачками — меньше нагрузка на сеть
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
		// Sampling: AlwaysSample — пишем все спаны
		// На проде с высоким RPS лучше TraceIDRatioBased(0.1) — 10% трафика
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	otel.SetTracerProvider(tp)

	// Propagator — определяет КАК trace_id передаётся между сервисами
	// W3C TraceContext — стандарт. Baggage — для кастомных полей.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Возвращаем функцию которая flush'ит оставшиеся спаны перед выходом
	return tp.Shutdown, nil
}
