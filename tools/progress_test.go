// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
	"testing"
	"time"
	"tutor-mcp/auth"
	"tutor-mcp/models"
)

func TestInstitutionProgressMCPJourney(t *testing.T) {
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
	_, v, err := s.CreateFormationDraft(ctx, owner, "Progress MCP", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddFormationConceptsIdempotent(ctx, owner, "progress-content", v.ID, nil, []models.FormationConceptInput{{StableKey: "numbers", Label: "Numbers"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, v.ID); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateCohort(ctx, owner, v.ID, "Cohort", 5, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	student, err := s.GetPrincipalForLearner(ctx, "L_owner", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.EnrollMembership(ctx, owner, c.ID, student.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	call := func(p models.Principal, name string, args any) *mcp.CallToolResult {
		t.Helper()
		return callTool(t, deps, func(server *mcp.Server, deps *Deps) {
			RegisterTools(server, deps)
			server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					ctx, err := auth.WithPrincipal(ctx, p)
					if err != nil {
						return nil, err
					}
					return next(auth.WithOAuthScope(ctx, strings.Join(p.Scopes, " ")), method, req)
				}
			})
		}, "", name, args)
	}
	reader := owner
	reader.Scopes = []string{models.OAuthScopeProgressRead}
	for _, tc := range []struct {
		p    models.Principal
		name string
	}{{student, "get_my_badges"}, {reader, "get_learner_badges"}} {
		r := call(tc.p, tc.name, LearnerProgressParams{EnrollmentID: e.ID})
		var badges models.BadgePage
		if r.IsError || json.Unmarshal([]byte(resultText(r)), &badges) != nil || len(badges.Items) != 0 {
			t.Fatalf("badge discovery: %s", resultText(r))
		}
	}
	if r := call(owner, "get_my_badges", LearnerProgressParams{EnrollmentID: e.ID}); !r.IsError {
		t.Fatal("other learner badge read")
	}
	if r := call(owner, "get_learner_badges", LearnerProgressParams{EnrollmentID: e.ID}); !r.IsError || !strings.Contains(resultText(r), "progress:read") {
		t.Fatal("badge scope bypass")
	}
	var page models.TrainerCohortPage
	r := call(reader, "list_trainer_cohorts", LearnerFormationPageParams{})
	if r.IsError || json.Unmarshal([]byte(resultText(r)), &page) != nil || len(page.Items) != 1 || page.Items[0].CohortID != c.ID {
		t.Fatalf("discover: %s", resultText(r))
	}
	var cohort models.CohortInsights
	r = call(reader, "get_cohort_insights", CohortInsightsParams{CohortID: c.ID})
	if r.IsError || json.Unmarshal([]byte(resultText(r)), &cohort) != nil || len(cohort.Learners) != 1 || cohort.Learners[0].EnrollmentID != e.ID || cohort.SynthesisGuidance == "" {
		t.Fatalf("cohort: %s", resultText(r))
	}
	var progress models.LearnerProgress
	r = call(reader, "get_learner_progress", LearnerProgressParams{EnrollmentID: e.ID})
	if r.IsError || json.Unmarshal([]byte(resultText(r)), &progress) != nil || progress.Learner.ConceptCount != 1 || progress.Learner.AverageMastery != nil {
		t.Fatalf("learner: %s", resultText(r))
	}
	for _, tc := range []struct {
		p    models.Principal
		args LearnerProgressParams
		code string
	}{
		{reader, LearnerProgressParams{EnrollmentID: "missing"}, "not_found"},
		{reader, LearnerProgressParams{EnrollmentID: e.ID, Limit: 101}, "invalid_request"},
		{models.Principal{UserID: student.UserID, TenantID: student.TenantID, MembershipID: student.MembershipID, LearnerID: student.LearnerID, Roles: student.Roles, TokenVersion: student.TokenVersion, Scopes: []string{models.OAuthScopeProgressRead}}, LearnerProgressParams{EnrollmentID: e.ID}, "forbidden"},
	} {
		r := call(tc.p, "get_learner_progress", tc.args)
		if !r.IsError || !strings.Contains(resultText(r), `"code":"`+tc.code+`"`) && !strings.Contains(resultText(r), `"code": "`+tc.code+`"`) {
			t.Fatalf("normalized %s: %s", tc.code, resultText(r))
		}
	}
	var stats models.CohortStatistics
	r = call(reader, "get_cohort_statistics", CohortStatisticsParams{CohortID: c.ID})
	if r.IsError || json.Unmarshal([]byte(resultText(r)), &stats) != nil || stats.Status != models.StatisticsNotYetComputed || stats.SynthesisGuidance == "" {
		t.Fatalf("statistics before computation: %s", resultText(r))
	}
	if _, err := s.RecomputeInstitutionStatistics(ctx, owner.TenantScope(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stats = models.CohortStatistics{}
	r = call(reader, "get_cohort_statistics", CohortStatisticsParams{CohortID: c.ID})
	if r.IsError || json.Unmarshal([]byte(resultText(r)), &stats) != nil || stats.Status != models.StatisticsInsufficientData ||
		stats.ComputedAt == nil || stats.Contributors.Value != nil || stats.Contributors.Status != models.StatisticsInsufficientData {
		t.Fatalf("one learner must never produce a figure: %s", resultText(r))
	}
	for _, tc := range []struct {
		p    models.Principal
		args CohortStatisticsParams
		code string
	}{
		{reader, CohortStatisticsParams{CohortID: "missing"}, "not_found"},
		{reader, CohortStatisticsParams{CohortID: c.ID, Limit: 101}, "invalid_request"},
		{models.Principal{UserID: student.UserID, TenantID: student.TenantID, MembershipID: student.MembershipID, LearnerID: student.LearnerID, Roles: student.Roles, TokenVersion: student.TokenVersion, Scopes: []string{models.OAuthScopeProgressRead}}, CohortStatisticsParams{CohortID: c.ID}, "forbidden"},
	} {
		r := call(tc.p, "get_cohort_statistics", tc.args)
		if !r.IsError || !strings.Contains(resultText(r), `"code":"`+tc.code+`"`) && !strings.Contains(resultText(r), `"code": "`+tc.code+`"`) {
			t.Fatalf("statistics %s: %s", tc.code, resultText(r))
		}
	}
	if r := call(owner, "get_cohort_statistics", CohortStatisticsParams{CohortID: c.ID}); !r.IsError || !strings.Contains(resultText(r), "progress:read") {
		t.Fatalf("statistics scope bypass: %s", resultText(r))
	}
	if r := call(owner, "list_trainer_cohorts", LearnerFormationPageParams{}); !r.IsError || !strings.Contains(resultText(r), "progress:read") {
		t.Fatalf("missing scope step-up: %s", resultText(r))
	}
}
