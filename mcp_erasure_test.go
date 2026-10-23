package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/models"
)

func TestMCPHTTPExistingSessionIsRevokedBeforeErasureDeletesData(t *testing.T) {
	configureMCPHTTPTestJWT(t)
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			f := newMCPHTTPFixture(t, backend)
			actors := f.seedTenant(t, "erasure-http", 2)
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			ownerUser, err := f.admin.GetLocalUserByEmail(ctx, "owner@erasure-http.test")
			if err != nil {
				t.Fatal(err)
			}
			memberships, err := f.admin.ListActiveMembershipsForUser(ctx, ownerUser.ID)
			if err != nil || len(memberships) != 1 {
				t.Fatalf("owner memberships: %v %v", memberships, err)
			}
			owner, err := f.admin.GetPrincipal(ctx, mcpHTTPMembershipScope(&memberships[0]), []string{models.OAuthScopeLearner})
			if err != nil {
				t.Fatal(err)
			}
			server := startMCPHTTPFixture(t, f)
			victim := connectMCPHTTPActor(t, ctx, server, actors[0])
			sibling := connectMCPHTTPActor(t, ctx, server, actors[1])
			args := map[string]any{"objective": "before erasure", "idempotency_key": "before-erasure"}
			if _, _, err := callMCPHTTPTool(ctx, victim, "update_learner_profile", args); err != nil {
				t.Fatal(err)
			}
			token, err := auth.GenerateJWTForPrincipalAndScope(mcpHTTPTestIssuer, auth.MCPResource(mcpHTTPTestIssuer), "mcp-http-test-client", actors[0], models.OAuthScopeLearner)
			if err != nil {
				t.Fatal(err)
			}
			request, err := f.admin.RequestTenantDSAR(ctx, owner, actors[0].LearnerID, "erase", "HTTP access revocation regression")
			if err != nil {
				t.Fatal(err)
			}
			worker := models.WorkerPrincipal{ActorID: "http-erasure-worker"}
			scope := owner.TenantScope()
			scope.UserID, scope.MembershipID = "worker_"+worker.ActorID, "worker_process"
			if done, _, err := f.admin.ProcessTenantDSARErasureBatch(ctx, scope, worker, request.ID, 1, time.Now().UTC()); err != nil || done {
				t.Fatalf("first erasure checkpoint: done=%v error=%v", done, err)
			}
			// The existing initialized SDK session and an exact retry both lose
			// access before any data/receipt can be deleted and then recreated.
			if _, _, err := callMCPHTTPTool(ctx, victim, "update_learner_profile", args); err == nil {
				t.Fatal("existing MCP session replayed a receipt after revocation")
			}
			for _, name := range []string{"get_learner_context", "update_learner_profile"} {
				body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
					"name": name, "arguments": map[string]any{"objective": "forbidden rewrite", "idempotency_key": "after-erasure"},
				}})
				if err != nil {
					t.Fatal(err)
				}
				httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/mcp", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				httpRequest.Header.Set("Authorization", "Bearer "+token)
				httpRequest.Header.Set("Content-Type", "application/json")
				response, err := server.Client().Do(httpRequest)
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				if response.StatusCode != http.StatusUnauthorized {
					t.Fatalf("revoked %s HTTP status=%d, want 401", name, response.StatusCode)
				}
			}
			if _, payload, err := callMCPHTTPTool(ctx, sibling, "update_learner_profile", map[string]any{"objective": "unaffected learner", "idempotency_key": "sibling-write"}); err != nil || payload["updated"] != true {
				t.Fatalf("erasure denied another learner: %v %v", payload, err)
			}
			learner, err := f.admin.GetLearnerByID(ctx, actors[0].LearnerID)
			if err != nil || learner.Objective != "before erasure" {
				t.Fatalf("revoked call changed surviving learner data: %+v %v", learner, err)
			}
		})
	}
}
