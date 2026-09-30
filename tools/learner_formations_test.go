// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestLearnerFormationMCPJourney(t *testing.T) {
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
	f, v, err := s.CreateFormationDraft(ctx, owner, "MCP learner course", "A shared course")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddFormationConceptsIdempotent(ctx, owner, "content", v.ID, nil, []models.FormationConceptInput{{StableKey: "addition", Label: "Addition"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFormationEnrollmentPolicy(ctx, owner, f.ID, "open"); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateCohort(ctx, owner, v.ID, "First cohort", 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args any, target any) {
		t.Helper()
		r := callTool(t, deps, RegisterTools, "L_owner", name, args)
		if r.IsError {
			t.Fatalf("%s: %s", name, resultText(r))
		}
		if target != nil {
			if err := json.Unmarshal([]byte(resultText(r)), target); err != nil {
				t.Fatal(err)
			}
		}
	}
	var page models.LearnerFormationPage
	var opening map[string]any
	call("get_learner_context", map[string]any{}, &opening)
	if instruction, _ := opening["next_action_for_llm"].(string); !strings.Contains(instruction, "join_formation") {
		t.Fatalf("institution setup guidance: %+v", opening)
	}
	call("list_available_formations", LearnerFormationPageParams{}, &page)
	if len(page.Items) != 1 || page.Items[0].CohortID != c.ID {
		t.Fatalf("catalog: %+v", page)
	}
	var detail models.LearnerFormationDetail
	call("get_learner_formation", LearnerFormationParams{CohortID: c.ID}, &detail)
	if len(detail.Concepts) != 1 {
		t.Fatal("missing program")
	}
	var join models.FormationJoinResult
	call("join_formation", LearnerFormationMutationParams{CohortID: c.ID, IdempotencyKey: "mcp-join"}, &join)
	if join.Status != "active" || join.Enrollment.DomainID == "" {
		t.Fatalf("join: %+v", join)
	}
	call("leave_formation", LearnerFormationMutationParams{CohortID: c.ID, IdempotencyKey: "mcp-leave"}, nil)
	call("get_my_formations", LearnerFormationPageParams{}, &page)
	if len(page.Items) != 1 || page.Items[0].Enrollment.Status != "cancelled" {
		t.Fatalf("left: %+v", page)
	}
	call("join_formation", LearnerFormationMutationParams{CohortID: c.ID, IdempotencyKey: "mcp-rejoin"}, nil)
	call("leave_formation", LearnerFormationMutationParams{CohortID: c.ID, IdempotencyKey: "mcp-leave"}, nil)
	call("get_my_formations", LearnerFormationPageParams{}, &page)
	if page.Items[0].Enrollment.Status != "active" || page.Items[0].Enrollment.DomainID != join.Enrollment.DomainID {
		t.Fatal("retry undid rejoin")
	}
}
