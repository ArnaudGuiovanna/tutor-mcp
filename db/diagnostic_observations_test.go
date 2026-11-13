// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestListQualifiedDiagnosticObservationsInDomain(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	domain, err := store.CreateDomain(ctx, "L1", "triage", "", models.KnowledgeSpace{Concepts: []string{"a", "b", "c"}, Prerequisites: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	record := func(suffix, concept string, success bool, hints int, evaluate bool, at time.Time) {
		t.Helper()
		id := "diag_" + suffix
		if err := store.CreateAssessmentAttempt(ctx, &models.AssessmentAttempt{
			ID: id, LearnerID: "L1", DomainID: domain.ID, ConceptID: concept,
			ActivityID: "activity_" + suffix, ActivityVersion: 1,
			ActivityType: "DIAGNOSTIC_ASSESSMENT", Observable: "observable",
			TaskText: "task", TaskContentHash: "hash", RubricJSON: `{}`,
			PassingScore: 1, Status: models.AssessmentAttemptPrepared, CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.SubmitAssessmentAttempt(ctx, "L1", id, "response", "response-hash", at); err != nil {
			t.Fatal(err)
		}
		if evaluate {
			if err := store.CompleteAssessmentEvaluation(ctx, "L1", id, `{}`, "reviewer", models.EvaluationMethodHostLLM, `{}`, 1, success, at); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.CreateInteraction(ctx, &models.Interaction{
			LearnerID: "L1", DomainID: domain.ID, AssessmentAttemptID: id, Concept: concept,
			ActivityType: "DIAGNOSTIC_ASSESSMENT", Success: success, HintsRequested: hints, CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	record("old", "a", true, 0, true, base.Add(-2*time.Hour))
	record("pass", "a", true, 0, true, base.Add(time.Minute))
	record("fail", "b", false, 0, true, base.Add(2*time.Minute))
	record("hinted", "b", true, 1, true, base.Add(3*time.Minute))
	record("pending", "a", true, 0, false, base.Add(4*time.Minute))

	scope := models.LegacyPrincipal("L1").TenantScope()
	observations, err := store.ListQualifiedDiagnosticObservationsInDomain(ctx, scope, domain.ID, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 ||
		observations[0].Concept != "a" || !observations[0].Success || !observations[0].ObservedAt.Equal(base.Add(time.Minute)) ||
		observations[1].Concept != "b" || observations[1].Success {
		t.Fatalf("observations=%+v", observations)
	}
	concepts, err := store.GetQualifiedDiagnosticConceptsSinceInDomain(ctx, "L1", domain.ID, base)
	if err != nil || len(concepts) != 2 {
		t.Fatalf("observations disagree with the qualified concept list: %v %v", concepts, err)
	}
	if _, err := store.ListQualifiedDiagnosticObservationsInDomain(ctx, models.TenantScope{}, domain.ID, base); err == nil {
		t.Fatal("an unscoped read was accepted")
	}

	// Every interaction outside the qualified list marks its concept: the
	// diagnostic before the phase (a), the hinted one (b), the unevaluated one
	// (a). A concept with qualified answers only (c) is not marked.
	record("only-qualified", "c", true, 0, true, base.Add(5*time.Minute))
	foreign, err := store.ListConceptsWithNonPlacementEvidenceInDomain(ctx, scope, domain.ID, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 2 || foreign[0] != "a" || foreign[1] != "b" {
		t.Fatalf("concepts with non-placement evidence=%v, want [a b]", foreign)
	}
	if foreign, err := store.ListConceptsWithNonPlacementEvidenceInDomain(ctx, scope, domain.ID, base.Add(-3*time.Hour)); err != nil ||
		len(foreign) != 2 {
		t.Fatalf("an earlier phase start keeps hinted/unevaluated answers foreign: %v %v", foreign, err)
	}
	if _, err := store.ListConceptsWithNonPlacementEvidenceInDomain(ctx, models.TenantScope{}, domain.ID, base); err == nil {
		t.Fatal("an unscoped read was accepted")
	}
}
