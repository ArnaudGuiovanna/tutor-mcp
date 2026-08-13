// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

// TestLearnerContextPostgresLoadGate is an opt-in release/staging gate. Run it
// once at each documented fleet cardinality:
//
//	TUTOR_TEST_LEARNER_CONTEXT_CARDINALITY=200
//	TUTOR_TEST_LEARNER_CONTEXT_CARDINALITY=10000
//	TUTOR_TEST_LEARNER_CONTEXT_CARDINALITY=100000
//
// TUTOR_TEST_PG_DSN must point to PostgreSQL. setupTestDB creates and removes a
// private schema, so the measurement cannot pollute another run.
func TestLearnerContextPostgresLoadGate(t *testing.T) {
	rawCardinality := os.Getenv("TUTOR_TEST_LEARNER_CONTEXT_CARDINALITY")
	if rawCardinality == "" {
		t.Skip("set TUTOR_TEST_LEARNER_CONTEXT_CARDINALITY to run the learner-context load gate")
	}
	cardinality, err := strconv.Atoi(rawCardinality)
	if err != nil || cardinality < 1 || cardinality > 100000 {
		t.Fatalf("TUTOR_TEST_LEARNER_CONTEXT_CARDINALITY must be an integer between 1 and 100000, got %q", rawCardinality)
	}
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Fatal("TUTOR_TEST_PG_DSN is required for the learner-context load gate")
	}

	store := setupTestDB(t)
	if store.dialect != DialectPostgres {
		t.Fatal("learner-context load gate requires PostgreSQL")
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	seedStarted := time.Now()
	if cardinality > 1 {
		if _, err := store.root.ExecContext(ctx, `
			INSERT INTO learners
				(id, email, password_hash, objective, created_at, email_verified_at)
			SELECT 'load-' || n, 'load-' || n || '@test.invalid', 'hash', 'load', $1, $1
			FROM generate_series(1, $2) AS n`, now, cardinality-1); err != nil {
			t.Fatalf("seed %d learners: %v", cardinality, err)
		}
		// One unrelated interaction per synthetic learner makes the interaction
		// indexes prove tenant/learner scoping instead of benchmarking a table
		// containing only the dense target learner.
		if _, err := store.root.ExecContext(ctx, `
			INSERT INTO interactions
				(learner_id, concept, activity_type, success, created_at)
			SELECT 'load-' || n, 'noise', 'PRACTICE', 1, $1
			FROM generate_series(1, $2) AS n`, now.Add(-45*24*time.Hour), cardinality-1); err != nil {
			t.Fatalf("seed unrelated interactions: %v", err)
		}
	}

	concepts := make([]string, 100)
	for index := range concepts {
		concepts[index] = fmt.Sprintf("concept-%03d", index)
	}
	domain, err := store.CreateDomain(ctx, "L1", "load-gate", "", models.KnowledgeSpace{
		Concepts: concepts, Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create load-gate domain: %v", err)
	}
	for index, concept := range concepts {
		insertContextState(t, store, "L1", domain.ID, concept, 0.2+float64(index%7)/10)
	}
	if _, err := store.root.ExecContext(ctx, `
		INSERT INTO interactions
			(learner_id, domain_id, concept, activity_type, success, created_at)
		SELECT 'L1', $1,
		       'concept-' || LPAD(((n - 1) % 100)::text, 3, '0'),
		       'PRACTICE', CASE WHEN n % 3 = 0 THEN 0 ELSE 1 END,
		       $2::timestamptz - ((n % 90)::text || ' days')::interval
		FROM generate_series(1, 5000) AS n`, domain.ID, now); err != nil {
		t.Fatalf("seed target interactions: %v", err)
	}
	if _, err := store.root.ExecContext(ctx, `ANALYZE learners, domains, concept_states, interactions, affect_states, learning_sessions`); err != nil {
		t.Fatalf("analyze load-gate data: %v", err)
	}
	t.Logf("seeded cardinality=%d dense_interactions=5000 in %s", cardinality, time.Since(seedStarted))

	readPath := func() ([3]time.Duration, error) {
		var components [3]time.Duration
		started := time.Now()
		if _, err := store.GetLearnerContextOverview(ctx, "L1", now); err != nil {
			return components, err
		}
		components[0] = time.Since(started)
		started = time.Now()
		if _, err := store.GetLearnerContextNarrativeSignals(ctx, "L1", domain.ID, concepts, now); err != nil {
			return components, err
		}
		components[1] = time.Since(started)
		started = time.Now()
		_, _ = store.GetActiveLearningSession(ctx, "L1")
		components[2] = time.Since(started)
		return components, nil
	}
	for range 5 {
		if _, err := readPath(); err != nil {
			t.Fatalf("warm learner-context read: %v", err)
		}
	}
	const samples = 50
	durations := make([]time.Duration, samples)
	componentDurations := [3][]time.Duration{
		make([]time.Duration, samples),
		make([]time.Duration, samples),
		make([]time.Duration, samples),
	}
	for index := range durations {
		started := time.Now()
		components, err := readPath()
		if err != nil {
			t.Fatalf("measured learner-context read: %v", err)
		}
		durations[index] = time.Since(started)
		for component := range components {
			componentDurations[component][index] = components[component]
		}
	}
	percentile := func(values []time.Duration, quantile float64) time.Duration {
		sorted := append([]time.Duration(nil), values...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		return sorted[int(math.Ceil(float64(len(sorted))*quantile))-1]
	}
	p50 := percentile(durations, 0.50)
	p95 := percentile(durations, 0.95)
	// This hard database-path budget leaves variance headroom around the
	// original 50 ms staging target while remaining far below the 2 s MCP tool
	// budget. Component p95 values stay visible in the test log for diagnosis.
	const p95Budget = 75 * time.Millisecond
	t.Logf(
		"learner-context cardinality=%d samples=%d p50=%s p95=%s budget=%s component_p95 overview=%s narrative=%s active_session=%s",
		cardinality, samples, p50, p95, p95Budget,
		percentile(componentDurations[0], 0.95),
		percentile(componentDurations[1], 0.95),
		percentile(componentDurations[2], 0.95),
	)
	if p95 >= p95Budget {
		t.Fatalf("learner-context p95 %s exceeds budget %s at cardinality %d", p95, p95Budget, cardinality)
	}

	query, args := store.learnerContextNarrativeQuery("L1", domain.ID, concepts, now)
	rows, err := store.query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+query, args...)
	if err != nil {
		t.Fatalf("explain learner-context narrative query: %v", err)
	}
	var planLines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			_ = rows.Close()
			t.Fatalf("scan learner-context plan: %v", err)
		}
		planLines = append(planLines, line)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close learner-context plan: %v", err)
	}
	plan := strings.Join(planLines, "\n")
	t.Logf("learner-context narrative plan:\n%s", plan)
	// At the 200-learner gate almost the entire interactions table belongs to
	// the intentionally dense target, so PostgreSQL may correctly prefer a
	// sequential scan. At 100k, an interaction-index plan is mandatory: a full
	// population scan would make the read path cardinality-dependent.
	if cardinality == 100000 && !strings.Contains(plan, "idx_interactions_") {
		t.Fatalf("learner-context narrative plan did not use an interaction index:\n%s", plan)
	}
}
