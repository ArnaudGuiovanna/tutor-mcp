package engine

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"tutor-mcp/models"
	"tutor-mcp/observability"
)

type panicSaaSObservationStore struct{ saasWorkerStore }

func (panicSaaSObservationStore) BeginIntegrationDelivery(context.Context, models.TenantScope, models.TenantWebhookJob, int, time.Time) (*models.IntegrationDelivery, error) {
	panic("private panic")
}

func TestSaaSPanicQueueOutcomeIsFailed(t *testing.T) {
	previous := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	if _, err := observability.Setup(t.Context(), "test", quietLogger()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetMeterProvider(previous)
		_, _ = observability.Setup(context.Background(), "test", quietLogger())
	})
	s := NewScheduler(nil, quietLogger())
	scope := models.LegacyPrincipal("L1").TenantScope()
	job := &models.AsyncJob{ID: "panic-metric", TenantID: scope.TenantID, Kind: "tenant_webhook_delivery", PayloadJSON: `{"integration_id":"i","event_id":"e","event_type":"test.event","payload":{}}`, CreatedAt: time.Now()}
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_, _ = s.consumeSaaSJob(t.Context(), panicSaaSObservationStore{}, scope, job)
	}()
	if !panicked {
		t.Fatal("fixture did not panic")
	}
	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &metrics); err != nil {
		t.Fatal(err)
	}
	var events int64
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "tutor.queue.events" {
				continue
			}
			for _, point := range metric.Data.(metricdata.Sum[int64]).DataPoints {
				outcome, _ := point.Attributes.Value(attribute.Key("queue.outcome"))
				if outcome.AsString() != "failed" {
					t.Fatalf("panic emitted queue outcome=%q", outcome.AsString())
				}
				events += point.Value
			}
		}
	}
	if events != 1 {
		t.Fatal("panic event missing/duplicated", events)
	}
}
