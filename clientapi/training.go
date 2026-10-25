// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package clientapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const TrainingPath = "/api/client/v1/training/"

func (api *API) training(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, TrainingPath)
	if !models.ValidTrainingAction(action) {
		writeError(w, 404, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, 405, "method_not_allowed")
		return
	}
	p, ok := auth.GetPrincipal(r.Context())
	if !ok {
		writeError(w, 401, "authentication_required")
		return
	}
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		writeError(w, 415, "json_required")
		return
	}
	var in models.TrainingRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*1024*1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, 400, "invalid_request")
		return
	}
	store, ok := api.store.(storeport.TrainingStore)
	if !ok {
		writeError(w, 503, "training_unavailable")
		return
	}
	out, err := store.TrainingAction(r.Context(), p, action, in)
	switch {
	case errors.Is(err, storeport.ErrInvalidPrincipal):
		writeError(w, 403, "forbidden")
		return
	case errors.Is(err, storeport.ErrCurriculumVersionConflict), errors.Is(err, storeport.ErrIdempotencyKeyConflict):
		writeError(w, 409, "revision_conflict")
		return
	case errors.Is(err, storeport.ErrFormationUnavailable), errors.Is(err, storeport.ErrNotFound):
		writeError(w, 404, "training_unavailable")
		return
	case err != nil:
		api.logger.Warn("training action rejected", "action", action)
		writeError(w, 422, "training_action_rejected")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
