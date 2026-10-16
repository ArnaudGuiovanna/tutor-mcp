package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var mcpHTTPLoadTools = [...]string{"get_learner_context", "get_pending_alerts", "get_next_activity", "update_learner_profile"}

type mcpHTTPLoadLearner struct {
	actor             models.Principal
	domain            *models.Domain
	client            *mcp.ClientSession
	objective         string
	recent            [3]memory.SessionPayload
	recentKeys        [3]memory.NarrativeKey
	decision, session string
	concepts          map[string]bool
}

func seedMCPHTTPLoadLearner(t *testing.T, ctx context.Context, f *mcpHTTPFixture, actor models.Principal, ordinal int) *mcpHTTPLoadLearner {
	t.Helper()
	learner := &mcpHTTPLoadLearner{actor: actor, objective: "seed objective for " + actor.LearnerID, concepts: map[string]bool{}}
	concepts := make([]string, 12)
	relevance := make(map[string]float64, 12)
	for i := range concepts {
		concepts[i] = fmt.Sprintf("concept-%02d", i)
		learner.concepts[concepts[i]] = true
		relevance[concepts[i]] = 1 - float64(i%6)*0.08
	}
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	limits, err := memory.LimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		if err := scoped.UpdateLearnerProfile(txCtx, actor.LearnerID, `{"device":"laptop","language":"en"}`, &learner.objective); err != nil {
			return err
		}
		var err error
		learner.domain, err = scoped.CreateDomain(txCtx, actor.LearnerID, "practice-"+actor.LearnerID, "representative private history", models.KnowledgeSpace{Concepts: concepts, Prerequisites: map[string][]string{concepts[1]: {concepts[0]}}})
		if err != nil {
			return err
		}
		if err := scoped.UpdateDomainPhase(txCtx, learner.domain.ID, models.PhaseInstruction, 0, base); err != nil {
			return err
		}
		if _, err := scoped.MergeDomainGoalRelevance(txCtx, actor.LearnerID, learner.domain.ID, relevance); err != nil {
			return err
		}
		for i, concept := range concepts {
			last := base.Add(-time.Duration(i+1) * 24 * time.Hour)
			state := models.NewConceptStateInDomain(actor.LearnerID, learner.domain.ID, concept)
			state.CardState, state.PMastery = "review", 0.42+float64((ordinal+i)%7)*0.06
			state.Stability, state.Difficulty = 2+float64(i%5), 3+float64(i%4)
			state.Reps, state.ScheduledDays, state.ElapsedDays, state.LastReview = 2+i%4, 1+i%3, 2+i%4, &last
			if err := scoped.UpsertConceptState(txCtx, state); err != nil {
				return err
			}
		}
		for i := range 24 {
			if err := scoped.CreateInteraction(txCtx, &models.Interaction{LearnerID: actor.LearnerID, DomainID: learner.domain.ID,
				Concept: concepts[i%len(concepts)], ActivityType: string(models.ActivityPractice), Success: i%5 != 0, Confidence: 0.65,
				ResponseTime: 30, Notes: "historical unverified practice", CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
				return err
			}
		}
		if err := f.runtime.EnsureRecoveryEnrollment(txCtx, actor.LearnerID); err != nil {
			return err
		}
		for i := range 32 {
			at := base.Add(time.Duration(i) * time.Minute)
			body := fmt.Sprintf("mcp-http-history learner=%s session=%02d", actor.LearnerID, i)
			content := "---\ndomain_id: " + learner.domain.ID + "\nlearner_id: " + actor.LearnerID + "\n---\n" + body
			// Sessions remain learner-scoped in their canonical PK/AAD. The
			// actual domain lives in authenticated plaintext/index metadata.
			object, replayed, err := f.runtime.CompareAndSwapNarrative(txCtx, memory.NarrativeKey{LearnerID: actor.LearnerID, Scope: memory.ScopeSession, Key: at.Format("2006-01-02T15-04-05Z")}, 0, content, "", "", limits)
			if err != nil {
				return err
			}
			if replayed || object.DomainID != "" || object.TenantID != actor.TenantID || object.EnrollmentID == "" {
				return fmt.Errorf("session seed lost its canonical tenant/enrollment key")
			}
			if i >= 29 {
				rank := 31 - i
				learner.recent[rank] = memory.SessionPayload{Timestamp: at, Body: body}
				learner.recentKeys[rank] = object.NarrativeKey
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertMCPHTTPEncryptedHistory(t, ctx, f, learner, 32)
	return learner
}

func assertMCPHTTPEncryptedHistory(t *testing.T, ctx context.Context, f *mcpHTTPFixture, learner *mcpHTTPLoadLearner, want int) {
	t.Helper()
	// Inspect stored envelopes outside every measured interval. Apply the
	// tenant even to the administrative inspection, without relying on bypass.
	func() {
		tx, err := f.admin.RawDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		query := `SELECT ciphertext,key_id,aad_version,domain_id,enrollment_id FROM narrative_objects WHERE tenant_id=? AND learner_id=? AND scope='session'`
		if f.postgres {
			if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant',$1,true)`, learner.actor.TenantID); err != nil {
				t.Fatal(err)
			}
			query = `SELECT ciphertext,key_id,aad_version,domain_id,enrollment_id FROM narrative_objects WHERE tenant_id=$1 AND learner_id=$2 AND scope='session'`
		}
		rows, err := tx.QueryContext(ctx, query, learner.actor.TenantID, learner.actor.LearnerID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var ciphertext, key, domain, enrollment string
			var aad int
			if err := rows.Scan(&ciphertext, &key, &aad, &domain, &enrollment); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(ciphertext, "enc:v1:http:") || strings.Contains(ciphertext, "mcp-http-history") || key != "http" || aad != 2 || domain != "" || enrollment == "" {
				t.Fatal("session envelope is plaintext or lost its canonical encryption identity")
			}
			count++
		}
		if err := rows.Err(); err != nil || count != want {
			t.Fatalf("encrypted history count=%d want=%d err=%v", count, want, err)
		}
	}()
	// A positive read through the real restricted runtime must decrypt the
	// owner's canonical keys, rather than merely hiding all rows with RLS.
	if err := f.runtime.WithTenantReadTx(ctx, learner.actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		for i, key := range learner.recentKeys {
			object, err := f.runtime.GetNarrative(txCtx, key)
			if err != nil {
				return err
			}
			parsed, err := memory.ParseSessionPayload(learner.recent[i].Timestamp, object.Content)
			if err != nil || parsed.Body != learner.recent[i].Body || parsed.Frontmatter["domain_id"] != learner.domain.ID {
				return fmt.Errorf("positive owner narrative read failed: %v", err)
			}
		}
		states, err := scoped.GetConceptStatesByDomain(txCtx, learner.actor.LearnerID, learner.domain.ID)
		if err != nil || len(states) != 12 {
			return fmt.Errorf("seed concept states=%d: %v", len(states), err)
		}
		interactions, err := scoped.GetRecentInteractionsByDomain(txCtx, learner.actor.LearnerID, learner.domain.ID, 100)
		if err != nil || len(interactions) != 24 {
			return fmt.Errorf("seed interactions=%d: %v", len(interactions), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func checkMCPHTTPLoadActivity(payload map[string]any, learner *mcpHTTPLoadLearner) error {
	if payload["needs_domain_setup"] != false || payload["domain_id"] != learner.domain.ID {
		return fmt.Errorf("activity lost its initialized domain")
	}
	if degraded, present := payload["degraded_components"]; present {
		components, ok := degraded.([]any)
		if !ok || len(components) != 0 {
			return fmt.Errorf("activity degraded: %v", degraded)
		}
	}
	activity, ok := payload["activity"].(map[string]any)
	activityType, typeOK := activity["type"].(string)
	if !ok || !typeOK || activityType == "" || !learner.concepts[fmt.Sprint(activity["concept"])] {
		return fmt.Errorf("activity did not select an owned concept: %v", payload["activity"])
	}
	decision, _ := payload["decision_id"].(string)
	session, _ := payload["session_id"].(string)
	contract, ok := payload["pedagogical_contract"].(map[string]any)
	if decision == "" || decision == learner.decision || session == "" || !ok || contract["decision_id"] != decision {
		return fmt.Errorf("activity did not freeze a fresh decision linked to a coherent session")
	}
	episodic, ok := payload["episodic_context"].(map[string]any)
	if !ok {
		return fmt.Errorf("activity omitted encrypted narrative context")
	}
	recent, ok := episodic["recent_sessions"].([]any)
	if !ok || len(recent) != 3 {
		return fmt.Errorf("activity recent sessions=%v; want three", episodic["recent_sessions"])
	}
	for i, entry := range recent {
		session, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid session payload")
		}
		frontmatter, ok := session["frontmatter"].(map[string]any)
		at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(session["timestamp"]))
		if !ok || frontmatter["domain_id"] != learner.domain.ID || frontmatter["learner_id"] != learner.actor.LearnerID || session["body"] != learner.recent[i].Body || err != nil || !at.Equal(learner.recent[i].Timestamp) {
			return fmt.Errorf("activity selected wrong narrative at rank %d", i)
		}
	}
	learner.decision, learner.session = decision, session
	return nil
}

func runMCPHTTPLoadSequence(ctx context.Context, learner *mcpHTTPLoadLearner, phase string) ([4]time.Duration, error) {
	var durations [4]time.Duration
	for i, tool := range mcpHTTPLoadTools {
		args := map[string]any{"domain_id": learner.domain.ID}
		objective := phase + " objective for " + learner.actor.LearnerID
		switch tool {
		case "get_next_activity":
			args["idempotency_key"] = "http-load:" + phase + ":activity:" + learner.actor.LearnerID
		case "update_learner_profile":
			args = map[string]any{"objective": objective, "idempotency_key": "http-load:" + phase + ":profile:" + learner.actor.LearnerID}
		}
		started := time.Now()
		_, payload, err := callMCPHTTPTool(ctx, learner.client, tool, args)
		durations[i] = time.Since(started) // transport, auth, waits, commit and decode
		if err != nil {
			return durations, err
		}
		switch tool {
		case "get_learner_context":
			err = checkMCPHTTPContext(payload, learner.actor.LearnerID, learner.domain.ID, learner.objective)
			if err == nil && (payload["needs_domain_setup"] != false || payload["concepts_count"] != float64(12)) {
				err = fmt.Errorf("learner context lost the twelve seeded concept states")
			}
		case "get_pending_alerts":
			alerts, alertsOK := payload["alerts"].([]any)
			_, criticalOK := payload["has_critical"].(bool)
			if !alertsOK || !criticalOK || payload["needs_domain_setup"] != false {
				err = fmt.Errorf("pending alerts omitted its initialized domain view")
				break
			}
			for _, entry := range alerts {
				alert, ok := entry.(map[string]any)
				if !ok {
					err = fmt.Errorf("pending alerts returned an invalid alert")
					break
				}
				if domain, _ := alert["domain_id"].(string); domain != "" && domain != learner.domain.ID {
					err = fmt.Errorf("pending alerts returned a foreign domain")
					break
				}
				if concept, _ := alert["concept"].(string); concept != "" && !learner.concepts[concept] {
					err = fmt.Errorf("pending alerts returned a foreign concept")
					break
				}
			}
		case "get_next_activity":
			err = checkMCPHTTPLoadActivity(payload, learner)
		case "update_learner_profile":
			if payload["updated"] != true || payload["fields_changed"] != float64(1) {
				err = fmt.Errorf("fresh profile mutation did not update exactly its objective")
			} else {
				learner.objective = objective
			}
		}
		if err != nil {
			return durations, fmt.Errorf("%s: %w", tool, err)
		}
	}
	return durations, nil
}

func assertMCPHTTPLoadDurable(t *testing.T, ctx context.Context, f *mcpHTTPFixture, learner *mcpHTTPLoadLearner) {
	t.Helper()
	if err := f.runtime.WithTenantReadTx(ctx, learner.actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		profile, err := scoped.GetLearnerByID(txCtx, learner.actor.LearnerID)
		if err != nil {
			return err
		}
		if profile.Objective != learner.objective {
			return fmt.Errorf("fresh objective did not persist for its own learner")
		}
		session, err := scoped.GetLearningSession(txCtx, learner.actor.LearnerID, learner.session)
		if err != nil {
			return err
		}
		if session.LearnerID != learner.actor.LearnerID || session.DomainID != learner.domain.ID || session.Status != models.LearningSessionStatusOpen {
			return fmt.Errorf("activity session ownership/status did not persist")
		}
		decision, err := scoped.GetPedagogicalDecision(txCtx, learner.actor.TenantScope(), learner.decision)
		if err != nil {
			return err
		}
		if decision.LearnerID != learner.actor.LearnerID || decision.DomainID != learner.domain.ID || decision.SessionID != learner.session || decision.Contract.DecisionID != learner.decision {
			return fmt.Errorf("activity decision ownership/contract did not persist")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func mcpHTTPPercentile(values []time.Duration, quantile float64) time.Duration {
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return ordered[max(0, int(math.Ceil(float64(len(ordered))*quantile))-1)]
}

func seedMCPHTTPForeignHistory(t *testing.T, ctx context.Context, f *mcpHTTPFixture, learner *mcpHTTPLoadLearner) {
	t.Helper()
	limits, err := memory.LimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.WithTenantTx(ctx, learner.actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		foreign, err := scoped.CreateDomain(txCtx, learner.actor.LearnerID, "foreign-history", "unrelated course", models.KnowledgeSpace{Concepts: []string{"foreign-concept"}})
		if err != nil {
			return err
		}
		base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
		if !base.After(learner.recent[0].Timestamp) {
			return fmt.Errorf("foreign history must be newer than every requested-domain session")
		}
		for i := range 1024 {
			at := base.Add(time.Duration(i) * time.Minute)
			content := fmt.Sprintf("---\ndomain_id: %s\nlearner_id: %s\n---\nmcp-http-history foreign=%04d", foreign.ID, learner.actor.LearnerID, i)
			if _, replayed, err := f.runtime.CompareAndSwapNarrative(txCtx, memory.NarrativeKey{LearnerID: learner.actor.LearnerID, Scope: memory.ScopeSession, Key: at.Format("2006-01-02T15-04-05Z")}, 0, content, "", "", limits); err != nil || replayed {
				return fmt.Errorf("seed foreign session %d: replay=%v err=%v", i, replayed, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertMCPHTTPEncryptedHistory(t, ctx, f, learner, 1056) // below the 2048 quota
}

// TestMCPHTTPPerformanceBudget is deliberately non-race in CI. The always-on
// identity/replay test covers correctness with race instrumentation separately.
func TestMCPHTTPPerformanceBudget(t *testing.T) {
	if os.Getenv("MCP_HTTP_PERF_BUDGET") != "1" {
		t.Skip("set MCP_HTTP_PERF_BUDGET=1 for the authenticated HTTP load gate")
	}
	configureMCPHTTPTestJWT(t)
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			f := newMCPHTTPFixture(t, backend)
			t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
			t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
			memory.ConfigureNarrativeStore(f.runtime)
			t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
			actors := append(f.seedTenant(t, "load-alpha", 25), f.seedTenant(t, "load-beta", 25)...)
			f.assertPostgresRLS(t, []models.Principal{actors[0], actors[25]})
			server := startMCPHTTPFixture(t, f)
			learners := make([]*mcpHTTPLoadLearner, len(actors))
			for i, actor := range actors {
				learners[i] = seedMCPHTTPLoadLearner(t, ctx, f, actor, i)
				learners[i].client = connectMCPHTTPActor(t, ctx, server, actor)
			}
			for _, learner := range learners[:4] {
				if _, err := runMCPHTTPLoadSequence(ctx, learner, "warmup"); err != nil {
					t.Fatal("warmup:", err)
				}
				assertMCPHTTPLoadDurable(t, ctx, f, learner)
			}
			type result struct {
				durations [4]time.Duration
				err       error
			}
			results := make([]result, len(learners))
			jobs, start := make(chan int), make(chan struct{})
			var workers sync.WaitGroup
			for range 4 {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					for i := range jobs {
						results[i].durations, results[i].err = runMCPHTTPLoadSequence(ctx, learners[i], "measured")
					}
				}()
			}
			before := f.runtime.RawDB().Stats()
			started := time.Now()
			close(start)
		feed:
			for i := range learners {
				select {
				case jobs <- i:
				case <-ctx.Done():
					break feed
				}
			}
			close(jobs)
			workers.Wait() // join before decoding summaries or closing anything
			elapsed := time.Since(started)
			after := f.runtime.RawDB().Stats()
			for i, result := range results {
				if result.err != nil {
					t.Errorf("learner %d: %v", i, result.err)
				}
			}
			calls := 0
			for i, tool := range mcpHTTPLoadTools {
				var samples []time.Duration
				for _, result := range results {
					if result.durations[i] > 0 {
						samples = append(samples, result.durations[i])
					}
				}
				calls += len(samples)
				if len(samples) != 50 {
					t.Errorf("%s samples=%d, want50", tool, len(samples))
					continue
				}
				p50, p95, p99 := mcpHTTPPercentile(samples, .5), mcpHTTPPercentile(samples, .95), mcpHTTPPercentile(samples, .99)
				t.Logf("HTTP tool=%s samples=50 p50=%s p95=%s p99=%s (nearest-rank p99=max)", tool, p50, p95, p99)
				if p95 >= 2*time.Second || p99 >= 5*time.Second {
					t.Errorf("%s exceeded initial HTTP budgets: p95=%s (<2s) p99=%s (<5s)", tool, p95, p99)
				}
			}
			pool := "writer"
			if f.postgres {
				pool = "shared"
			}
			t.Logf("HTTP backend=%s learners=50 workers=4 measured_calls=%d warmup_sequences=4 warmup_calls=16 seed_per_learner=12concepts/24interactions/32encrypted_sessions elapsed=%s observed_calls_per_second=%.2f pool=%s max_connections=%d wait_count_delta=%d wait_duration_delta=%s (SQLite reader waits are not sampled)", backend, calls, elapsed, float64(calls)/elapsed.Seconds(), pool, after.MaxOpenConnections, after.WaitCount-before.WaitCount, after.WaitDuration-before.WaitDuration)
			if calls != 200 {
				t.Errorf("measured calls=%d, want200 total", calls)
			}
			if t.Failed() {
				return
			}
			for _, learner := range learners {
				assertMCPHTTPLoadDurable(t, ctx, f, learner)
			}
			// This expensive shape is a separate observation, not hidden in
			// the main p95. All foreign sessions outrank the requested domain.
			large := learners[0]
			seedMCPHTTPForeignHistory(t, ctx, f, large)
			largeStarted := time.Now()
			_, payload, err := callMCPHTTPTool(ctx, large.client, "get_next_activity", map[string]any{"domain_id": large.domain.ID, "idempotency_key": "http-load:large:" + large.actor.LearnerID})
			largeDuration := time.Since(largeStarted)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkMCPHTTPLoadActivity(payload, large); err != nil {
				t.Fatal(err)
			}
			assertMCPHTTPLoadDurable(t, ctx, f, large)
			t.Logf("HTTP large_history samples=1 foreign_sessions=1024 requested_domain_sessions=32 latency=%s budget<5s (no percentile claim)", largeDuration)
			if largeDuration >= 5*time.Second {
				t.Fatal("large-history HTTP call exceeded its separate 5s budget")
			}
		})
	}
}
