// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package clientapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"tutor-mcp/auth"
	"tutor-mcp/models"
)

const LearningActivationPath = "/api/client/v1/me/learning"

func (api *API) enableLearning(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	actor, ok := auth.GetPrincipal(r.Context())
	if !ok || !auth.IsNativeClientSession(r.Context()) {
		writeError(w, http.StatusForbidden, "native_identity_required")
		return
	}
	store, ok := api.store.(interface {
		EnableNativeLearning(context.Context, models.Principal) (*models.NativeLearningMembership, error)
	})
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "learning_activation_unavailable")
		return
	}
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "json_required")
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || !input.Confirm || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "explicit_confirmation_required")
		return
	}
	out, err := store.EnableNativeLearning(r.Context(), actor)
	if err != nil {
		api.logger.Warn("native learning activation rejected")
		writeError(w, http.StatusForbidden, "learning_activation_rejected")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
