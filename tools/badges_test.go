// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Exercise the real observation pipeline and its FSRS schedule, rather than
// constructing the JSON snapshots that the badge reader consumes.
func TestInstitutionBadgesRealInteractionAndFSRS(t *testing.T) {
	s, deps := setupToolsTest(t)
	ctx := t.Context()
	deps.Institution = true
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
	_, v, err := s.CreateFormationDraft(ctx, owner, "Badge journey", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddFormationConceptsIdempotent(ctx, owner, "badge-content", v.ID, nil, []models.FormationConceptInput{{StableKey: "numbers", Label: "Numbers"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, v.ID); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateCohort(ctx, owner, v.ID, "Badge cohort", 5, nil, nil)
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
	start := time.Now().UTC().Add(-700 * 24 * time.Hour).Truncate(time.Second)
	record := func(activity models.ActivityType, at time.Time) *models.ConceptState {
		t.Helper()
		session, err := s.OpenLearningSession(ctx, p.LearnerID, e.DomainID, "", at.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		id := "badge-journey-" + rand.Text()
		d := &models.PedagogicalDecision{ID: id, LearnerID: p.LearnerID, DomainID: e.DomainID, SessionID: session.ID, CurriculumVersion: curriculum.Version, PolicyVersion: models.PedagogicalPolicyVersion, CreatedAt: at.Add(-time.Minute), ContextJSON: "{}"}
		d.Contract = models.PedagogicalContract{DecisionID: id, PolicyVersion: d.PolicyVersion, CurriculumVersion: d.CurriculumVersion, TargetConcept: "numbers", RecommendedActivityType: activity, Competency: &competency, LearningEventProtocol: models.LearningEventProtocol}
		if err := s.CreatePedagogicalDecision(ctx, p.TenantScope(), d); err != nil {
			t.Fatal(err)
		}
		a := &models.AssessmentAttempt{ID: "attempt-" + id, LearnerID: p.LearnerID, DomainID: e.DomainID, ConceptID: "numbers", SessionID: session.ID, ActivityID: id, ActivityVersion: 1, ActivityType: string(activity), DecisionID: id, CurriculumVersion: curriculum.Version, CurriculumConceptJSON: string(raw), Observable: "Independent response", TaskText: "Name the larger number: 2 or 4", RubricJSON: `{"criteria":[{"id":"correct","description":"Correct response","max_score":1}],"passing_score":1}`, PassingScore: 1, CreatedAt: at.Add(-time.Minute)}
		if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitAssessmentAttempt(ctx, p.LearnerID, a.ID, "4", "", at); err != nil {
			t.Fatal(err)
		}
		var state *models.ConceptState
		err = s.WithTenantTx(ctx, p.TenantScope(), func(txCtx context.Context, _ storeport.Store) error {
			var applyErr error
			state, _, applyErr = applyInteraction(txCtx, deps, p.LearnerID, interactionInput{DomainID: e.DomainID, SessionID: session.ID, Concept: "numbers", ActivityType: string(activity), AssessmentAttemptID: a.ID, Success: true, Confidence: .8, AssessmentScore: 1, EvaluatorID: "host", EvaluationMethod: models.EvaluationMethodHostLLM, RubricScoreJSON: `{"criteria_scores":[{"id":"correct","score":1,"evidence":"The response is correct."}]}`}, at.Add(time.Second))
			return applyErr
		})
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	state := record(models.ActivityMasteryChallenge, start)
	page, err := s.GetEnrollmentBadges(ctx, p, e.ID, "", 10, false)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("observation did not award mastery/completion: %+v %v", page, err)
	}
	for count := 0; count < 15 && state.LastReview.Sub(start) < 90*24*time.Hour; count++ {
		at := *state.NextReview
		if at.Sub(*state.LastReview) < 24*time.Hour {
			at = state.LastReview.Add(24 * time.Hour)
		}
		state = record(models.ActivityRecall, at)
	}
	page, err = s.GetEnrollmentBadges(ctx, p, e.ID, "", 10, false)
	if err != nil || len(page.Items) != 5 {
		t.Fatalf("real FSRS did not award all milestones: %+v %v", page, err)
	}
	for _, b := range page.Items {
		if b.EvidenceStatus != "valid" {
			t.Fatalf("real proof invalid: %+v", b)
		}
	}
}
