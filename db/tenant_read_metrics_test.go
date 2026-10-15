package db

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestTenantReadPoolMetricsDistinguishPhysicalPools(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "sqlite_reader_writer"}[separate], func(t *testing.T) {
			s := sqliteReaderTestStore(t)
			if !separate {
				s = NewStore(s.root)
			}
			previous := otel.GetMeterProvider()
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			otel.SetMeterProvider(provider)
			defer func() { otel.SetMeterProvider(previous); _ = provider.Shutdown(context.Background()) }()
			unregister, err := RegisterStoreDBPools(s)
			if err != nil {
				t.Fatal(err)
			}
			defer unregister()
			var metrics metricdata.ResourceMetrics
			if err := reader.Collect(t.Context(), &metrics); err != nil {
				t.Fatal(err)
			}
			max := map[string]int64{}
			waitPoints := 0
			for _, scope := range metrics.ScopeMetrics {
				for _, m := range scope.Metrics {
					switch m.Name {
					case "tutor.db.connections":
						for _, point := range m.Data.(metricdata.Gauge[int64]).DataPoints {
							state, _ := point.Attributes.Value(attribute.Key("state"))
							pool, _ := point.Attributes.Value(attribute.Key("pool"))
							if state.AsString() == "max_open" {
								max[pool.AsString()] += point.Value
							}
						}
					case "tutor.db.waits":
						waitPoints += len(m.Data.(metricdata.Sum[int64]).DataPoints)
					}
				}
			}
			if separate {
				if len(max) != 2 || max["reader"] != 4 || max["writer"] != 1 || waitPoints != 2 {
					t.Fatalf("pool observations duplicated/missing: %v waits=%d", max, waitPoints)
				}
			} else if len(max) != 1 || max["shared"] != 1 || waitPoints != 1 {
				t.Fatalf("shared pool counted twice: %v waits=%d", max, waitPoints)
			}
			if err := unregister(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
