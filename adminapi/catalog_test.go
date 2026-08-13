// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/models"
)

type catalogStoreStub struct {
	createCalls int
	lastKey     string
}

func (s *catalogStoreStub) CreateFormationDraftIdempotent(_ context.Context, actor models.Principal, key, name, description string) (*models.Formation, *models.FormationVersion, bool, error) {
	s.createCalls++
	s.lastKey = key
	return &models.Formation{ID: "formation-1", TenantID: actor.TenantID, Name: name, Description: description},
		&models.FormationVersion{ID: "version-1", TenantID: actor.TenantID}, s.createCalls > 1, nil
}
func (*catalogStoreStub) AddFormationModuleIdempotent(context.Context, models.Principal, string, string, models.FormationModuleInput) (string, bool, error) {
	return "", false, nil
}
func (*catalogStoreStub) AddFormationConceptIdempotent(context.Context, models.Principal, string, string, models.FormationConceptInput) (string, bool, error) {
	return "", false, nil
}
func (*catalogStoreStub) PublishFormationVersionIdempotent(context.Context, models.Principal, string, string) (*models.FormationVersion, bool, error) {
	return &models.FormationVersion{}, false, nil
}
func (*catalogStoreStub) CreateCohortIdempotent(context.Context, models.Principal, string, string, string, int, *time.Time, *time.Time) (*models.Cohort, bool, error) {
	return &models.Cohort{}, false, nil
}
func (*catalogStoreStub) EnrollMembershipIdempotent(context.Context, models.Principal, string, string, string, string) (*models.Enrollment, bool, error) {
	return &models.Enrollment{}, false, nil
}
func (*catalogStoreStub) ListFormations(_ context.Context, actor models.Principal, after string, limit int) (models.FormationPage, error) {
	return models.FormationPage{Items: []models.Formation{{ID: "f1", TenantID: actor.TenantID}}, NextAfter: after + "next"}, nil
}
func (*catalogStoreStub) ListCohorts(context.Context, models.Principal, string, int) (models.CohortPage, error) {
	return models.CohortPage{}, nil
}
func (*catalogStoreStub) ListCohortEnrollments(context.Context, models.Principal, string, string, int) (models.EnrollmentPage, error) {
	return models.EnrollmentPage{}, nil
}
func (*catalogStoreStub) GetCohortReport(context.Context, models.Principal, string) (models.CohortReport, error) {
	return models.CohortReport{}, nil
}

func adminRequest(t *testing.T, handler http.Handler, method, path, body, scope string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	principal := models.Principal{UserID: "user", TenantID: "tenant", MembershipID: "membership",
		LearnerID: "learner", Roles: []string{models.RoleOwner}, Scopes: strings.Fields(scope), TokenVersion: 1}
	ctx, err := auth.WithPrincipal(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	ctx = auth.WithOAuthScope(ctx, scope)
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestCatalogAdminHTTPRequiresIdempotencyAndWriteScope(t *testing.T) {
	store := &catalogStoreStub{}
	handler := New(store, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	missing := adminRequest(t, handler, http.MethodPost, "/admin/catalog/formations",
		`{"name":"Course"}`, models.OAuthScopeLearnerWrite, nil)
	if missing.Code != http.StatusBadRequest || store.createCalls != 0 {
		t.Fatalf("missing key status=%d calls=%d body=%s", missing.Code, store.createCalls, missing.Body.String())
	}
	readOnly := adminRequest(t, handler, http.MethodPost, "/admin/catalog/formations",
		`{"name":"Course"}`, models.OAuthScopeLearnerRead, map[string]string{"Idempotency-Key": "request-1"})
	if readOnly.Code != http.StatusForbidden || store.createCalls != 0 {
		t.Fatalf("read-only status=%d calls=%d", readOnly.Code, store.createCalls)
	}
	created := adminRequest(t, handler, http.MethodPost, "/admin/catalog/formations",
		`{"name":"Course"}`, models.OAuthScopeLearnerWrite, map[string]string{"Idempotency-Key": "request-1"})
	if created.Code != http.StatusCreated || store.createCalls != 1 || store.lastKey != "request-1" {
		t.Fatalf("created status=%d calls=%d body=%s", created.Code, store.createCalls, created.Body.String())
	}
	replayed := adminRequest(t, handler, http.MethodPost, "/admin/catalog/formations",
		`{"name":"Course"}`, models.OAuthScopeLearnerWrite, map[string]string{"Idempotency-Key": "request-1"})
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), `"replayed":true`) {
		t.Fatalf("replay status=%d body=%s", replayed.Code, replayed.Body.String())
	}
}

func TestCatalogAdminHTTPPaginationIsBounded(t *testing.T) {
	handler := New(&catalogStoreStub{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	bad := adminRequest(t, handler, http.MethodGet, "/admin/catalog/formations?limit=101", "",
		models.OAuthScopeLearnerRead, nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad page status=%d", bad.Code)
	}
	good := adminRequest(t, handler, http.MethodGet, "/admin/catalog/formations?limit=50&after=f0", "",
		models.OAuthScopeLearnerRead, nil)
	if good.Code != http.StatusOK || !strings.Contains(good.Body.String(), "f0next") {
		t.Fatalf("good page status=%d body=%s", good.Code, good.Body.String())
	}
}
