// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"tutor-mcp/auth"
	"tutor-mcp/clientapi"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func startClientAPIFixture(t *testing.T, backend storeport.Store) *httptest.Server {
	t.Helper()
	ip := auth.NewRateLimiterWithNamespace("client_api_ip", 1000, 2000)
	principal := auth.NewRateLimiterWithNamespace("client_api_principal", 1000, 2000)
	t.Cleanup(ip.Stop)
	t.Cleanup(principal.Stop)
	concurrency, err := auth.NewPrincipalConcurrencyLimiter(128, 8)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	mountClientAPI(mux, backend, logger, mcpHTTPTestIssuer, mcpHTTPGuards{IP: ip, Learner: principal, Concurrency: concurrency})
	server := newIPv4HTTPTestServer(t, wrapHTTPHandler(logger, mcpHTTPTestIssuer, mux))
	t.Cleanup(server.Close)
	return server
}

func clientAPIRequest(t *testing.T, server *httptest.Server, actor *models.Principal, scope, method, query string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, server.URL+clientapi.OverviewPath+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if actor != nil {
		token, err := auth.GenerateJWTForPrincipalAndScope(mcpHTTPTestIssuer, auth.MCPResource(mcpHTTPTestIssuer), "native-client-test", *actor, scope)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("client response can be cached: status=%d headers=%v", response.StatusCode, response.Header)
	}
	return response.StatusCode, response.Header, body
}

