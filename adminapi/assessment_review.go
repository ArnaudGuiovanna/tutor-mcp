// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"tutor-mcp/assessment"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// AssessmentReviewStore has no mutation or trust-promotion capability.
type AssessmentReviewStore interface {
	ListAssessmentReviewCandidates(context.Context, models.Principal, string, string, int) (models.AssessmentReviewPage, error)
	GetAssessmentReviewMaterial(context.Context, models.Principal, string) (*models.AssessmentReviewMaterial, error)
}

type AssessmentReviewAPI struct {
	store  AssessmentReviewStore
	logger *slog.Logger
}

func NewAssessmentReview(store AssessmentReviewStore, logger *slog.Logger) *AssessmentReviewAPI {
	if store == nil {
		panic("adminapi: nil assessment review store")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AssessmentReviewAPI{store: store, logger: logger}
}

func (api *AssessmentReviewAPI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/assessment-reviews/attempts", api.list)
	mux.HandleFunc("GET /admin/assessment-reviews/attempts/{attemptID}", api.material)
	mux.HandleFunc("POST /admin/assessment-reviews/attempts/{attemptID}/preview", api.preview)
	return mux
}

func (api *AssessmentReviewAPI) list(w http.ResponseWriter, r *http.Request) {
	actor, ok := principalFor(r, models.OAuthScopeLearnerRead)
	if !ok {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	after, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	page, err := api.store.ListAssessmentReviewCandidates(r.Context(), actor, r.URL.Query().Get("cohort_id"), after, limit)
	if err != nil {
		api.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (api *AssessmentReviewAPI) readMaterial(w http.ResponseWriter, r *http.Request) (*models.AssessmentReviewMaterial, bool) {
	actor, ok := principalFor(r, models.OAuthScopeLearnerRead)
	if !ok {
		writeError(w, http.StatusForbidden, "forbidden")
		return nil, false
	}
	material, err := api.store.GetAssessmentReviewMaterial(r.Context(), actor, r.PathValue("attemptID"))
	if err != nil {
		api.writeError(w, err)
		return nil, false
	}
	return material, true
}

func (api *AssessmentReviewAPI) material(w http.ResponseWriter, r *http.Request) {
	material, ok := api.readMaterial(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, material)
}

// preview checks a proposed rubric_score document, but neither stores the
// proposal nor endorses the reviewer. POST avoids putting learner prose in URLs.
func (api *AssessmentReviewAPI) preview(w http.ResponseWriter, r *http.Request) {
	material, ok := api.readMaterial(w, r)
	if !ok {
		return
	}
	if !material.TextAvailable {
		writeError(w, http.StatusConflict, "review_text_unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, assessment.MaxJSONBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "score_too_large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_score")
		}
		return
	}
	result, err := assessment.EvaluateJSON(material.Rubric, string(raw))
	if err != nil {
		// Parser errors can quote supplied learner prose. Do not log or echo it.
		writeError(w, http.StatusBadRequest, "invalid_score")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"attempt_id": material.AttemptID, "rubric_score": result.Score,
		"total": result.Total, "passed": result.Passed,
		"recorded": false, "trusted_evaluation": false,
	})
}

func (api *AssessmentReviewAPI) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storeport.ErrInvalidPrincipal):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, storeport.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, storeport.ErrInvalidAssessmentReviewRequest):
		writeError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, storeport.ErrAssessmentReviewMaterialUnavailable):
		writeError(w, http.StatusConflict, "review_material_unavailable")
	default:
		api.logger.Error("assessment review read failed", "error_type", "store")
		writeError(w, http.StatusInternalServerError, "internal_error")
	}
}
