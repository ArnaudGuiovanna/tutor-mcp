// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package adminapi

import (
	"net/http"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func (api *API) registerAdmissionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/catalog/cohorts/{cohortID}/admissions", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeLearnerRead)
		if !ok {
			return
		}
		s, ok := api.store.(storeport.FormationAdmissionStore)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		after, limit, ok := pageParams(w, r)
		if !ok {
			return
		}
		out, err := s.ListFormationAdmissions(r.Context(), p, r.PathValue("cohortID"), after, limit)
		if err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	})
	mux.HandleFunc("POST /admin/catalog/cohorts/{cohortID}/admissions", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeLearnerWrite)
		if !ok {
			return
		}
		s, ok := api.store.(storeport.FormationAdmissionStore)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		key, ok := idempotencyKey(w, r)
		if !ok {
			return
		}
		var input struct {
			MembershipID    string `json:"membership_id"`
			Decision        string `json:"decision"`
			ExpectedVersion int64  `json:"expected_version"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		out, replayed, err := s.DecideFormationAdmission(r.Context(), p, key, r.PathValue("cohortID"), input.MembershipID, input.Decision, input.ExpectedVersion)
		if err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, replayStatus(replayed), map[string]any{"admission": out, "replayed": replayed})
	})
}
