package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// setupTestDB exercises these migrations and store contracts on SQLite by
// default and on an isolated PostgreSQL schema with TUTOR_TEST_PG_DSN.
func TestPedagogicalDecisionCreateGetIsolationAndImmutability(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := newPedagogicalDecisionFixture(t, s, "L1", "owner")
	other := newPedagogicalDecisionFixture(t, s, "L2", "other")
	got, err := s.GetPedagogicalDecision(ctx, owner.scope, owner.decision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Contract.Competency == nil || got.Contract.Competency.ID != owner.decision.Contract.Competency.ID ||
		got.PolicyVersion != owner.decision.PolicyVersion || got.CurriculumVersion != owner.curriculum.Version ||
		got.ContextJSON != owner.decision.ContextJSON {
		t.Fatalf("frozen decision lost its curriculum or runtime context: %+v", got)
	}
	if err := s.WithTenantTx(ctx, owner.scope, func(txCtx context.Context, scoped storeport.Store) error {
		if _, err := scoped.GetPedagogicalDecision(txCtx, owner.scope, owner.decision.ID); err != nil {
			return err
		}
		next := *owner.decision
		next.ID = "scoped-decision"
		next.Contract.DecisionID = next.ID
		if err := scoped.CreatePedagogicalDecision(txCtx, owner.scope, &next); err != nil {
			return err
		}
		if _, err := scoped.GetPedagogicalDecision(txCtx, other.scope, other.decision.ID); err == nil {
			return fmt.Errorf("scoped store accepted a different tenant principal")
		}
		return nil
	}); err != nil {
		t.Fatalf("compose scoped decision methods: %v", err)
	}
	if _, err := s.GetPedagogicalDecision(ctx, other.scope, owner.decision.ID); err == nil {
		t.Fatal("another learner read the decision")
	}
	foreignScope := owner.scope
	foreignScope.TenantID = "tenant-not-owner"
	if _, err := s.GetPedagogicalDecision(ctx, foreignScope, owner.decision.ID); err == nil {
		t.Fatal("another tenant read the decision")
	}
	if err := s.CreatePedagogicalDecision(ctx, other.scope, owner.decision); err == nil {
		t.Fatal("another learner rewrote the decision")
	}
	if err := s.CreatePedagogicalDecision(ctx, owner.scope, owner.decision); err == nil {
		t.Fatal("duplicate create overwrote an immutable decision")
	}
	if _, err := s.root.ExecContext(ctx, rb(s, `UPDATE pedagogical_decisions SET snapshot_json = '{}' WHERE id = ?`), owner.decision.ID); err == nil {
		t.Fatal("migration did not reject direct mutation of a decision")
	}
	// Mutating a caller-owned read model must not mutate the stored envelope.
	got.Contract.TargetConcept = "changed"
	unchanged, err := s.GetPedagogicalDecision(ctx, owner.scope, owner.decision.ID)
	if err != nil || unchanged.Contract.TargetConcept != "a" {
		t.Fatalf("decision changed after rejected writes: got=%+v err=%v", unchanged, err)
	}
	if _, err := s.root.ExecContext(ctx, rb(s, `INSERT INTO pedagogical_decisions
		(id, tenant_id, learner_id, domain_id, session_id, curriculum_version, snapshot_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), "foreign-session", owner.scope.TenantID, "L1", owner.domain.ID,
		other.decision.SessionID, owner.curriculum.Version, "{}", time.Now().UTC()); err == nil {
		t.Fatal("migration accepted a decision bound to another learner's session")
	}
}

func TestPedagogicalDecisionBoundAttemptScopeAndImmutableBinding(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, "L1", "owner")
	other := newPedagogicalDecisionFixture(t, s, "L2", "other")
	otherDomain, err := s.CreateDomain(ctx, "L1", "Other domain", "", models.KnowledgeSpace{Concepts: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*models.AssessmentAttempt)
	}{
		{name: "wrong learner", mutate: func(a *models.AssessmentAttempt) { a.LearnerID = "L2" }},
		{name: "wrong domain", mutate: func(a *models.AssessmentAttempt) { a.DomainID = otherDomain.ID }},
		{name: "wrong session", mutate: func(a *models.AssessmentAttempt) { a.SessionID = other.decision.SessionID }},
		{name: "wrong concept", mutate: func(a *models.AssessmentAttempt) { a.ConceptID = "b" }},
		{name: "wrong activity", mutate: func(a *models.AssessmentAttempt) { a.ActivityType = string(models.ActivityRecall) }},
		{name: "wrong version", mutate: func(a *models.AssessmentAttempt) { a.CurriculumVersion++ }},
		{name: "rewritten competency", mutate: func(a *models.AssessmentAttempt) { a.CurriculumConceptJSON = `{}` }},
		{name: "foreign decision", mutate: func(a *models.AssessmentAttempt) { a.DecisionID = other.decision.ID }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := f.attempt(t, "invalid-"+tc.name)
			tc.mutate(attempt)
			if err := s.CreateAssessmentAttempt(ctx, attempt); err == nil {
				t.Fatal("bound assessment with a mismatched contract was accepted")
			}
		})
	}
	attempt := f.attempt(t, "valid-bound-attempt")
	if err := s.CreateAssessmentAttempt(ctx, attempt); err != nil {
		t.Fatalf("valid bound assessment: %v", err)
	}
	for _, query := range []string{
		`UPDATE assessment_attempts SET decision_id = NULL WHERE id = ?`,
		`UPDATE assessment_attempts SET curriculum_version = curriculum_version + 1 WHERE id = ?`,
		`UPDATE assessment_attempts SET curriculum_concept_json = '{}' WHERE id = ?`,
		`UPDATE assessment_attempts SET outcome_ids_json = '["different"]' WHERE id = ?`,
		`UPDATE assessment_attempts SET concept_id = 'b' WHERE id = ?`,
		`UPDATE assessment_attempts SET activity_type = 'TRANSFER_PROBE' WHERE id = ?`,
		`UPDATE assessment_attempts SET session_id = NULL WHERE id = ?`,
	} {
		if _, err := s.root.ExecContext(ctx, rb(s, query), attempt.ID); err == nil {
			t.Fatalf("migration allowed a bound assessment to be rewritten: %s", query)
		}
	}
	now := time.Now().UTC()
	if err := s.SubmitAssessmentAttempt(ctx, "L1", attempt.ID, "The learner's response", "response-hash", now); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAssessmentEvaluation(ctx, "L1", attempt.ID,
		`{"criteria_scores":[{"id":"correctness","score":1,"evidence":"Correct response."}]}`,
		"host-test", models.EvaluationMethodHostLLM, `{}`, 1, true, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAssessmentAttempt(ctx, "L1", attempt.ID)
	if err != nil || got.DecisionID != attempt.DecisionID || got.CurriculumConceptJSON != attempt.CurriculumConceptJSON ||
		got.CurriculumVersion != attempt.CurriculumVersion || got.OutcomeIDsJSON != "[]" || got.TrustedEvaluation {
		t.Fatalf("evaluation altered binding or minted trust: got=%+v err=%v", got, err)
	}
	evaluated, err := s.GetEvaluatedAssessmentAttemptsInDomain(ctx, "L1", f.domain.ID, "a", 10)
	if err != nil || len(evaluated) != 1 || evaluated[0].DecisionID != attempt.DecisionID {
		t.Fatalf("single-concept evidence reader lost the binding: got=%+v err=%v", evaluated, err)
	}
	batch, err := s.GetEvaluatedAssessmentAttemptsBatchInDomain(ctx, "L1", f.domain.ID, []string{"a"}, 10)
	if err != nil || len(batch["a"]) != 1 || batch["a"][0].CurriculumConceptJSON != attempt.CurriculumConceptJSON {
		t.Fatalf("batched evidence reader lost the binding: got=%+v err=%v", batch, err)
	}
}

func TestPedagogicalDecisionBoundAttemptSingleUse(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, "L1", "single-use")
	attempts := []*models.AssessmentAttempt{f.attempt(t, "attempt-one"), f.attempt(t, "attempt-two")}
	errors := make(chan error, len(attempts))
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, attempt := range attempts {
		workers.Add(1)
		go func(a *models.AssessmentAttempt) {
			defer workers.Done()
			<-start
			errors <- s.CreateAssessmentAttempt(ctx, a)
		}(attempt)
	}
	close(start)
	workers.Wait()
	close(errors)
	successes := 0
	for err := range errors {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent generated tasks consumed one decision %d times, want exactly once", successes)
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM assessment_attempts WHERE decision_id = ?`, f.decision.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("persisted bound attempts=%d want 1: %v", count, err)
	}
}

func TestPedagogicalDecisionRejectsStaleCurriculum(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	f := newPedagogicalDecisionFixture(t, s, "L1", "stale")
	next := models.CloneCurriculumSnapshot(f.curriculum)
	next.Concepts[0].Label = "Updated label"
	next.Operation = models.CurriculumOperation{Type: models.CurriculumOperationRename, Rationale: "clarify the competency label"}
	next.Provenance = models.CurriculumProvenance{SourceType: "test", Author: "L1", Rationale: "validate stale decision rejection"}
	if err := s.CompareAndSwapCurriculum(ctx, "L1", f.domain.ID, f.curriculum.Version, next); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAssessmentAttempt(ctx, f.attempt(t, "stale-attempt")); err == nil {
		t.Fatal("an old decision prepared an assessment after the curriculum changed")
	}
	stale := *f.decision
	stale.ID = "another-stale-decision"
	stale.Contract.DecisionID = stale.ID
	if err := s.CreatePedagogicalDecision(ctx, f.scope, &stale); err == nil {
		t.Fatal("new decision accepted a stale curriculum snapshot")
	}
	if _, err := s.GetPedagogicalDecision(ctx, f.scope, f.decision.ID); err != nil {
		t.Fatalf("historical decision became unreadable after curriculum revision: %v", err)
	}
}

type pedagogicalDecisionFixture struct {
	scope      models.TenantScope
	domain     *models.Domain
	curriculum *models.CurriculumSnapshot
	decision   *models.PedagogicalDecision
}

func newPedagogicalDecisionFixture(t *testing.T, s *Store, learnerID, suffix string) pedagogicalDecisionFixture {
	t.Helper()
	ctx := context.Background()
	seedLearner(t, s, learnerID)
	domain, err := s.CreateDomain(ctx, learnerID, "Curriculum "+suffix, "", models.KnowledgeSpace{Concepts: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	curriculum, err := s.EnsureCurriculumBaseline(ctx, learnerID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.OpenLearningSession(ctx, learnerID, domain.ID, "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	scope := models.LegacyPrincipal(learnerID).TenantScope()
	var competency *models.CurriculumConcept
	for i := range curriculum.Concepts {
		if curriculum.Concepts[i].Key == "a" {
			competency = &curriculum.Concepts[i]
		}
	}
	if competency == nil {
		t.Fatal("fixture curriculum has no target competency")
	}
	id := "decision-" + suffix
	decision := &models.PedagogicalDecision{
		ID: id, LearnerID: learnerID, DomainID: domain.ID, SessionID: session.ID,
		CurriculumVersion: curriculum.Version, PolicyVersion: models.PedagogicalPolicyVersion,
		ContextJSON: `{"phase":"instruction","evidence":"developing"}`, CreatedAt: time.Now().UTC(),
		Contract: models.PedagogicalContract{
			DecisionID: id, PolicyVersion: models.PedagogicalPolicyVersion, CurriculumVersion: curriculum.Version,
			TargetConcept: "a", RecommendedActivityType: models.ActivityPractice, Competency: competency,
		},
	}
	if err := s.CreatePedagogicalDecision(ctx, scope, decision); err != nil {
		t.Fatal(err)
	}
	return pedagogicalDecisionFixture{scope: scope, domain: domain, curriculum: curriculum, decision: decision}
}

func (f pedagogicalDecisionFixture) attempt(t *testing.T, id string) *models.AssessmentAttempt {
	t.Helper()
	competency, err := json.Marshal(f.decision.Contract.Competency)
	if err != nil {
		t.Fatal(err)
	}
	return &models.AssessmentAttempt{
		ID: id, LearnerID: f.decision.LearnerID, DomainID: f.domain.ID, ConceptID: "a", SessionID: f.decision.SessionID,
		ActivityID: fmt.Sprintf("activity-%s", id), ActivityVersion: 1, ActivityType: string(models.ActivityPractice),
		DecisionID: f.decision.ID, CurriculumVersion: f.curriculum.Version, CurriculumConceptJSON: string(competency), OutcomeIDsJSON: "[]",
		Observable: "Apply the competency independently.", TaskText: "Produce the requested response.", TaskContentHash: "task-hash",
		RubricJSON:   `{"criteria":[{"id":"correctness","description":"A correct response.","max_score":1}],"passing_score":1}`,
		PassingScore: 1, Status: models.AssessmentAttemptPrepared, CreatedAt: time.Now().UTC(),
	}
}
