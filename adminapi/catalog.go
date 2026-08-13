// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

// Package adminapi exposes the tenant-bound pedagogical administration API.
package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const maxAdminBodyBytes = 1 << 20

type CatalogStore interface {
	CreateFormationDraftIdempotent(context.Context, models.Principal, string, string, string) (*models.Formation, *models.FormationVersion, bool, error)
	AddFormationModuleIdempotent(context.Context, models.Principal, string, string, models.FormationModuleInput) (string, bool, error)
	AddFormationConceptIdempotent(context.Context, models.Principal, string, string, models.FormationConceptInput) (string, bool, error)
	PublishFormationVersionIdempotent(context.Context, models.Principal, string, string) (*models.FormationVersion, bool, error)
	CreateCohortIdempotent(context.Context, models.Principal, string, string, string, int, *time.Time, *time.Time) (*models.Cohort, bool, error)
	EnrollMembershipIdempotent(context.Context, models.Principal, string, string, string, string) (*models.Enrollment, bool, error)
	ListFormations(context.Context, models.Principal, string, int) (models.FormationPage, error)
	ListCohorts(context.Context, models.Principal, string, int) (models.CohortPage, error)
	ListCohortEnrollments(context.Context, models.Principal, string, string, int) (models.EnrollmentPage, error)
	GetCohortReport(context.Context, models.Principal, string) (models.CohortReport, error)
}

type API struct {
	store  CatalogStore
	logger *slog.Logger
}

func New(store CatalogStore, logger *slog.Logger) *API {
	if store == nil {
		panic("adminapi: nil catalog store")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &API{store: store, logger: logger}
}

func (api *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/catalog/formations", api.listFormations)
	mux.HandleFunc("POST /admin/catalog/formations", api.createFormation)
	mux.HandleFunc("POST /admin/catalog/formation-versions/{versionID}/modules", api.addModule)
	mux.HandleFunc("POST /admin/catalog/formation-versions/{versionID}/concepts", api.addConcept)
	mux.HandleFunc("POST /admin/catalog/formation-versions/{versionID}/publish", api.publishVersion)
	mux.HandleFunc("GET /admin/catalog/cohorts", api.listCohorts)
	mux.HandleFunc("POST /admin/catalog/cohorts", api.createCohort)
	mux.HandleFunc("GET /admin/catalog/cohorts/{cohortID}/enrollments", api.listEnrollments)
	mux.HandleFunc("POST /admin/catalog/cohorts/{cohortID}/enrollments", api.createEnrollment)
	mux.HandleFunc("GET /admin/catalog/cohorts/{cohortID}/report", api.cohortReport)
	return mux
}

func principalFor(r *http.Request, requiredScope string) (models.Principal, bool) {
	principal, ok := auth.GetPrincipal(r.Context())
	if !ok || principal.Validate() != nil || !auth.HasOAuthScope(r.Context(), requiredScope) {
		return models.Principal{}, false
	}
	return principal, true
}

func (api *API) principal(w http.ResponseWriter, r *http.Request, scope string) (models.Principal, bool) {
	principal, ok := principalFor(r, scope)
	if !ok {
		writeError(w, http.StatusForbidden, "forbidden")
	}
	return principal, ok
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "idempotency_key_required")
		return "", false
	}
	return key, true
}

func pageParams(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "invalid_page")
			return "", 0, false
		}
		limit = parsed
	}
	return r.URL.Query().Get("after"), limit, true
}