func TestClientAPIOverviewUsesOnlyAuthenticatedLearnerAndScope(t *testing.T) {
	configureMCPHTTPTestJWT(t)
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			f := newMCPHTTPFixture(t, backend)
			actors := append(f.seedTenant(t, "client-alpha", 2), f.seedTenant(t, "client-beta", 1)...)
			f.assertPostgresRLS(t, actors)
			for _, actor := range actors {
				err := f.runtime.WithTenantTx(t.Context(), actor.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
					objective := "objective-" + actor.LearnerID
					if err := scoped.UpdateLearnerProfile(ctx, actor.LearnerID, `{"language":"fr","notes":"private-profile-note","email":"hidden@profile.test"}`, &objective); err != nil {
						return err
					}
					_, err := scoped.CreateDomain(ctx, actor.LearnerID, "domain-"+actor.LearnerID, "learn independently", models.KnowledgeSpace{Concepts: []string{"concept"}})
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			server := startClientAPIFixture(t, f.runtime)
			if status, _, _ := clientAPIRequest(t, server, nil, "", http.MethodGet, ""); status != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status=%d", status)
			}
			for _, scope := range []string{models.OAuthScopeLearnerWrite, models.OAuthScopeFormationRead} {
				if status, _, _ := clientAPIRequest(t, server, &actors[0], scope, http.MethodGet, ""); status != http.StatusForbidden {
					t.Fatalf("scope %q status=%d", scope, status)
				}
			}
			for _, actor := range actors {
				for _, scope := range []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearner} {
					status, header, body := clientAPIRequest(t, server, &actor, scope, http.MethodGet, "")
					if status != http.StatusOK || header.Get("Content-Type") != "application/json" {
						t.Fatalf("overview status=%d type=%q body=%s", status, header.Get("Content-Type"), body)
					}
					var response clientapi.Overview
					if err := json.Unmarshal(body, &response); err != nil {
						t.Fatal(err)
					}
					if response.ContractVersion != 1 || response.Identity.TenantID != actor.TenantID || response.Identity.MembershipID != actor.MembershipID || response.Identity.UserID != actor.UserID || response.Identity.LearnerID != actor.LearnerID || response.Context.Learner.ID != actor.LearnerID || response.Context.Learner.Objective != "objective-"+actor.LearnerID || response.Context.Learner.Profile.Language != "fr" {
						t.Fatalf("identity projection mismatch: %+v", response)
					}
					if len(response.Context.Domains) != 1 || response.Context.Domains[0].Name != "domain-"+actor.LearnerID || response.Context.Domains[0].ConceptCount != 1 {
						t.Fatalf("foreign or missing domains: %+v", response.Context.Domains)
					}
					for _, forbidden := range []string{"private-profile-note", "hidden@profile.test", "password", "test-password-hash", "notes"} {
						if strings.Contains(string(body), forbidden) {
							t.Fatalf("response exposed %q", forbidden)
						}
					}
					for _, other := range actors {
						if other.LearnerID != actor.LearnerID && strings.Contains(string(body), other.LearnerID) {
							t.Fatal("another learner identity reached response")
						}
					}
					if response.Formations.Items == nil || response.AvailableFormations.Items == nil || !response.Capabilities.CommonCatalog || response.Capabilities.PersonalLearning != nil {
						t.Fatalf("empty pages or unknown capabilities are misleading: %+v", response)
					}
				}
			}
			for _, mutate := range []func(*models.Principal){
				func(p *models.Principal) { p.TenantID = actors[2].TenantID },
				func(p *models.Principal) { p.LearnerID = actors[1].LearnerID },
				func(p *models.Principal) { p.MembershipID = actors[1].MembershipID },
			} {
				forged := actors[0]
				mutate(&forged)
				if status, _, _ := clientAPIRequest(t, server, &forged, models.OAuthScopeLearner, http.MethodGet, ""); status != http.StatusUnauthorized {
					t.Fatalf("forged identity status=%d", status)
				}
			}
			for _, query := range []string{"?tenant_id=" + actors[2].TenantID, "?learner_id=" + actors[1].LearnerID, "?limit=0", "?limit=101", "?limit=no", "?limit=", "?limit=1&limit=2", "?after=%00", "?available_after=%FF", "?after=" + strings.Repeat("x", 257)} {
				if status, _, _ := clientAPIRequest(t, server, &actors[0], models.OAuthScopeLearnerRead, http.MethodGet, query); status != http.StatusBadRequest {
					t.Fatalf("invalid query %q status=%d", query, status)
				}
			}
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
				if status, header, _ := clientAPIRequest(t, server, &actors[0], models.OAuthScopeLearnerRead, method, ""); status != http.StatusMethodNotAllowed || header.Get("Allow") != http.MethodGet {
					t.Fatalf("mutation method %s status=%d allow=%q", method, status, header.Get("Allow"))
				}
			}
			if _, err := f.admin.SetMembershipAuthorization(t.Context(), actors[0].TenantScope(), models.MembershipStatusSuspended, actors[0].Roles); err != nil {
				t.Fatal(err)
			}
			if status, _, _ := clientAPIRequest(t, server, &actors[0], models.OAuthScopeLearnerRead, http.MethodGet, ""); status != http.StatusUnauthorized {
				t.Fatalf("revoked principal status=%d", status)
			}
		})
	}
}

