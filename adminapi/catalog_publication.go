// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"net/http"
	"tutor-mcp/models"
)

type catalogPublicationStore interface {
	ListFormationVersions(context.Context, models.Principal, string, string, int) (models.FormationVersionPage, error)
	ReviewFormationVersion(context.Context, models.Principal, string) (*models.FormationReview, error)
	PublishFormationToCatalogIdempotent(context.Context, models.Principal, string, string) (*models.FormationPublication, bool, error)
}

func (api *API) registerCatalogPublicationRoutes(mux *http.ServeMux) {
	store, ok := api.store.(catalogPublicationStore)
	if !ok {
		return
	}
	mux.HandleFunc("GET /admin/catalog/formations/{formationID}/versions", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeFormationRead)
		if !ok {
			return
		}
		after, limit, ok := pageParams(w, r)
		if !ok {
			return
		}
		page, err := store.ListFormationVersions(r.Context(), p, r.PathValue("formationID"), after, limit)
		if err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /admin/catalog/formation-versions/{versionID}/review", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeFormationRead)
		if !ok {
			return
		}
		out, err := store.ReviewFormationVersion(r.Context(), p, r.PathValue("versionID"))
		if err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /admin/catalog/formation-versions/{versionID}/publish-to-catalog", func(w http.ResponseWriter, r *http.Request) {
		p, ok := api.principal(w, r, models.OAuthScopeFormationWrite)
		if !ok {
			return
		}
		key, ok := idempotencyKey(w, r)
		if !ok {
			return
		}
		out, replayed, err := store.PublishFormationToCatalogIdempotent(r.Context(), p, key, r.PathValue("versionID"))
		if err != nil {
			api.writeStoreError(w, err)
			return
		}
		writeJSON(w, replayStatus(replayed), out)
	})
}
