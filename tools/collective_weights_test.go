// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Exercise the real observation pipeline: the collective weight must reach the
// learner inside the interaction, be traced, and apply once per weight version.
func TestCollectiveWeightsReachTheLearnerThroughRealInteractions(t *testing.T) {
	s, deps := setupToolsTest(t)
	deps.Institution = true
	ctx := t.Context()
	owner, err := s.GetPrincipalForLearner(ctx, "L_attacker", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembershipAuthorization(ctx, owner.TenantScope(), models.MembershipStatusActive, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMembershipMFAVerification(ctx, owner.TenantScope(), time.Now()); err != nil {
		t.Fatal(err)
	}
	owner, err = s.GetPrincipalForLearner(ctx, "L_attacker", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	_, v, err := s.CreateFormationDraft(ctx, owner, "Weights journey", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddFormationConceptsIdempotent(ctx, owner, "weights-content", v.ID, nil, []models.FormationConceptInput{{StableKey: "numbers", Label: "Numbers"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, v.ID); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateCohort(ctx, owner, v.ID, "Weights cohort", 5, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPrincipalForLearner(ctx, "L_owner", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.EnrollMembership(ctx, owner, c.ID, p.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	curriculum, err := s.GetCurriculumSnapshot(ctx, p.LearnerID, e.DomainID, 0)
	if err != nil {
		t.Fatal(err)
	}
	competency := curriculum.Concepts[0]
	raw, _ := json.Marshal(competency)
	insertWeight := func(version int, learn float64) {
		t.Helper()
		if _, err := s.RawDB().ExecContext(ctx, `INSERT INTO concept_collective_weights
 (tenant_id, formation_version_id, formation_concept_id, version, p_learn, p_forget, p_slip, p_guess, contributors, policy_version, computed_at)
 SELECT c.tenant_id, c.formation_version_id, c.id, ?, ?, .08, .2, .3, 30, ?, ?
 FROM formation_concepts c WHERE c.tenant_id = ? AND c.formation_version_id = ?`,
			version, learn, models.CollectiveWeightsPolicyVersion, time.Now().UTC(), owner.TenantID, v.ID); err != nil {
			t.Fatal(err)
		}
	}
	record := func(at time.Time) (*models.ConceptState, map[string]any) {
		t.Helper()
		session, err := s.OpenLearningSession(ctx, p.LearnerID, e.DomainID, "", at.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		id := "weights-journey-" + rand.Text()
		d := &models.PedagogicalDecision{ID: id, LearnerID: p.LearnerID, DomainID: e.DomainID, SessionID: session.ID, CurriculumVersion: curriculum.Version, PolicyVersion: models.PedagogicalPolicyVersion, CreatedAt: at.Add(-time.Minute), ContextJSON: "{}"}
		d.Contract = models.PedagogicalContract{DecisionID: id, PolicyVersion: d.PolicyVersion, CurriculumVersion: d.CurriculumVersion, TargetConcept: "numbers", RecommendedActivityType: models.ActivityPractice, Competency: &competency, LearningEventProtocol: models.LearningEventProtocol}
		if err := s.CreatePedagogicalDecision(ctx, p.TenantScope(), d); err != nil {
			t.Fatal(err)
		}
		a := &models.AssessmentAttempt{ID: "attempt-" + id, LearnerID: p.LearnerID, DomainID: e.DomainID, ConceptID: "numbers", SessionID: session.ID, ActivityID: id, ActivityVersion: 1, ActivityType: string(models.ActivityPractice), DecisionID: id, CurriculumVersion: curriculum.Version, CurriculumConceptJSON: string(raw), Observable: "Independent response", TaskText: "Name the larger number: 2 or 4", RubricJSON: `{"criteria":[{"id":"correct","description":"Correct response","max_score":1}],"passing_score":1}`, PassingScore: 1, CreatedAt: at.Add(-time.Minute)}
		if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitAssessmentAttempt(ctx, p.LearnerID, a.ID, "4", "", at); err != nil {
			t.Fatal(err)
		}
		var state *models.ConceptState
		var meta map[string]any
		err = s.WithTenantTx(ctx, p.TenantScope(), func(txCtx context.Context, _ storeport.Store) error {
			var applyErr error
			authCtx, applyErr := auth.WithPrincipal(txCtx, p)
			if applyErr != nil {
				return applyErr
			}
			state, meta, applyErr = applyInteraction(authCtx, deps, p.LearnerID, interactionInput{DomainID: e.DomainID, SessionID: session.ID, Concept: "numbers", ActivityType: string(models.ActivityPractice), AssessmentAttemptID: a.ID, Success: true, Confidence: .8, AssessmentScore: 1, EvaluatorID: "host", EvaluationMethod: models.EvaluationMethodHostLLM, RubricScoreJSON: `{"criteria_scores":[{"id":"correct","score":1,"evidence":"The response is correct."}]}`}, at.Add(time.Second))
			return applyErr
		})
		if err != nil {
			t.Fatal(err)
		}
		return state, meta
	}
	ledger := func() (version int, mode string, rows int) {
		t.Helper()
		if err := s.RawDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM collective_weight_applications WHERE tenant_id = ?`, owner.TenantID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows > 0 {
			if err := s.RawDB().QueryRowContext(ctx, `SELECT weight_version, mode FROM collective_weight_applications WHERE tenant_id = ?`, owner.TenantID).Scan(&version, &mode); err != nil {
				t.Fatal(err)
			}
		}
		return
	}

	start := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	_, meta := record(start) // no weight yet: nothing is applied or traced
	if _, ok := meta["bkt_collective_weights"]; ok {
		t.Fatal("no weight exists: nothing may be traced")
	}
	if _, _, rows := ledger(); rows != 0 {
		t.Fatalf("ledger without a weight: %d", rows)
	}
	insertWeight(1, .3)
	state, meta := record(start.Add(24 * time.Hour))
	trace, ok := meta["bkt_collective_weights"].(*models.CollectiveWeightApplication)
	if !ok || trace.Version != 1 || trace.Mode != models.CollectiveApplyBlend || trace.After.PGuess <= trace.Before.PGuess {
		t.Fatalf("the application must be traced in the observation: %+v", meta)
	}
	if version, mode, rows := ledger(); rows != 1 || version != 1 || mode != models.CollectiveApplyBlend {
		t.Fatalf("ledger: %d %s %d", version, mode, rows)
	}
	if state.Reps < 2 {
		t.Fatalf("the interaction itself must still be recorded: %+v", state)
	}
	_, meta = record(start.Add(30 * time.Hour))
	if _, ok := meta["bkt_collective_weights"]; ok {
		t.Fatal("a version must apply once")
	}
	insertWeight(2, .31)
	if _, meta = record(start.Add(36 * time.Hour)); meta["bkt_collective_weights"] == nil {
		t.Fatal("a new version must apply at the next interaction")
	}
}
