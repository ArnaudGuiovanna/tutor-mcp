package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/models"
)

func TestTenantToolRechecksAuthorizationAfterAuthentication(t *testing.T) {
	for _, name := range []string{"get_pending_alerts", "update_learner_profile"} {
		for _, status := range []string{models.MembershipStatusRevoked, models.MembershipStatusSuspended, models.MembershipStatusActive} {
			t.Run(name+"/"+status, func(t *testing.T) {
				s, deps := setupToolsTest(t)
				principal, err := s.GetPrincipalForLearner(t.Context(), "L_owner", []string{models.OAuthScopeLearner})
				if err != nil {
					t.Fatal(err)
				}
				// Simulate HTTP authentication succeeding immediately before a
				// revocation, suspension or version change commits.
				if err := s.ValidatePrincipal(t.Context(), principal); err != nil {
					t.Fatal(err)
				}
				ctx, err := auth.WithPrincipal(t.Context(), principal)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.SetMembershipAuthorization(t.Context(), principal.TenantScope(), status, principal.Roles); err != nil {
					t.Fatal(err)
				}
				reader := toolMetricReader(t)
				called := false
				h := toolCallObservationMiddleware(map[string]struct{}{name: {}})(toolTenantTransactionMiddleware(deps)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
					called = true
					return &mcp.CallToolResult{}, nil
				}))
				result, err := h(ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name}})
				if err != nil {
					t.Fatal(err)
				}
				toolResult, ok := result.(*mcp.CallToolResult)
				if called || !ok || !toolResult.IsError || !strings.Contains(resultText(toolResult), "principal is no longer valid") {
					t.Fatalf("stale authorization reached handler=%v result=%+v", called, result)
				}
				requireToolMetric(t, reader, name, "denied", 1)
			})
		}
	}
}