func clientAPIOwner(t *testing.T, f *mcpHTTPFixture, slug string) models.Principal {
	t.Helper()
	user, err := f.admin.GetLocalUserByEmail(t.Context(), "owner@"+slug+".test")
	if err != nil {
		t.Fatal(err)
	}
	memberships, err := f.admin.ListActiveMembershipsForUser(t.Context(), user.ID)
	if err != nil || len(memberships) != 1 {
		t.Fatalf("owner memberships=%v err=%v", memberships, err)
	}
	principal, err := f.admin.GetPrincipal(t.Context(), mcpHTTPMembershipScope(&memberships[0]), []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	return principal
}

func TestClientAPIOverviewCatalogPaginationAndOwnEnrollments(t *testing.T) {
	configureMCPHTTPTestJWT(t)
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			f := newMCPHTTPFixture(t, backend)
			actors := f.seedTenant(t, "client-catalog", 2)
			owner := clientAPIOwner(t, f, "client-catalog")
			for i := range 3 {
				_, version, err := f.admin.CreateFormationDraft(t.Context(), owner, fmt.Sprintf("Course %d", i), "Shared learning")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.admin.AddFormationModule(t.Context(), owner, version.ID, models.FormationModuleInput{StableKey: "foundation", Title: "Foundation", Position: 0}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.admin.AddFormationConcept(t.Context(), owner, version.ID, models.FormationConceptInput{ModuleStableKey: "foundation", StableKey: "numbers", Label: "Numbers", Position: 0}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.admin.PublishFormationVersion(t.Context(), owner, version.ID); err != nil {
					t.Fatal(err)
				}
				cohort, err := f.admin.CreateCohort(t.Context(), owner, version.ID, fmt.Sprintf("Group %d", i), 10, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.admin.EnrollMembership(t.Context(), owner, cohort.ID, actors[0].MembershipID, "{}"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.admin.EnrollMembership(t.Context(), owner, cohort.ID, actors[1].MembershipID, "{}"); err != nil {
					t.Fatal(err)
				}
			}
			server := startClientAPIFixture(t, f.runtime)
			var seen = map[string]bool{}
			after, availableAfter := "", ""
			for page := range 3 {
				query := "?limit=1&after=" + url.QueryEscape(after) + "&available_after=" + url.QueryEscape(availableAfter)
				status, _, body := clientAPIRequest(t, server, &actors[0], models.OAuthScopeLearnerRead, http.MethodGet, query)
				if status != http.StatusOK {
					t.Fatalf("page %d status=%d body=%s", page, status, body)
				}
				var response clientapi.Overview
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Formations.Items) != 1 || len(response.AvailableFormations.Items) != 1 {
					t.Fatalf("page %d sizes=%d,%d", page, len(response.Formations.Items), len(response.AvailableFormations.Items))
				}
				item := response.Formations.Items[0]
				if item.Enrollment == nil || item.Enrollment.LearnerID != actors[0].LearnerID || item.Enrollment.MembershipID != actors[0].MembershipID || seen[item.CohortID] {
					t.Fatalf("wrong or repeated enrollment: %+v", item)
				}
				seen[item.CohortID] = true
				after, availableAfter = response.Formations.NextAfter, response.AvailableFormations.NextAfter
				if (page < 2 && (after == "" || availableAfter == "")) || (page == 2 && (after != "" || availableAfter != "")) {
					t.Fatalf("page %d cursor after=%q available=%q", page, after, availableAfter)
				}
			}
		})
	}
}

type clientAPIRevokingStore struct {
	storeport.Store
	before func() error
	once   sync.Once
	err    error
}

func (s *clientAPIRevokingStore) WithTenantReadTx(ctx context.Context, scope models.TenantScope, fn func(context.Context, storeport.Store) error) error {
	s.once.Do(func() { s.err = s.before() })
	if s.err != nil {
		return s.err
	}
	return s.Store.WithTenantReadTx(ctx, scope, fn)
}

func TestClientAPIRevalidatesMembershipInsideReadTransaction(t *testing.T) {
	configureMCPHTTPTestJWT(t)
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			f := newMCPHTTPFixture(t, backend)
			actor := f.seedTenant(t, "client-read-fence", 1)[0]
			store := &clientAPIRevokingStore{Store: f.runtime, before: func() error {
				_, err := f.admin.SetMembershipAuthorization(t.Context(), actor.TenantScope(), models.MembershipStatusRevoked, actor.Roles)
				return err
			}}
			server := startClientAPIFixture(t, store)
			status, _, body := clientAPIRequest(t, server, &actor, models.OAuthScopeLearnerRead, http.MethodGet, "")
			if status != http.StatusUnauthorized || strings.Contains(string(body), actor.LearnerID) {
				t.Fatalf("revocation between bearer and read transaction leaked data: status=%d body=%s", status, body)
			}
		})
	}
}
