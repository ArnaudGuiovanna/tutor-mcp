// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"tutor-mcp/models"
)

type CurriculumReviewStore interface {
	GetCurriculumReviewMaterial(context.Context, models.Principal, string, int) (*models.CurriculumReviewMaterial, error)
	RecordCurriculumReview(context.Context, models.Principal, string, int, string, string, string) (*models.CurriculumReviewOpinion, bool, error)
	GetOwnCurriculumReview(context.Context, models.Principal, string, int) (*models.CurriculumReviewOpinion, error)
}

type CurriculumReviewAPI struct {
	store  CurriculumReviewStore
	errors *AssessmentReviewAPI
}

func NewCurriculumReview(store CurriculumReviewStore, logger *slog.Logger) *CurriculumReviewAPI {
	if logger == nil {
		logger = slog.Default()
	}
	return &CurriculumReviewAPI{store: store, errors: &AssessmentReviewAPI{logger: logger}}
}

func (api *CurriculumReviewAPI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/curriculum-reviews/domains/{domainID}/versions/{version}", api.material)
	mux.HandleFunc("POST /admin/curriculum-reviews/domains/{domainID}/versions/{version}/opinions", api.record)
	mux.HandleFunc("GET /admin/curriculum-reviews/domains/{domainID}/versions/{version}/opinions/mine", api.own)
	return mux
}

func curriculumReviewRequest(w http.ResponseWriter, r *http.Request) (models.Principal, int, bool) {
	actor, ok := principalFor(r, models.OAuthScopeLearnerRead)
	if !ok {
		writeError(w, http.StatusForbidden, "forbidden")
		return actor, 0, false
	}
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || version < 1 {
		writeError(w, http.StatusBadRequest, "invalid_version")
		return actor, 0, false
	}
	return actor, version, true
}

func (api *CurriculumReviewAPI) material(w http.ResponseWriter, r *http.Request) {
	actor, version, ok := curriculumReviewRequest(w, r)
	if !ok {
		return
	}
	material, err := api.store.GetCurriculumReviewMaterial(r.Context(), actor, r.PathValue("domainID"), version)
	if err != nil {
		api.errors.writeError(w, err)
		return
	}
	w.Header().Set("ETag", `"`+material.MaterialHash+`"`)
	writeJSON(w, http.StatusOK, material)
}

func (api *CurriculumReviewAPI) record(w http.ResponseWriter, r *http.Request) {
	actor, version, ok := curriculumReviewRequest(w, r)
	if !ok {
		return
	}
	if _, ok := principalFor(r, models.OAuthScopeLearnerWrite); !ok {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	if len(r.Header.Values("Idempotency-Key")) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key")
		return
	}
	match := r.Header.Get("If-Match")
	if match == "" {
		writeError(w, http.StatusPreconditionRequired, "material_precondition_required")
		return
	}
	if len(r.Header.Values("If-Match")) != 1 || len(match) != 66 || match[0] != '"' || match[65] != '"' {
		writeError(w, http.StatusBadRequest, "invalid_material_precondition")
		return
	}
	raw, ok := readReviewScore(w, r)
	if !ok {
		return
	}
	opinion, replayed, err := api.store.RecordCurriculumReview(r.Context(), actor, r.PathValue("domainID"), version, key, match[1:65], raw)
	if err != nil {
		api.errors.writeError(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"opinion": opinion, "replayed": replayed, "curriculum_changed": false})
}

func (api *CurriculumReviewAPI) own(w http.ResponseWriter, r *http.Request) {
	actor, version, ok := curriculumReviewRequest(w, r)
	if !ok {
		return
	}
	opinion, err := api.store.GetOwnCurriculumReview(r.Context(), actor, r.PathValue("domainID"), version)
	if err != nil {
		api.errors.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opinion)
}
