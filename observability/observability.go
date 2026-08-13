// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package observability

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var (
	instrumentsMu  sync.RWMutex
	workerRuns     metric.Int64Counter
	workerDuration metric.Float64Histogram
	queueEvents    metric.Int64Counter
	queueLag       metric.Float64Histogram
	mcpCalls       metric.Int64Counter
	mcpDuration    metric.Float64Histogram
)

// Setup installs OTLP trace and metric providers when an OTLP endpoint is
// configured. With no endpoint the API remains a standards-compatible no-op,
// so telemetry failure can never make the data plane unavailable.
func Setup(ctx context.Context, serviceVersion string, logger *slog.Logger) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" &&
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" &&
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" {
		configureInstruments()
		return func(context.Context) error { return nil }, nil
	}
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithTelemetrySDK(), resource.WithAttributes(
		attribute.String("service.name", "tutor-mcp"),
		attribute.String("service.version", serviceVersion),
		attribute.String("deployment.environment.name", environmentName()),
	))
	if err != nil {
		return nil, err
	}
	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	metricExporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	traces := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExporter), sdktrace.WithResource(res))
	metrics := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(15*time.Second))),
	)
	otel.SetTracerProvider(traces)
	otel.SetMeterProvider(metrics)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if logger != nil {
			logger.Error("OpenTelemetry export failed", "error_type", "otel_export")
		}
	}))
	configureInstruments()
	return func(shutdownCtx context.Context) error {
		return errors.Join(metrics.Shutdown(shutdownCtx), traces.Shutdown(shutdownCtx))
	}, nil
}

func configureInstruments() {
	meter := otel.Meter("tutor-mcp/runtime")
	runs, _ := meter.Int64Counter("tutor.worker.runs", metric.WithDescription("Tenant worker run outcomes"))
	duration, _ := meter.Float64Histogram("tutor.worker.duration", metric.WithUnit("s"), metric.WithDescription("Tenant worker duration"))
	queues, _ := meter.Int64Counter("tutor.queue.events", metric.WithDescription("Durable queue transitions"))
	lag, _ := meter.Float64Histogram("tutor.queue.lag", metric.WithUnit("s"), metric.WithDescription("Age of a durable job when claimed"))
	tools, _ := meter.Int64Counter("tutor.mcp.tool.calls", metric.WithDescription("MCP tool call outcomes"))
	toolDuration, _ := meter.Float64Histogram("tutor.mcp.tool.duration", metric.WithUnit("s"), metric.WithDescription("MCP tool call duration"))
	instrumentsMu.Lock()
	workerRuns, workerDuration, queueEvents = runs, duration, queues
	queueLag, mcpCalls, mcpDuration = lag, tools, toolDuration
	instrumentsMu.Unlock()
}

func HTTPHandler(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "http.server",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " http.server" }),
	)
}

func TenantPseudonym(tenantID string) string {
	return entityPseudonym("tenant", tenantID)
}

func entityPseudonym(kind, value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("tutor-observability\x00" + kind + "\x00" + value))
	return hex.EncodeToString(sum[:8])
}

func RecordWorkerRun(ctx context.Context, tenantID, job, outcome string, duration time.Duration) {
	instrumentsMu.RLock()
	runs, histogram := workerRuns, workerDuration
	instrumentsMu.RUnlock()
	attrs := metric.WithAttributes(attribute.String("tenant.pseudonym", TenantPseudonym(tenantID)),
		attribute.String("job.name", job), attribute.String("job.outcome", outcome))
	if runs != nil {
		runs.Add(ctx, 1, attrs)
	}
	if histogram != nil {
		histogram.Record(ctx, duration.Seconds(), attrs)
	}
}

func RecordQueueEvent(ctx context.Context, tenantID, queue, outcome string) {
	instrumentsMu.RLock()
	counter := queueEvents
	instrumentsMu.RUnlock()
	if counter != nil {
		counter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("tenant.pseudonym", TenantPseudonym(tenantID)),
			attribute.String("queue.name", queue), attribute.String("queue.outcome", outcome)))
	}
}

