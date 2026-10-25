// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package main

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/adminapi"
	"tutor-mcp/auth"
	"tutor-mcp/clientapi"
	"tutor-mcp/models"
	"tutor-mcp/tools"
)

func TestNativeClientAuthorPublishAndLearningJourney(t *testing.T) {
	configureMCPHTTPTestJWT(t)
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			f := newMCPHTTPFixture(t, backend)
			operator := models.ControlPlanePrincipal{ActorID: "native-journey", Roles: []string{models.RolePlatformAdmin}, Reason: "native client journey", RequestID: "native-journey"}
			tenant, err := f.admin.ProvisionTenant(t.Context(), operator, "native-journey", "Journey institution", "test", "plan_legacy")
			if err != nil {
				t.Fatal(err)
			}
			pub := func(index byte) string {
				secret := make([]byte, 32)
				secret[31] = index
				_, key := btcec.PrivKeyFromBytes(secret)
				return hex.EncodeToString(schnorr.SerializePubKey(key))
			}
			identity, err := f.admin.BootstrapNativeOwner(t.Context(), operator, models.NativeOwnerProvision{TenantID: tenant.ID, DisplayName: "Owner", PublicKey: pub(71), RecoveryPublicKey: pub(72)})
			if err != nil {
				t.Fatal(err)
			}
			token := func(identity *models.NativeIdentity, id, scope string) (string, models.Principal) {
				row := &models.NativeSession{ID: id, IdentityID: identity.ID, TenantID: identity.TenantID, UserID: identity.UserID, MembershipID: identity.MembershipID, LearnerID: identity.LearnerID, KeyVersion: identity.Version, Scopes: strings.Fields(scope), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(25 * time.Minute)}
				p, err := f.admin.CreateNativeSession(t.Context(), row)
				if err != nil {
					t.Fatal(err)
				}
				bearer, err := auth.GenerateJWTForPrincipalAndScope(mcpHTTPTestIssuer, auth.MCPResource(mcpHTTPTestIssuer), "native:"+id, p, scope)
				if err != nil {
					t.Fatal(err)
				}
				return bearer, p
			}
			read, _ := token(identity, "journey-read", "formation:read")
			write, owner := token(identity, "journey-write", "formation:read formation:write")
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			mux := http.NewServeMux()
			protect := func(next http.Handler) http.Handler {
				return auth.BearerMiddlewareWithNativePrincipalValidator(mcpHTTPTestIssuer, models.OAuthScopeLearnerRead, f.runtime, next)
			}
			mux.Handle("/api/client/v1/", protect(clientapi.New(f.runtime, logger).Handler()))
			mux.Handle("/admin/catalog/", protect(nativeAdministrativeScope("/admin/catalog/", adminapi.New(f.runtime, logger).Handler())))
			ip := auth.NewRateLimiterWithNamespace("native-journey-ip", 1000, 2000)
			defer ip.Stop()
			principal := auth.NewRateLimiterWithNamespace("native-journey-principal", 1000, 2000)
			defer principal.Stop()
			concurrency, err := auth.NewPrincipalConcurrencyLimiter(128, 8)
			if err != nil {
				t.Fatal(err)
			}
			mux.Handle("/mcp", newMCPHTTPHandler(&tools.Deps{Store: f.runtime, Logger: logger, BaseURL: mcpHTTPTestIssuer, Institution: true}, defaultMCPMaxRequestBodyBytes, defaultMCPToolCallTimeout, mcpHTTPGuards{IP: ip, Learner: principal, Concurrency: concurrency, NativeOnly: true}))
			server := newIPv4HTTPTestServer(t, mux)
			defer server.Close()
			request := func(bearer, method, path, body string) (int, map[string]any) {
				r, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Authorization", "Bearer "+bearer)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Idempotency-Key", "journey-"+method+"-"+path)
				res, err := server.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				data, _ := io.ReadAll(res.Body)
				var value map[string]any
				_ = json.Unmarshal(data, &value)
				return res.StatusCode, value
			}
			list := func(bearer string) []string {
				httpClient := &http.Client{Transport: bearerRoundTripper{base: http.DefaultTransport, token: bearer}, Timeout: 20 * time.Second}
				client := mcp.NewClient(&mcp.Implementation{Name: "native-journey", Version: "test"}, nil)
				session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: httpClient}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer session.Close()
				result, err := session.ListTools(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				for _, tool := range result.Tools {
					names = append(names, tool.Name)
				}
				return names
			}
			names := list(read)
			if !slices.Contains(names, "list_formation_versions") || slices.Contains(names, "list_trainer_cohorts") || slices.Contains(names, "get_learner_context") || slices.Contains(names, "publish_formation_to_catalog") {
				t.Fatalf("irrelevant tools exposed to owner read activity: %v", names)
			}
			if status, _ := request(read, "POST", "/admin/catalog/formations", `{"name":"Denied"}`); status != 403 {
				t.Fatalf("read session wrote: %d", status)
			}
			status, created := request(write, "POST", "/admin/catalog/formations", `{"name":"A real institutional formation","description":"Learn through examples"}`)
			if status != 201 {
				t.Fatalf("create=%d %#v", status, created)
			}
			formationID := created["formation"].(map[string]any)["id"].(string)
			versionID := created["version"].(map[string]any)["id"].(string)
			if _, err = f.admin.AddFormationModule(t.Context(), owner, versionID, models.FormationModuleInput{StableKey: "m", Title: "Module"}); err != nil {
				t.Fatal(err)
			}
			if _, err = f.admin.AddFormationConcept(t.Context(), owner, versionID, models.FormationConceptInput{StableKey: "c", ModuleStableKey: "m", Label: "Concept"}); err != nil {
				t.Fatal(err)
			}
			if status, out := request(read, "GET", "/admin/catalog/formations/"+formationID+"/versions", ""); status != 200 || len(out["items"].([]any)) != 1 {
				t.Fatalf("versions=%d %#v", status, out)
			}
			if status, out := request(read, "GET", "/admin/catalog/formation-versions/"+versionID+"/review", ""); status != 200 || out["publishable"] != true {
				t.Fatalf("review=%d %#v", status, out)
			}
			if status, _ := request(read, "POST", "/admin/catalog/formation-versions/"+versionID+"/publish-to-catalog", `{}`); status != 403 {
				t.Fatalf("read session published=%d", status)
			}
			if status, out := request(write, "POST", "/admin/catalog/formation-versions/"+versionID+"/publish-to-catalog", `{}`); status != 201 || out["cohort"] == nil {
				t.Fatalf("publish=%d %#v", status, out)
			}
			if status, out := request(read, "GET", "/admin/catalog/formation-versions/"+versionID+"/review", ""); status != 200 || out["catalog_available"] != true {
				t.Fatalf("catalogue state=%d %#v", status, out)
			}
			if status, _ := request(write, "POST", clientapi.LearningActivationPath, `{"confirm":false}`); status != 400 {
				t.Fatalf("unconfirmed opt-in=%d", status)
			}
			if status, out := request(write, "POST", clientapi.LearningActivationPath, `{"confirm":true}`); status != 200 || out["changed"] != true {
				t.Fatalf("opt-in=%d %#v", status, out)
			}
			if status, _ := request(write, "GET", clientapi.OverviewPath, ""); status != 401 {
				t.Fatalf("old session survived=%d", status)
			}
			identity, err = f.admin.GetNativeIdentity(t.Context(), tenant.ID, identity.PublicKey, "session")
			if err != nil {
				t.Fatal(err)
			}
			learning, _ := token(identity, "journey-learning", "learner:read learner:write")
			status, overview := request(learning, "GET", clientapi.OverviewPath, "")
			if status != 200 {
				t.Fatalf("learning overview=%d %#v", status, overview)
			}
			if len(overview["formations"].(map[string]any)["items"].([]any)) != 0 || len(overview["availableFormations"].(map[string]any)["items"].([]any)) != 1 {
				t.Fatalf("publication or opt-in auto-enrolled: %#v", overview)
			}
			if status, _ := request(learning, "POST", "/admin/catalog/formation-versions/"+versionID+"/publish-to-catalog", `{}`); status != 403 {
				t.Fatalf("learning activity gained author delegation=%d", status)
			}
			staff, err := f.admin.ProvisionNativeMember(t.Context(), operator, models.NativeOwnerProvision{TenantID: tenant.ID, DisplayName: "Trainer", PublicKey: pub(73), RecoveryPublicKey: pub(74)}, []string{models.RoleTrainer})
			if err != nil {
				t.Fatal(err)
			}
			training, _ := token(staff, "journey-training", "progress:read")
			if status, out := request(training, "GET", clientapi.OverviewPath, ""); status != 200 || out["capabilities"].(map[string]any)["training"] != true || out["identity"].(map[string]any)["learnerId"] != "" {
				t.Fatalf("trainer overview=%d %#v", status, out)
			}
			names = list(training)
			if !slices.Contains(names, "list_trainer_cohorts") || slices.Contains(names, "list_formations") || slices.Contains(names, "get_learner_context") {
				t.Fatalf("trainer discovery=%v", names)
			}
		})
	}
}
