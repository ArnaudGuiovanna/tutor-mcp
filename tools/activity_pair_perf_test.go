// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	pairPerfDefaultActiveLearners = 50
	pairPerfDefaultConcepts       = 12
	pairPerfDefaultInteractions   = 24
)

// TestGetPendingAlertsThenNextActivityP95Budget is an explicit CI/release gate
// for the first-turn pair expected by the client handoff. It is skipped by
// default to keep normal unit-test runs resilient to noisy shared runners.
//
// Enable with:
//
//	MCP_PERF_BUDGET=1 go test ./tools -run TestGetPendingAlertsThenNextActivityP95Budget -count=1
//
// Optional knobs:
//
//	MCP_PERF_ACTIVE_LEARNERS=200 MCP_PERF_P95_BUDGET_MS=2000
func TestGetPendingAlertsThenNextActivityP95Budget(t *testing.T) {
	if !pairPerfBudgetEnabled() {
		t.Skip("set MCP_PERF_BUDGET=1 to enforce the paired tool p95 budget")
	}

	activeLearners := pairPerfEnvInt(t, "MCP_PERF_ACTIVE_LEARNERS", pairPerfDefaultActiveLearners)
	samples := pairPerfEnvInt(t, "MCP_PERF_SAMPLES", activeLearners)
	budget := pairPerfEnvDurationMS(t, "MCP_PERF_P95_BUDGET_MS", pairPerfDefaultP95Budget(activeLearners))
	warmup := pairPerfEnvInt(t, "MCP_PERF_WARMUP", min(3, activeLearners))

	store, deps := setupToolsTest(t)
	fixture := seedAlertActivityPairFixture(t, store, activeLearners)
	callPair := newAlertActivityPairCaller(t, deps)

	for i := 0; i < warmup; i++ {
		callPair(fixture.learners[i%len(fixture.learners)])
	}

	durations := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		durations = append(durations, callPair(fixture.learners[i%len(fixture.learners)]))
	}

	p50 := percentileDuration(durations, 0.50)
	p95 := percentileDuration(durations, 0.95)
	t.Logf("get_pending_alerts + get_next_activity: active_learners=%d samples=%d warmup=%d p50=%s p95=%s budget=%s",
		activeLearners, samples, warmup, p50, p95, budget)
	if p95 > budget {
		t.Fatalf("paired tool p95 budget exceeded: p95=%s budget=%s active_learners=%d samples=%d",
			p95, budget, activeLearners, samples)
	}
}

// Keep the performance harness's identity contract covered even when the
// environment-dependent latency gate is disabled during ordinary unit tests.
func TestAlertActivityPairCarriesPedagogicalPrincipal(t *testing.T) {
	store, deps := setupToolsTest(t)
	fixture := seedAlertActivityPairFixture(t, store, 2)
	callPair := newAlertActivityPairCaller(t, deps)
	for _, learnerID := range fixture.learners {
		var scopedStates int
		if err := store.RawDB().QueryRow(`SELECT COUNT(*) FROM concept_states
 WHERE learner_id = ? AND domain_id <> ''`, learnerID).Scan(&scopedStates); err != nil || scopedStates != pairPerfDefaultConcepts {
			t.Fatalf("performance fixture states are not domain-scoped: count=%d err=%v", scopedStates, err)
		}
		callPair(learnerID)
		var count int
		if err := store.RawDB().QueryRow(`SELECT COUNT(*) FROM pedagogical_decisions
 WHERE tenant_id = ? AND learner_id = ?`, models.LegacyTenantID, learnerID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("paired call did not freeze learner's decision: learner=%s count=%d err=%v", learnerID, count, err)
		}
	}
}

func BenchmarkGetPendingAlertsThenNextActivityPair(b *testing.B) {
	activeLearners := pairPerfEnvInt(b, "MCP_PERF_ACTIVE_LEARNERS", pairPerfDefaultActiveLearners)
	store, deps := setupBenchTools(b)
	fixture := seedAlertActivityPairFixture(b, store, activeLearners)
	callPair := newAlertActivityPairCaller(b, deps)

	durations := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		durations = append(durations, callPair(fixture.learners[i%len(fixture.learners)]))
	}
	b.StopTimer()

	p50 := percentileDuration(durations, 0.50)
	p95 := percentileDuration(durations, 0.95)
	b.ReportMetric(float64(p50.Microseconds()), "p50_us")
	b.ReportMetric(float64(p95.Microseconds()), "p95_us")
	b.ReportMetric(float64(pairPerfDefaultP95Budget(activeLearners).Milliseconds()), "default_budget_ms")
}