func RecordQueueLag(ctx context.Context, tenantID, queue string, lag time.Duration) {
	instrumentsMu.RLock()
	histogram := queueLag
	instrumentsMu.RUnlock()
	if histogram != nil {
		if lag < 0 {
			lag = 0
		}
		histogram.Record(ctx, lag.Seconds(), metric.WithAttributes(
			attribute.String("tenant.pseudonym", TenantPseudonym(tenantID)),
			attribute.String("queue.name", queue)))
	}
}

func RecordMCPTool(ctx context.Context, tenantID, membershipID, enrollmentID, tool, outcome string, duration time.Duration) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("tenant.pseudonym", TenantPseudonym(tenantID)),
		attribute.String("membership.pseudonym", entityPseudonym("membership", membershipID)),
		attribute.String("enrollment.pseudonym", entityPseudonym("enrollment", enrollmentID)),
		attribute.String("mcp.tool.name", tool),
		attribute.String("mcp.tool.outcome", outcome),
	)
	instrumentsMu.RLock()
	counter, histogram := mcpCalls, mcpDuration
	instrumentsMu.RUnlock()
	attrs := metric.WithAttributes(
		attribute.String("tenant.pseudonym", TenantPseudonym(tenantID)),
		attribute.String("mcp.tool.name", tool), attribute.String("mcp.tool.outcome", outcome))
	if counter != nil {
		counter.Add(ctx, 1, attrs)
	}
	if histogram != nil {
		histogram.Record(ctx, duration.Seconds(), attrs)
	}
}

// StartWorkerRun creates a trace for one bounded tenant/job execution. Tenant
// identity is pseudonymized and the bounded job name is safe as a dimension.
func StartWorkerRun(ctx context.Context, tenantID, job string) (context.Context, func(string)) {
	ctx, span := otel.Tracer("tutor-mcp/worker").Start(ctx, "worker "+job,
		trace.WithAttributes(
			attribute.String("tenant.pseudonym", TenantPseudonym(tenantID)),
			attribute.String("job.name", job),
		))
	return ctx, func(outcome string) {
		span.SetAttributes(attribute.String("job.outcome", outcome))
		if outcome == "failed" {
			span.SetStatus(codes.Error, "worker run failed")
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}

// RegisterDBPool exports fleet capacity and saturation without query text,
// DSNs or tenant identifiers. The returned cleanup is idempotent in the OTel
// implementations used by the service.
func RegisterDBPool(database *sql.DB) (func() error, error) {
	if database == nil {
		return nil, errors.New("register DB pool: database is nil")
	}
	meter := otel.Meter("tutor-mcp/database")
	connections, err := meter.Int64ObservableGauge("tutor.db.connections",
		metric.WithDescription("Database pool connections by state"))
	if err != nil {
		return nil, err
	}
	waits, err := meter.Int64ObservableCounter("tutor.db.waits",
		metric.WithDescription("Cumulative waits for a database connection"))
	if err != nil {
		return nil, err
	}
	waitDuration, err := meter.Float64ObservableCounter("tutor.db.wait.duration",
		metric.WithUnit("s"), metric.WithDescription("Cumulative time waiting for a database connection"))
	if err != nil {
		return nil, err
	}
	registration, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		stats := database.Stats()
		observer.ObserveInt64(connections, int64(stats.MaxOpenConnections), metric.WithAttributes(attribute.String("state", "max_open")))
		observer.ObserveInt64(connections, int64(stats.OpenConnections), metric.WithAttributes(attribute.String("state", "open")))
		observer.ObserveInt64(connections, int64(stats.InUse), metric.WithAttributes(attribute.String("state", "in_use")))
		observer.ObserveInt64(connections, int64(stats.Idle), metric.WithAttributes(attribute.String("state", "idle")))
		observer.ObserveInt64(waits, stats.WaitCount)
		observer.ObserveFloat64(waitDuration, stats.WaitDuration.Seconds())
		return nil
	}, connections, waits, waitDuration)
	if err != nil {
		return nil, err
	}
	return registration.Unregister, nil
}

func TraceID(ctx context.Context) string {
	span := trace.SpanContextFromContext(ctx)
	if !span.IsValid() {
		return ""
	}
	return span.TraceID().String()
}

func environmentName() string {
	if value := os.Getenv("ENVIRONMENT"); value != "" {
		return value
	}
	if value := os.Getenv("DEPLOYMENT_PROFILE"); value != "" {
		return value
	}
	return "development"
}
