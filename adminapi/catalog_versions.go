// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"net/http"

	"tutor-mcp/models"
)

type formationVersionStore interface {
	GetFormationVersion(context.Context, models.Principal, string) (*models.FormationVersionDetail, error)
	CloneFormationVersionIdempotent(context.Context, models.Principal, string, string) (*models.FormationVersionDetail, bool, error)
	ReplaceFormationContentIdempotent(context.Context, models.Principal, string, string, []models.FormationModuleInput, []models.FormationConceptInput) (*models.FormationVersionDetail, bool, error)
	SetFormationEnrollmentPolicy(context.Context, models.Principal, string, string) error
	AssignFormationTrainer(context.Context, models.Principal, string, string, bool) error
	RemoveFormation(context.Context, models.Principal, string) (string, error)
	SetInstitutionLearningPolicy(context.Context, models.Principal, bool) error
	MigrateFormationEnrollment(context.Context, models.Principal, string, string) (*models.FormationMigration, error)
}

func (api *API) registerFormationRoutes(mux *http.ServeMux) {
	store, ok := api.store.(formationVersionStore)
	if !ok {
		return
	}
	mux.HandleFunc("GET /admin/catalog/formation-versions/{versionID}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeLearnerRead)
		if !ok {
			return
		}
		out, err := store.GetFormationVersion(r.Context(), p, r.PathValue("versionID"))
		if err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /admin/catalog/formation-versions/{versionID}/clone", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
		if !ok {
			return
		}
		key, ok := idempotencyKey(w, r)
		if !ok {
			return
		}
		out, replayed, err := store.CloneFormationVersionIdempotent(r.Context(), p, key, r.PathValue("versionID"))
		if err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, replayStatus(replayed), out)
	})
	mux.HandleFunc("PUT /admin/catalog/formation-versions/{versionID}/content", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
		if !ok {
			return
		}
		key, ok := idempotencyKey(w, r)
		if !ok {
			return
		}
		var input struct {
			Modules  []models.FormationModuleInput  `json:"modules"`
			Concepts []models.FormationConceptInput `json:"concepts"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		out, _, err := store.ReplaceFormationContentIdempotent(r.Context(), p, key, r.PathValue("versionID"), input.Modules, input.Concepts)
		if err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("DELETE /admin/catalog/formations/{formationID}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
		if !ok {
			return
		}
		status, err := store.RemoveFormation(r.Context(), p, r.PathValue("formationID"))
		if err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": status})
	})
	mux.HandleFunc("PUT /admin/catalog/formations/{formationID}/enrollment-policy", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
		if !ok {
			return
		}
		var input struct {
			Policy string `json:"policy"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := store.SetFormationEnrollmentPolicy(r.Context(), p, r.PathValue("formationID"), input.Policy); err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, input)
	})
	mux.HandleFunc("PUT /admin/catalog/formations/{formationID}/trainers/{membershipID}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
		if !ok {
			return
		}
		var input struct {
			Assigned bool `json:"assigned"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := store.AssignFormationTrainer(r.Context(), p, r.PathValue("formationID"), r.PathValue("membershipID"), input.Assigned); err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, input)
	})
	mux.HandleFunc("PUT /admin/catalog/learning-policy", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
		if !ok {
			return
		}
		var input struct {
			AllowFreeDomains bool `json:"allow_free_domains"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := store.SetInstitutionLearningPolicy(r.Context(), p, input.AllowFreeDomains); err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, input)
	})
	mux.HandleFunc("POST /admin/catalog/enrollments/{enrollmentID}/migrate", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
		if !ok {
			return
		}
		var input struct {
			CohortID string `json:"cohort_id"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		out, err := store.MigrateFormationEnrollment(r.Context(), p, r.PathValue("enrollmentID"), input.CohortID)
		if err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
}