type alertActivityPairFixture struct {
	learners []string
}

func seedAlertActivityPairFixture(tb testing.TB, store *db.Store, activeLearners int) alertActivityPairFixture {
	tb.Helper()
	if activeLearners <= 0 {
		tb.Fatalf("active learner count must be positive, got %d", activeLearners)
	}

	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	learners := make([]string, 0, activeLearners)
	for learnerIdx := 0; learnerIdx < activeLearners; learnerIdx++ {
		learnerID := fmt.Sprintf("L_perf_%03d", learnerIdx)
		learners = append(learners, learnerID)
		if _, err := store.RawDB().Exec(
			`INSERT INTO learners (id, email, password_hash, objective, created_at)
			 VALUES (?, ?, 'hash', 'perf fixture', ?)`,
			learnerID, learnerID+"@test.com", base,
		); err != nil {
			tb.Fatalf("seed learner %s: %v", learnerID, err)
		}

		concepts := pairPerfConcepts(pairPerfDefaultConcepts)
		relevance := make(map[string]float64, len(concepts))
		for conceptIdx, concept := range concepts {
			relevance[concept] = 1.0 - float64(conceptIdx%6)*0.08
		}
		domain, err := store.CreateDomainWithValueFramings(context.Background(), learnerID, "math", "ship a stable MVP", models.KnowledgeSpace{
			Concepts:      concepts,
			Prerequisites: pairPerfPrerequisites(concepts),
		}, "")
		if err != nil {
			tb.Fatalf("seed domain for %s: %v", learnerID, err)
		}
		if err := store.UpdateDomainPhase(context.Background(), domain.ID, models.PhaseInstruction, 0, base); err != nil {
			tb.Fatalf("seed phase for %s: %v", learnerID, err)
		}
		if _, err := store.MergeDomainGoalRelevance(context.Background(), domain.ID, relevance); err != nil {
			tb.Fatalf("seed goal relevance for %s: %v", learnerID, err)
		}

		for conceptIdx, concept := range concepts {
			lastReview := base.Add(-time.Duration(conceptIdx+1) * 24 * time.Hour)
			cs := models.NewConceptStateInDomain(learnerID, domain.ID, concept)
			cs.CardState = "review"
			cs.PMastery = 0.42 + float64((learnerIdx+conceptIdx)%7)*0.06
			if conceptIdx%5 == 0 {
				cs.PMastery = 0.9
			}
			cs.Stability = 2 + float64(conceptIdx%5)
			cs.Difficulty = 0.25 + float64(conceptIdx%4)*0.08
			cs.ElapsedDays = 2 + conceptIdx%4
			cs.ScheduledDays = 1 + conceptIdx%3
			cs.Reps = 2 + conceptIdx%4
			cs.LastReview = &lastReview
			if err := store.UpsertConceptState(context.Background(), cs); err != nil {
				tb.Fatalf("seed concept state %s/%s: %v", learnerID, concept, err)
			}
		}

		for interactionIdx := 0; interactionIdx < pairPerfDefaultInteractions; interactionIdx++ {
			concept := concepts[(learnerIdx+interactionIdx)%len(concepts)]
			success := interactionIdx%5 != 0
			misconceptionType := ""
			misconceptionDetail := ""
			if !success && concept == concepts[0] {
				misconceptionType = "sign_error"
				misconceptionDetail = "lost the sign while transforming the expression"
			}
			createdAt := base.Add(time.Duration(learnerIdx*pairPerfDefaultInteractions+interactionIdx) * time.Second)
			insertPairPerfInteraction(tb, store.RawDB(), learnerID, domain.ID, concept, success, misconceptionType, misconceptionDetail, createdAt)
		}

		insertPairPerfAffect(tb, store.RawDB(), learnerID, "baseline", 3, 3, 3, 2, 0.45, base.Add(6*time.Hour))
		insertPairPerfAffect(tb, store.RawDB(), learnerID, "recent_low_1", 2, 2, 2, 3, 0.35, base.Add(7*time.Hour))
		insertPairPerfAffect(tb, store.RawDB(), learnerID, "recent_low_2", 2, 2, 1, 3, 0.30, base.Add(8*time.Hour))
	}

	return alertActivityPairFixture{learners: learners}
}

func pairPerfConcepts(count int) []string {
	concepts := make([]string, count)
	for i := range concepts {
		concepts[i] = fmt.Sprintf("c%02d", i)
	}
	return concepts
}

