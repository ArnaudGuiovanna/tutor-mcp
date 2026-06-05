package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

var calibTestCounter int

func setupCalibTest(t *testing.T) (*db.Store, *Deps) {
	t.Helper()
	calibTestCounter++
	dsn := fmt.Sprintf("file:calibmem_%s_%d?mode=memory&cache=shared", t.Name(), calibTestCounter)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(raw); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"L_owner", "L_attacker"} {
		_, err := raw.Exec(
			`INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES (?, ?, 'hash', 'test', ?)`,
			id, id+"@test.com", now,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { raw.Close() })
	store := db.NewStore(raw)
	deps := &Deps{
		Store:  store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return store, deps
}

// callRecordCalibration registers the tool, connects an in-memory MCP session
// with a receiving middleware that injects the given learner ID into ctx, then
// invokes record_calibration_result.
func callRecordCalibration(t *testing.T, deps *Deps, learnerID, predictionID string, actual float64) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	registerRecordCalibrationResult(server, deps)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			ctx = context.WithValue(ctx, auth.LearnerIDKey, learnerID)
			return next(ctx, method, req)
		}
	})

	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	argsJSON, _ := json.Marshal(map[string]any{
		"prediction_id": predictionID,
		"actual_score":  actual,
	})
	return session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "record_calibration_result",
		Arguments: json.RawMessage(argsJSON),
	})
}

func TestRecordCalibrationResult_OwnerAllowed(t *testing.T) {
	store, deps := setupCalibTest(t)

	rec := &models.CalibrationRecord{
		PredictionID: "cal_owner_1",
		LearnerID:    "L_owner",
		ConceptID:    "Concept_A",
		Predicted:    0.75,
	}
	if err := store.CreateCalibrationPrediction(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	res, err := callRecordCalibration(t, deps, "L_owner", "cal_owner_1", 0.80)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		var text string
		if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*mcp.TextContent); ok {
				text = tc.Text
			}
		}
		t.Fatalf("owner write should succeed, got error: %s", text)
	}

	// Verify the record was actually updated.
	saved, err := store.GetCalibrationRecord(context.Background(), "cal_owner_1", "L_owner")
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if saved.Actual == nil || *saved.Actual != 0.80 {
		t.Fatalf("expected actual=0.80, got %+v", saved.Actual)
	}
}

// callRecordCalibrationRaw is the same as callRecordCalibration but accepts
// a pre-built JSON arguments blob, so a test can inject values that the Go
// json encoder refuses (e.g. NaN).
func callRecordCalibrationRaw(t *testing.T, deps *Deps, learnerID string, argsJSON []byte) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	registerRecordCalibrationResult(server, deps)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			ctx = context.WithValue(ctx, auth.LearnerIDKey, learnerID)
			return next(ctx, method, req)
		}
	})

	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	return session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "record_calibration_result",
		Arguments: json.RawMessage(argsJSON),
	})
}

