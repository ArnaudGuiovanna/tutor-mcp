// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package tools

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"slices"
	"strings"
	"testing"
	"tutor-mcp/auth"
	"tutor-mcp/models"
)

func TestNativeToolDiscoveryMatchesRoleAndDelegatedActivity(t *testing.T) {
	available := []*mcp.Tool{}
	for _, name := range []string{"list_formations", "list_formation_versions", "review_formation_version", "publish_formation_to_catalog", "list_trainer_cohorts", "get_learner_context", "join_formation"} {
		available = append(available, &mcp.Tool{Name: name})
	}
	for _, test := range []struct {
		name, scope, learner string
		roles                []string
		want                 []string
	}{
		{"owner read", "formation:read", "", []string{models.RoleOwner}, []string{"list_formations", "list_formation_versions", "review_formation_version"}},
		{"owner author", "formation:read formation:write", "", []string{models.RoleOwner}, []string{"list_formations", "list_formation_versions", "review_formation_version", "publish_formation_to_catalog"}},
		{"trainer", "progress:read", "", []string{models.RoleTrainer}, []string{"list_trainer_cohorts"}},
		{"learner", "learner:read learner:write", "learner", []string{models.RoleLearner}, []string{"get_learner_context", "join_formation"}},
		{"learner forged scope", "formation:read formation:write", "learner", []string{models.RoleLearner}, []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, err := auth.WithPrincipal(context.Background(), models.Principal{TenantID: "tenant", UserID: "user", MembershipID: "member", LearnerID: test.learner, Roles: test.roles, Scopes: strings.Fields(test.scope), TokenVersion: 1})
			if err != nil {
				t.Fatal(err)
			}
			ctx = auth.WithOAuthScope(ctx, test.scope)
			var actual []string
			for _, tool := range authorizedNativeTools(ctx, available) {
				actual = append(actual, tool.Name)
			}
			if !slices.Equal(actual, test.want) {
				t.Fatalf("visible=%v want=%v", actual, test.want)
			}
		})
	}
}

func TestNativeContributionToolsWorkWithoutEnablingPrivateLearning(t *testing.T) {
	p := models.Principal{TenantID: "tenant", UserID: "author", MembershipID: "member", Roles: []string{models.RoleTrainer}, Scopes: []string{models.OAuthScopeFormationContribute}, TokenVersion: 1}
	ctx, err := auth.WithPrincipal(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx = auth.WithOAuthScope(ctx, models.OAuthScopeFormationContribute)
	names := []string{"list_training_library", "get_training", "preview_training_edit", "propose_training_edit", "save_training_edit", "share_training", "get_training_policy"}
	available := []*mcp.Tool{}
	for _, name := range append(slices.Clone(names), "get_learner_context", "get_conversation_context", "join_training", "list_formations") {
		available = append(available, &mcp.Tool{Name: name})
		required, _ := requiredOAuthScopesForTool(name)
		if hasToolOAuthScopes(ctx, name, required) != slices.Contains(names, name) {
			t.Errorf("incorrect scope decision for %s", name)
		}
	}
	var visible []string
	for _, tool := range authorizedNativeTools(ctx, available) {
		visible = append(visible, tool.Name)
	}
	if !slices.Equal(visible, names) {
		t.Fatalf("visible=%v want=%v", visible, names)
	}
}