func (api *API) writeStoreError(w http.ResponseWriter, err error) {
	status, code := http.StatusBadRequest, "invalid_request"
	switch {
	case errors.Is(err, storeport.ErrInvalidPrincipal):
		status, code = http.StatusForbidden, "forbidden"
	case strings.Contains(err.Error(), "idempotency key conflict"):
		status, code = http.StatusConflict, "idempotency_conflict"
	case strings.Contains(err.Error(), "capacity"):
		status, code = http.StatusConflict, "cohort_capacity_reached"
	case strings.Contains(err.Error(), "not found"):
		status, code = http.StatusNotFound, "not_found"
	}
	if status >= 500 {
		api.logger.Error("catalog admin request failed", "error_type", "store")
	}
	writeError(w, status, code)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func replayStatus(replayed bool) int {
	if replayed {
		return http.StatusOK
	}
	return http.StatusCreated
}

func (api *API) createFormation(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	formation, version, replayed, err := api.store.CreateFormationDraftIdempotent(r.Context(), principal, key, input.Name, input.Description)
	if err != nil {
		api.writeStoreError(w, err)
		return
	}
	writeJSON(w, replayStatus(replayed), map[string]any{"formation": formation, "version": version, "replayed": replayed})
}

func (api *API) addModule(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input models.FormationModuleInput
	if !decodeJSON(w, r, &input) {
		return
	}
	id, replayed, err := api.store.AddFormationModuleIdempotent(r.Context(), principal, key, r.PathValue("versionID"), input)
	if err != nil {
		api.writeStoreError(w, err)
		return
	}
	writeJSON(w, replayStatus(replayed), map[string]any{"id": id, "replayed": replayed})
}

func (api *API) addConcept(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input models.FormationConceptInput
	if !decodeJSON(w, r, &input) {
		return
	}
	id, replayed, err := api.store.AddFormationConceptIdempotent(r.Context(), principal, key, r.PathValue("versionID"), input)
	if err != nil {
		api.writeStoreError(w, err)
		return
	}
	writeJSON(w, replayStatus(replayed), map[string]any{"id": id, "replayed": replayed})
}

func (api *API) publishVersion(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	version, replayed, err := api.store.PublishFormationVersionIdempotent(r.Context(), principal, key, r.PathValue("versionID"))
	if err != nil {
		api.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": version, "replayed": replayed})
}

func (api *API) listFormations(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.principal(w, r, models.OAuthScopeLearnerRead)
	if !ok {
		return
	}
	after, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	page, err := api.store.ListFormations(r.Context(), principal, after, limit)
	if err != nil {
		api.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (api *API) createCohort(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input struct {
		FormationVersionID string     `json:"formation_version_id"`
		Name               string     `json:"name"`
		Capacity           int        `json:"capacity"`
		StartsAt           *time.Time `json:"starts_at"`
		EndsAt             *time.Time `json:"ends_at"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	cohort, replayed, err := api.store.CreateCohortIdempotent(r.Context(), principal, key, input.FormationVersionID, input.Name, input.Capacity, input.StartsAt, input.EndsAt)
	if err != nil {
		api.writeStoreError(w, err)
		return
	}
	writeJSON(w, replayStatus(replayed), map[string]any{"cohort": cohort, "replayed": replayed})
}

func (api *API) listCohorts(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.principal(w, r, models.OAuthScopeLearnerRead)
	if !ok {
		return
	}
	after, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	page, err := api.store.ListCohorts(r.Context(), principal, after, limit)
	if err != nil {
		api.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (api *API) createEnrollment(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input struct {
		MembershipID string          `json:"membership_id"`
		Objectives   json.RawMessage `json:"objectives"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	objectives := string(input.Objectives)
	if objectives == "" || objectives == "null" {
		objectives = "{}"
	}
	enrollment, replayed, err := api.store.EnrollMembershipIdempotent(r.Context(), principal, key, r.PathValue("cohortID"), input.MembershipID, objectives)
	if err != nil {
		api.writeStoreError(w, err)
		return
	}
	writeJSON(w, replayStatus(replayed), map[string]any{"enrollment": enrollment, "replayed": replayed})
}

func (api *API) listEnrollments(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.principal(w, r, models.OAuthScopeLearnerRead)
	if !ok {
		return
	}
	after, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	page, err := api.store.ListCohortEnrollments(r.Context(), principal, r.PathValue("cohortID"), after, limit)
	if err != nil {
		api.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (api *API) cohortReport(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.principal(w, r, models.OAuthScopeLearnerRead)
	if !ok {
		return
	}
	report, err := api.store.GetCohortReport(r.Context(), principal, r.PathValue("cohortID"))
	if err != nil {
		api.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
