// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var errInjectedReadback = errors.New("injected post-commit readback failure")

type failGoalRelevanceReadbackStore struct {
	storeport.Store
	domainReads int
}

func (s *failGoalRelevanceReadbackStore) GetDomainByID(ctx context.Context, id string) (*models.Domain, error) {
	s.domainReads++
	if s.domainReads > 1 {
		return nil, errInjectedReadback
	}
	return s.Store.GetDomainByID(ctx, id)
}

type failHumanReviewReadbackStore struct{ storeport.Store }

func (s *failHumanReviewReadbackStore) HasHumanReviewedEvaluationInDomain(context.Context, string, string) (bool, error) {
	return false, errInjectedReadback
}

type failCurriculumReadbackStore struct{ storeport.Store }

func (s *failCurriculumReadbackStore) GetCurriculumSnapshot(context.Context, string, string, int) (*models.CurriculumSnapshot, error) {
	return nil, errInjectedReadback
}

type corruptAvailabilityResponseStore struct{ storeport.Store }

func (s *corruptAvailabilityResponseStore) UpdateAvailability(ctx context.Context, availability *models.Availability, expectedVersion int) (*models.Availability, error) {
	updated, err := s.Store.UpdateAvailability(ctx, availability, expectedVersion)
	if updated != nil {
		updated.WindowsJSON = `{not-json`
		updated.AccessibilityJSON = `{not-json`
	}
	return updated, err
}

type failMetacognitiveReadbackStore struct{ storeport.Store }

func (s *failMetacognitiveReadbackStore) GetInteractionsSinceInDomain(context.Context, string, string, time.Time) ([]*models.Interaction, error) {
	return nil, errInjectedReadback
}

type failSessionRecapReadStore struct{ storeport.Store }

func (s *failSessionRecapReadStore) GetInteractionsBySessionInDomain(context.Context, string, string, string) ([]*models.Interaction, error) {
	return nil, errors.New("injected pre-write recap failure")
}

type failNegotiationTradeoffReadStore struct{ storeport.Store }

func (s *failNegotiationTradeoffReadStore) GetConceptStateInDomain(context.Context, string, string, string) (*models.ConceptState, error) {
	return nil, errors.New("injected pre-write negotiation read failure")
}

func TestSetGoalRelevance_PostCommitReadbackFailureIsDegradedSuccess(t *testing.T) {
	t.Setenv("REGULATION_GOAL", "on")
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "atomic-goal")
	deps.Store = &failGoalRelevanceReadbackStore{Store: store}

	result := callTool(t, deps, registerSetGoalRelevance, "L_owner", "set_goal_relevance", map[string]any{
		"domain_id": domain.ID,
		"relevance": map[string]float64{"a": 0.9},
	})
	if result.IsError {
		t.Fatalf("committed merge must not become an MCP error: %q", resultText(result))
	}
	payload := decodeResult(t, result)
	assertDegradedComponent(t, payload, "updated_domain_readback")
	stored, err := store.GetDomainGoalRelevance(context.Background(), domain.ID)
	if err != nil || stored.Relevance["a"] != 0.9 {
		t.Fatalf("goal relevance was not committed: stored=%+v err=%v", stored, err)
	}
}

func TestMarkDomainHighStakes_PostCommitReadbackFailureFailsClosed(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "atomic-safety")
	deps.Store = &failHumanReviewReadbackStore{Store: store}

	result := callTool(t, deps, registerMarkDomainHighStakes, "L_owner", "mark_domain_high_stakes", map[string]any{
		"domain_id": domain.ID,
	})
	if result.IsError {
		t.Fatalf("committed safety classification must not become an MCP error: %q", resultText(result))
	}
	payload := decodeResult(t, result)
	assertDegradedComponent(t, payload, "human_review_evidence_readback")
	if payload["demonstrated_claims_allowed"] != false || payload["intrusive_notifications_allowed"] != false {
		t.Fatalf("degraded safety readback must fail closed: %v", payload)
	}
	stored, err := store.GetDomainByID(context.Background(), domain.ID)
	if err != nil || !stored.HighStakes {
		t.Fatalf("high-stakes classification was not committed: stored=%+v err=%v", stored, err)
	}
}

func TestPublishCurriculumRevision_PostCommitReadbackFailureIsDegradedSuccess(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "atomic-curriculum")
	baseline, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	deps.Store = &failCurriculumReadbackStore{Store: store}

	result := callTool(t, deps, registerReviseCurriculum, "L_owner", "publish_curriculum_revision", map[string]any{
		"domain_id":          domain.ID,
		"expected_version":   1,
		"operation":          "rename",
		"source_concept_ids": []string{baseline.Concepts[0].ID},
		"new_label":          "renamed competency",
		"provenance": map[string]any{
			"source_type": "learner",
			"rationale":   "use a clearer competency label",
		},
	})
	if result.IsError {
		t.Fatalf("committed curriculum CAS must not become an MCP error: %q", resultText(result))
	}
	payload := decodeResult(t, result)
	assertDegradedComponent(t, payload, "curriculum_snapshot_readback")
	if payload["version"] != float64(2) || payload["snapshot_status"] != "committed_readback_unavailable" {
		t.Fatalf("missing known commit metadata: %v", payload)
	}
	published, err := store.GetCurriculumSnapshot(context.Background(), "L_owner", domain.ID, 2)
	if err != nil || published.Concepts[0].Label != "renamed competency" {
		t.Fatalf("curriculum revision was not committed: published=%+v err=%v", published, err)
	}
}