func pairPerfPrerequisites(concepts []string) map[string][]string {
	prereqs := make(map[string][]string, len(concepts)-1)
	for i := 1; i < len(concepts); i++ {
		prereqs[concepts[i]] = []string{concepts[i-1]}
	}
	return prereqs
}

func insertPairPerfInteraction(
	tb testing.TB,
	raw *sql.DB,
	learnerID, domainID, concept string,
	success bool,
	misconceptionType, misconceptionDetail string,
	createdAt time.Time,
) {
	tb.Helper()
	successInt := 0
	if success {
		successInt = 1
	}
	if _, err := raw.Exec(
		`INSERT INTO interactions
		    (learner_id, concept, activity_type, success, response_time, confidence,
		     hints_requested, self_initiated, is_proactive_review, misconception_type,
		     misconception_detail, domain_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		learnerID, concept, string(models.ActivityPractice), successInt,
		700+len(concept)*10, 0.55, 1, 1, 0,
		pairPerfNullString(misconceptionType), pairPerfNullString(misconceptionDetail),
		domainID, createdAt,
	); err != nil {
		tb.Fatalf("seed interaction %s/%s: %v", learnerID, concept, err)
	}
}

func insertPairPerfAffect(
	tb testing.TB,
	raw *sql.DB,
	learnerID, sessionID string,
	energy, subjectConfidence, satisfaction, perceivedDifficulty int,
	autonomyScore float64,
	createdAt time.Time,
) {
	tb.Helper()
	if _, err := raw.Exec(
		`INSERT INTO affect_states
		    (learner_id, session_id, energy, subject_confidence, satisfaction,
		     perceived_difficulty, next_session_intent, autonomy_score, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		learnerID, sessionID, energy, subjectConfidence, satisfaction,
		perceivedDifficulty, 2, autonomyScore, createdAt,
	); err != nil {
		tb.Fatalf("seed affect %s/%s: %v", learnerID, sessionID, err)
	}
}

func pairPerfNullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func newAlertActivityPairCaller(tb testing.TB, deps *Deps) func(string) time.Duration {
	tb.Helper()
	ctx := context.Background()
	var currentLearner atomic.Value
	currentLearner.Store("")

	server := mcp.NewServer(&mcp.Implementation{Name: "pair-perf", Version: "0.0.1"}, nil)
	registerGetPendingAlerts(server, deps)
	registerGetNextActivity(server, deps)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if learnerID, _ := currentLearner.Load().(string); learnerID != "" {
				var err error
				ctx, err = auth.WithPrincipal(ctx, models.LegacyPrincipal(learnerID))
				if err != nil {
					return nil, err
				}
				ctx = auth.WithOAuthScope(ctx, models.OAuthScopeLearner)
			}
			return next(ctx, method, req)
		}
	})

	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		tb.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "pair-perf-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = session.Close() })

	emptyArgs := json.RawMessage(`{}`)
	return func(learnerID string) time.Duration {
		currentLearner.Store(learnerID)
		start := time.Now()
		alerts, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "get_pending_alerts",
			Arguments: emptyArgs,
		})
		if err != nil {
			tb.Fatalf("get_pending_alerts transport error for %s: %v", learnerID, err)
		}
		if alerts.IsError {
			tb.Fatalf("get_pending_alerts for %s: %q", learnerID, resultText(alerts))
		}

		activity, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "get_next_activity",
			Arguments: emptyArgs,
		})
		if err != nil {
			tb.Fatalf("get_next_activity transport error for %s: %v", learnerID, err)
		}
		if activity.IsError {
			tb.Fatalf("get_next_activity for %s: %q", learnerID, resultText(activity))
		}
		return time.Since(start)
	}
}

func percentileDuration(samples []time.Duration, percentile float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func pairPerfBudgetEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MCP_PERF_BUDGET"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func pairPerfDefaultP95Budget(activeLearners int) time.Duration {
	if activeLearners <= pairPerfDefaultActiveLearners {
		return 500 * time.Millisecond
	}
	return 2 * time.Second
}

func pairPerfEnvInt(tb testing.TB, name string, defaultValue int) int {
	tb.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		tb.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return value
}

func pairPerfEnvDurationMS(tb testing.TB, name string, defaultValue time.Duration) time.Duration {
	tb.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		tb.Fatalf("%s must be a positive integer number of milliseconds, got %q", name, raw)
	}
	return time.Duration(value) * time.Millisecond
}
