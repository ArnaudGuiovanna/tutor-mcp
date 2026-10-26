// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package clientapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const InstitutionActionsPath = "/api/client/v1/institution/actions"

func (api *API) institutionAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, 405, "method_not_allowed")
		return
	}
	// Never export native member-management powers through an agent's MCP
	// OAuth token. The host holds a signed, expiring native account session.
	if !auth.IsNativeClientSession(r.Context()) {
		writeError(w, 403, "native_session_required")
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
	var body struct {
		Action string                    `json:"action"`
		Input  models.InstitutionRequest `json:"input"`
	}
	rawBody, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if readErr != nil {
		writeError(w, 400, "invalid_request")
		return
	}
	decoder := json.NewDecoder(strings.NewReader(string(rawBody)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF || !models.ValidInstitutionAction(body.Action) {
		writeError(w, 400, "invalid_request")
		return
	}
	store, ok := api.store.(storeport.InstitutionActionsStore)
	if !ok {
		writeError(w, 503, "institution_unavailable")
		return
	}
	if len(r.Header.Values(auth.NativeActionProofHeader)) != 1 || auth.NativeClientOrigin(r.Context()) == "" || auth.NativeClientSessionID(r.Context()) == "" || r.URL.RawQuery != "" {
		writeError(w, 401, "action_proof_required")
		return
	}
	proof, err := auth.VerifyNativeActionProof(r.Header.Get(auth.NativeActionProofHeader), auth.NativeClientOrigin(r.Context())+InstitutionActionsPath, p.TenantID, auth.NativeClientSessionID(r.Context()), rawBody, time.Now().UTC())
	if err != nil {
		writeError(w, 401, "invalid_action_proof")
		return
	}
	if err = store.ConsumeNativeActionProof(r.Context(), p, proof); err != nil {
		writeError(w, 401, "invalid_action_proof")
		return
	}
	out, err := store.InstitutionAction(r.Context(), p, body.Action, body.Input)
	switch {
	case errors.Is(err, storeport.ErrInvalidPrincipal):
		writeError(w, 403, "forbidden")
		return
	case errors.Is(err, storeport.ErrIdempotencyKeyConflict):
		writeError(w, 409, "request_conflict")
		return
	case errors.Is(err, storeport.ErrAlreadyMember):
		writeError(w, 409, "already_member")
		return
	case errors.Is(err, storeport.ErrNotFound):
		writeError(w, 404, "not_found")
		return
	case err != nil:
		api.logger.Warn("institution action rejected", "action", body.Action)
		writeError(w, 422, "institution_action_rejected")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
