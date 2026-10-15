package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/models"
	"tutor-mcp/observability"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type observedToolPoint struct {
	name, outcome string
	calls         int64
	durations     uint64
	seconds       float64
}

func toolMetricReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		t.Setenv(key, "")
	}
	previous, propagator := otel.GetMeterProvider(), otel.GetTextMapPropagator()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := observability.Setup(context.Background(), "test", logger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetMeterProvider(previous)
		_, _ = observability.Setup(context.Background(), "test", logger)
		otel.SetTextMapPropagator(propagator)
	})
	return reader
}

func collectToolMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]*observedToolPoint {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	points := map[string]*observedToolPoint{}
	point := func(attrs attribute.Set) *observedToolPoint {
		name, _ := attrs.Value(attribute.Key("mcp.tool.name"))
		outcome, _ := attrs.Value(attribute.Key("mcp.tool.outcome"))
		key := name.AsString() + "/" + outcome.AsString()
		if points[key] == nil {
			points[key] = &observedToolPoint{name: name.AsString(), outcome: outcome.AsString()}
		}
		return points[key]
	}
	for _, scope := range collected.ScopeMetrics {
		for _, metric := range scope.Metrics {
			switch data := metric.Data.(type) {
			case metricdata.Sum[int64]:
				if metric.Name == "tutor.mcp.tool.calls" {
					for _, p := range data.DataPoints {
						point(p.Attributes).calls += p.Value
					}
				}
			case metricdata.Histogram[float64]:
				if metric.Name == "tutor.mcp.tool.duration" {
					for _, p := range data.DataPoints {
						v := point(p.Attributes)
						v.durations += p.Count
						v.seconds += p.Sum
					}
				}
			}
		}
	}
	return points
}

func requireToolMetric(t *testing.T, reader *sdkmetric.ManualReader, name, outcome string, count int64) *observedToolPoint {
	t.Helper()
	points := collectToolMetrics(t, reader)
	p := points[name+"/"+outcome]
	if len(points) != 1 || p == nil || p.calls != count || p.durations != uint64(count) {
		t.Fatalf("metrics=%+v, want exactly %s/%s count %d", points, name, outcome, count)
	}
	return p
}

type observedTransactionStore struct {
	storeport.Store
	before, after       func()
	beginErr, commitErr error
	delay               time.Duration
}

func (s *observedTransactionStore) WithTenantTx(ctx context.Context, _ models.TenantScope, fn func(context.Context, storeport.Store) error) error {
	if s.before != nil {
		s.before()
	}
	time.Sleep(s.delay)
	if s.beginErr != nil {
		return s.beginErr
	}
	if err := fn(ctx, s); err != nil {
		return err
	}
	if s.after != nil {
		s.after()
	}
	time.Sleep(s.delay)
	return s.commitErr
}
func (s *observedTransactionStore) GetDomainByLearner(context.Context, string) (*models.Domain, error) {
	return &models.Domain{ID: "domain", Name: "domain", Graph: models.KnowledgeSpace{Concepts: []string{"a"}}}, nil
}

func TestToolObservationIncludesTransactionAndFinalCommitResult(t *testing.T) {
	for _, mode := range []string{"success", "begin_failure", "commit_failure", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			reader := toolMetricReader(t)
			checkNotObserved := func() {
				if got := collectToolMetrics(t, reader); len(got) != 0 {
					t.Errorf("observed before transaction completed: %+v", got)
				}
			}
			store := &observedTransactionStore{before: checkNotObserved, after: checkNotObserved, delay: 15 * time.Millisecond}
			deps := &Deps{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if mode == "begin_failure" {
				store.beginErr = errors.New("pool unavailable")
			}
			if mode == "commit_failure" {
				store.commitErr = errors.New("commit unavailable")
			}
			if mode == "deadline" {
				deps.ToolCallTimeout = 5 * time.Millisecond
			}
			started := time.Now()
			result := callTool(t, deps, RegisterTools, "L_owner", "validate_domain_graph", map[string]any{})
			want := "succeeded"
			if mode != "success" {
				want = "failed"
			}
			if result.IsError != (want == "failed") {
				t.Fatalf("final error=%v mode=%s", result.IsError, mode)
			}
			p := requireToolMetric(t, reader, "validate_domain_graph", want, 1)
			minimum := store.delay
			if mode != "begin_failure" {
				minimum *= 2
			}
			if p.seconds < minimum.Seconds() || p.seconds > time.Since(started).Seconds() {
				t.Fatalf("duration=%fs, minimum=%s elapsed=%s", p.seconds, minimum, time.Since(started))
			}
		})
	}
}

type observationReplayStore struct {
	storeport.Store
	writes int
}

func (s *observationReplayStore) UpdateLearnerProfile(ctx context.Context, id, profile string, objective *string) error {
	s.writes++
	return s.Store.UpdateLearnerProfile(ctx, id, profile, objective)
}

