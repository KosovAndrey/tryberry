package postgres

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

// withSpan оборачивает БД-операцию в OpenTelemetry span + Prometheus метрики.
// Используй так:
//
//	return withSpan(ctx, "upsert_user", func(ctx context.Context) error {
//	    return r.db.QueryRow(ctx, q, ...).Scan(...)
//	})
func withSpan(ctx context.Context, operation string, fn func(context.Context) error) error {
	tracer := otel.Tracer("postgres")
	ctx, span := tracer.Start(ctx, "db."+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.operation", operation),
		),
	)
	defer span.End()

	err := metrics.WithDBTiming(operation, func() error {
		return fn(ctx)
	})

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}