func TestUpdateAvailability_DecodesPresentationBeforeCommit(t *testing.T) {
	store, deps := setupToolsTest(t)
	deps.Store = &corruptAvailabilityResponseStore{Store: store}

	result := callTool(t, deps, registerUpdateAvailabilityModel, "L_owner", "update_availability_model", map[string]any{
		"expected_version":             0,
		"timezone":                     "UTC",
		"preferred_windows":            []map[string]any{{"day": "monday", "start": "09:00", "end": "10:00"}},
		"avg_session_duration_minutes": 30,
		"sessions_per_week":            3,
		"do_not_disturb":               false,
		"notification_consent":         true,
		"notification_frequency":       "daily",
		"max_notifications_per_day":    1,
		"accessibility_preferences":    map[string]any{"avoid_emojis": true},
	})
	if result.IsError {
		t.Fatalf("post-commit presentation must use prevalidated values: %q", resultText(result))
	}
	payload := decodeResult(t, result)
	if payload["version"] != float64(1) {
		t.Fatalf("unexpected committed availability response: %v", payload)
	}
	windows, ok := payload["preferred_windows"].([]any)
	if !ok || len(windows) != 1 {
		t.Fatalf("prevalidated windows missing from response: %v", payload)
	}
	stored, err := store.GetAvailability(context.Background(), "L_owner")
	if err != nil || stored.Version != 1 {
		t.Fatalf("availability was not committed: stored=%+v err=%v", stored, err)
	}
}

func TestUpdateLearnerMemory_PostRenameSyncFailureIsDegradedSuccess(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, deps := setupToolsTest(t)
	originalWrite := writeLearnerMemory
	writeLearnerMemory = func(memory.WriteRequest) error {
		return &memory.CommittedWriteError{Err: errors.New("injected directory sync failure")}
	}
	t.Cleanup(func() { writeLearnerMemory = originalWrite })

	result := callTool(t, deps, registerUpdateLearnerMemory, "L_owner", "update_learner_memory", map[string]any{
		"scope":     "memory_pending",
		"operation": "append",
		"content":   "- one committed observation",
	})
	if result.IsError {
		t.Fatalf("visible post-rename write must not be retried: %q", resultText(result))
	}
	payload := decodeResult(t, result)
	assertDegradedComponent(t, payload, "memory_directory_sync")
}

func TestGetNextActivity_PostMutationEnrichmentFailureIsDegradedSuccess(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "atomic-activity")
	deps.Store = &failMetacognitiveReadbackStore{Store: store}

	result := callTool(t, deps, registerGetNextActivity, "L_owner", "get_next_activity", map[string]any{
		"domain_id": domain.ID,
	})
	if result.IsError {
		t.Fatalf("post-routing enrichment must not abort the activity: %q", resultText(result))
	}
	payload := decodeResult(t, result)
	assertDegradedComponent(t, payload, "metacognitive_interactions")
	if payload["session_id"] == "" || payload["activity"] == nil {
		t.Fatalf("durable routing result was lost: %v", payload)
	}
}

func TestRecordSessionClose_RecapFailurePrecedesIntentionAndCloseWrites(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "atomic-close")
	session := openTestLearningSession(t, store, "L_owner", "session-atomic-close", domain.ID)
	deps.Store = &failSessionRecapReadStore{Store: store}

	result := callTool(t, deps, registerRecordSessionClose, "L_owner", "record_session_close", map[string]any{
		"session_id": session.ID,
		"domain_id":  domain.ID,
		"implementation_intention": map[string]any{
			"trigger": "tomorrow after coffee",
			"action":  "practice concept a",
		},
	})
	if !result.IsError {
		t.Fatalf("expected injected pre-write recap failure, got %q", resultText(result))
	}
	intentions, err := store.GetRecentImplementationIntentions(context.Background(), "L_owner", time.Now().UTC().Add(-time.Hour), 10)
	if err != nil || len(intentions) != 0 {
		t.Fatalf("intention was written before recap succeeded: intentions=%+v err=%v", intentions, err)
	}
	storedSession, err := store.GetLearningSession(context.Background(), "L_owner", session.ID)
	if err != nil || storedSession.Status != models.LearningSessionStatusOpen {
		t.Fatalf("session closed before recap succeeded: session=%+v err=%v", storedSession, err)
	}
}

func TestLearningNegotiation_FailedPreviewDoesNotPersistProjectedPhase(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "atomic-negotiation")
	if err := store.UpdateDomainPhase(context.Background(), domain.ID, models.PhaseMaintenance, 0, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	deps.Store = &failNegotiationTradeoffReadStore{Store: store}

	result := callTool(t, deps, registerLearningNegotiation, "L_owner", "learning_negotiation", map[string]any{
		"session_id":      "preview-session",
		"domain_id":       domain.ID,
		"learner_concept": "b",
	})
	if !result.IsError {
		t.Fatalf("expected injected pre-write negotiation error, got %q", resultText(result))
	}
	stored, err := store.GetDomainByID(context.Background(), domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != models.PhaseMaintenance {
		t.Fatalf("negotiation preview persisted projected phase %q; want %q", stored.Phase, models.PhaseMaintenance)
	}
}

func assertDegradedComponent(t *testing.T, payload map[string]any, want string) {
	t.Helper()
	components, ok := payload["degraded_components"].([]any)
	if !ok {
		t.Fatalf("degraded_components missing from %v", payload)
	}
	for _, component := range components {
		if component == want {
			return
		}
	}
	t.Fatalf("degraded component %q missing from %v", want, components)
}