func TestToolObservationCountsReplayAndConflict(t *testing.T) {
	reader := toolMetricReader(t)
	_, deps := setupToolsTest(t)
	store := &observationReplayStore{Store: deps.Store}
	deps.Store = store
	for i := 0; i < 2; i++ {
		result := callTool(t, deps, RegisterTools, "L_owner", "update_learner_profile", map[string]any{"idempotency_key": "observed-key", "language": "fr"})
		if result.IsError {
			t.Fatalf("call%d: %v", i, result)
		}
	}
	if store.writes != 1 {
		t.Fatalf("handler writes=%d", store.writes)
	}
	requireToolMetric(t, reader, "update_learner_profile", "succeeded", 2)
	result := callTool(t, deps, RegisterTools, "L_owner", "update_learner_profile", map[string]any{"idempotency_key": "observed-key", "language": "en"})
	if !result.IsError {
		t.Fatal("expected conflict")
	}
	points := collectToolMetrics(t, reader)
	if len(points) != 2 || points["update_learner_profile/failed"] == nil || points["update_learner_profile/failed"].calls != 1 || store.writes != 1 {
		t.Fatalf("conflict metrics=%+v writes=%d", points, store.writes)
	}
}

func TestToolObservationTenantRefusals(t *testing.T) {
	for _, mode := range []string{"missing_principal", "progress_role", "learning_role"} {
		t.Run(mode, func(t *testing.T) {
			reader := toolMetricReader(t)
			ctx := context.Background()
			name := "validate_domain_graph"
			if mode != "missing_principal" {
				principal := models.LegacyPrincipal("L_owner")
				if mode == "learning_role" {
					principal.Roles = []string{models.RoleBillingAdmin}
				} else {
					name = "get_learner_progress"
				}
				var err error
				ctx, err = auth.WithPrincipal(ctx, principal)
				if err != nil {
					t.Fatal(err)
				}
			}
			deps := &Deps{Store: &observedTransactionStore{before: func() { t.Error("refused request opened transaction") }}}
			h := toolCallObservationMiddleware(map[string]struct{}{name: {}})(toolTenantTransactionMiddleware(deps)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
				t.Error("refused request reached handler")
				return nil, nil
			}))
			_, _ = h(ctx, "tools/call", idempotencyToolRequest(t, name, "deny", map[string]any{}))
			requireToolMetric(t, reader, name, "denied", 1)
		})
	}
}

func TestToolObservationScopeRefusalsSDKAndHTTP(t *testing.T) {
	for _, transport := range []string{"sdk", "http"} {
		t.Run(transport, func(t *testing.T) {
			reader := toolMetricReader(t)
			if transport == "http" {
				h := OAuthScopeHTTPMiddleware("https://test.example", 4096, true, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("denied HTTP request entered SDK") }))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, scopedHTTPRequest(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"record_interaction","arguments":{}}}`, models.OAuthScopeLearnerRead))
				if w.Code != 403 {
					t.Fatalf("status=%d", w.Code)
				}
			} else {
				ctx := auth.WithOAuthScope(context.Background(), models.OAuthScopeLearnerRead)
				h := toolCallObservationMiddleware(map[string]struct{}{"record_interaction": {}})(toolOAuthScopeMiddleware("", true)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
					t.Error("scope denied reached handler")
					return nil, nil
				}))
				_, _ = h(ctx, "tools/call", idempotencyToolRequest(t, "record_interaction", "deny", map[string]any{}))
			}
			requireToolMetric(t, reader, "record_interaction", "denied", 1)
		})
	}
}

func TestToolObservationFinalFailureOverridesDenial(t *testing.T) {
	for _, mode := range []string{"deadline", "error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			reader := toolMetricReader(t)
			next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				result, _ := errorResult("refused")
				markToolCallDenied(ctx, result)
				switch mode {
				case "deadline":
					<-ctx.Done()
					return result, nil
				case "error":
					return result, errors.New("final failure")
				default:
					panic("private panic")
				}
			}
			if mode == "deadline" {
				next = ToolCallDeadlineMiddleware(time.Millisecond)(next)
			}
			h := toolCallObservationMiddleware(map[string]struct{}{"record_interaction": {}})(next)
			func() {
				defer func() {
					if recovered := recover(); mode != "panic" && recovered != nil {
						t.Errorf("unexpected panic")
					}
				}()
				_, _ = h(context.Background(), "tools/call", idempotencyToolRequest(t, "record_interaction", "final", map[string]any{}))
			}()
			requireToolMetric(t, reader, "record_interaction", "failed", 1)
		})
	}
}

func TestToolObservationBoundsUnknownNamesAndIgnoresOtherMethods(t *testing.T) {
	reader := toolMetricReader(t)
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return nil, errors.New("unknown method/tool")
	}
	h := toolCallObservationMiddleware(map[string]struct{}{"registered": {}})(next)
	_, _ = h(context.Background(), "tools/list", nil)
	if got := collectToolMetrics(t, reader); len(got) != 0 {
		t.Fatalf("tools/list observed=%v", got)
	}
	for i := 0; i < 100; i++ {
		_, _ = h(context.Background(), "tools/call", idempotencyToolRequest(t, fmt.Sprintf("attacker-%d", i), "key", map[string]any{}))
	}
	requireToolMetric(t, reader, "unknown", "failed", 100)
}