// TestRecordCalibrationResult_ActualScoreOutOfRange covers the issue #83
// gap left over from #25/#50 numeric validation: actual_score was being
// silently persisted regardless of magnitude or finiteness, corrupting the
// calibration bias estimate downstream. Each sub-case must be rejected
// either at the JSON layer (NaN literal) or by validateUnitInterval.
func TestRecordCalibrationResult_ActualScoreOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		// Raw JSON literal injected as actual_score so we can send NaN,
		// which encoding/json refuses to marshal.
		actualLiteral string
		want          string
	}{
		{"negative", "-0.1", "must be in [0, 1]"},
		{"too_large", "1.5", "must be in [0, 1]"},
		{"nan", "NaN", "finite"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, deps := setupCalibTest(t)

			predictionID := "cal_owner_" + tc.name
			rec := &models.CalibrationRecord{
				PredictionID: predictionID,
				LearnerID:    "L_owner",
				ConceptID:    "Concept_A",
				Predicted:    0.5,
			}
			if err := store.CreateCalibrationPrediction(context.Background(), rec); err != nil {
				t.Fatal(err)
			}

			argsJSON := []byte(fmt.Sprintf(
				`{"prediction_id":%q,"actual_score":%s}`,
				predictionID, tc.actualLiteral,
			))
			res, err := callRecordCalibrationRaw(t, deps, "L_owner", argsJSON)
			if err != nil {
				// NaN is not valid JSON; some validators reject it before
				// the handler runs. That is also an acceptable rejection
				// of a non-finite actual_score.
				if tc.name == "nan" {
					return
				}
				t.Fatalf("unexpected transport error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected error result for actual_score=%s, got success", tc.actualLiteral)
			}
			var text string
			if len(res.Content) > 0 {
				if tcc, ok := res.Content[0].(*mcp.TextContent); ok {
					text = tcc.Text
				}
			}
			if !strings.Contains(text, "actual_score") || !strings.Contains(text, tc.want) {
				t.Fatalf("expected error mentioning actual_score and %q, got %q", tc.want, text)
			}

			// Record must NOT be mutated when validation rejects.
			saved, err := store.GetCalibrationRecord(context.Background(), predictionID, "L_owner")
			if err != nil {
				t.Fatalf("get record: %v", err)
			}
			if saved.Actual != nil {
				t.Fatalf("record should remain untouched, got actual=%v", *saved.Actual)
			}
		})
	}
}

// _ = math.NaN keeps the math import required even when no sub-case below
// directly references math; the test above relies on JSON-literal NaN.
var _ = math.NaN

// TestRecordCalibrationResult_RejectsForeignPredictionID is the issue #87
// regression: ownership is now enforced at the DB layer, so even if the tool
// handler skipped its (now-removed) manual check, calling the MCP tool with
// another learner's prediction_id must error and leave the row untouched.
func TestRecordCalibrationResult_RejectsForeignPredictionID(t *testing.T) {
	store, deps := setupCalibTest(t)

	rec := &models.CalibrationRecord{
		PredictionID: "cal_foreign_1",
		LearnerID:    "L_owner",
		ConceptID:    "Concept_A",
		Predicted:    0.40,
	}
	if err := store.CreateCalibrationPrediction(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	res, err := callRecordCalibration(t, deps, "L_attacker", "cal_foreign_1", 0.95)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true, got success")
	}

	// Row must remain unmodified — Actual still nil, owner still L_owner.
	saved, err := store.GetCalibrationRecord(context.Background(), "cal_foreign_1", "L_owner")
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if saved.Actual != nil {
		t.Fatalf("expected Actual nil after rejected foreign call, got %v", *saved.Actual)
	}
	if saved.LearnerID != "L_owner" {
		t.Fatalf("LearnerID = %q want L_owner", saved.LearnerID)
	}
}

func TestRecordCalibrationResult_ForeignLearnerRejected(t *testing.T) {
	store, deps := setupCalibTest(t)

	rec := &models.CalibrationRecord{
		PredictionID: "cal_victim_1",
		LearnerID:    "L_owner",
		ConceptID:    "Concept_A",
		Predicted:    0.30,
	}
	if err := store.CreateCalibrationPrediction(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	res, err := callRecordCalibration(t, deps, "L_attacker", "cal_victim_1", 0.99)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result, got success")
	}
	var text string
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	if !strings.Contains(text, "not found") {
		t.Fatalf("expected neutral 'not found' message, got %q", text)
	}

	// Verify the record was NOT modified.
	saved, err := store.GetCalibrationRecord(context.Background(), "cal_victim_1", "L_owner")
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if saved.Actual != nil {
		t.Fatalf("record should remain untouched, got actual=%v", *saved.Actual)
	}
	if saved.LearnerID != "L_owner" {
		t.Fatalf("owner should remain L_owner, got %q", saved.LearnerID)
	}
}
