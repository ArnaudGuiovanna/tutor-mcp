// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

// Package clientapi serves structured, learner-owned views for native clients.
// It shares the Tutor service's /mcp OAuth resource and persisted identity
// checks. It does not grant browser-console administrative capabilities.
package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const OverviewPath = "/api/client/v1/overview"
const RequiredScope = models.OAuthScopeLearnerRead

type API struct {
	store  storeport.Store
	logger *slog.Logger
}

func New(store storeport.Store, logger *slog.Logger) *API {
	if store == nil {
		panic("clientapi: nil store")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &API{store: store, logger: logger}
}

// NoStore must wrap the authentication boundary too: denied or expired access
// responses must not be cached by a desktop network layer or an HTTP proxy.
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (api *API) Handler() http.Handler {
	return NoStore(http.HandlerFunc(api.handle))
}

type Identity struct {
	TenantID     string   `json:"tenantId"`
	UserID       string   `json:"userId"`
	MembershipID string   `json:"membershipId"`
	LearnerID    string   `json:"learnerId"`
	Roles        []string `json:"roles"`
}

type Learner struct {
	ID         string                `json:"id"`
	Objective  string                `json:"objective"`
	Profile    models.LearnerProfile `json:"profile"`
	CreatedAt  time.Time             `json:"createdAt"`
	LastActive time.Time             `json:"lastActive"`
}

type Domain struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Archived     bool      `json:"archived"`
	HighStakes   bool      `json:"highStakes"`
	PriorityRank *int      `json:"priorityRank,omitempty"`
	ConceptCount int       `json:"conceptCount"`
	CreatedAt    time.Time `json:"createdAt"`
}

type ConceptState struct {
	DomainID   string     `json:"domainId"`
	Concept    string     `json:"concept"`
	Stability  float64    `json:"stability"`
	CardState  string     `json:"cardState"`
	LastReview *time.Time `json:"lastReview,omitempty"`
	PMastery   float64    `json:"pMastery"`
}

type InteractionCount struct {
	DomainID string `json:"domainId"`
	Concept  string `json:"concept"`
	Count    int    `json:"count"`
}

type Context struct {
	Learner           Learner            `json:"learner"`
	Domains           []Domain           `json:"domains"`
	ConceptStates     []ConceptState     `json:"conceptStates"`
	TodayInteractions []InteractionCount `json:"todayInteractions"`
}

type Capabilities struct {
	CommonCatalog      bool `json:"commonCatalog"`
	Learning           bool `json:"learning"`
	FormationAuthoring bool `json:"formationAuthoring"`
	// The current read port does not expose the institution's personal-domain
	// policy. Unknown is omitted rather than announced as an authorization.
	PersonalLearning *bool `json:"personalLearning,omitempty"`
}

type Overview struct {
	ContractVersion     int                         `json:"contractVersion"`
	Identity            Identity                    `json:"identity"`
	Context             Context                     `json:"context"`
	Formations          models.LearnerFormationPage `json:"formations"`
	AvailableFormations models.LearnerFormationPage `json:"availableFormations"`
	AuthoredFormations  models.FormationPage        `json:"authoredFormations"`
	Capabilities        Capabilities                `json:"capabilities"`
}

func (api *API) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != OverviewPath {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	actor, ok := auth.GetPrincipal(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required")
		return
	}
	canLearn := auth.HasOAuthScope(r.Context(), RequiredScope) && actor.LearnerID != "" &&
		actor.Authorize(models.PermissionLearningSelf, models.AuthorizationResource{TenantID: actor.TenantID, OwnerUserID: actor.UserID})
	canViewAuthoring := auth.HasOAuthScope(r.Context(), models.OAuthScopeFormationRead) &&
		actor.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: actor.TenantID})
	if !canLearn && !canViewAuthoring {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="learner:read"`)
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	params, err := parsePageParams(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_page")
		return
	}
	result := Overview{
		ContractVersion:     1,
		Identity:            Identity{TenantID: actor.TenantID, UserID: actor.UserID, MembershipID: actor.MembershipID, LearnerID: actor.LearnerID, Roles: append([]string{}, actor.Roles...)},
		Context:             Context{Domains: []Domain{}, ConceptStates: []ConceptState{}, TodayInteractions: []InteractionCount{}},
		Formations:          models.LearnerFormationPage{Items: []models.LearnerFormation{}},
		AvailableFormations: models.LearnerFormationPage{Items: []models.LearnerFormation{}},
		AuthoredFormations:  models.FormationPage{Items: []models.Formation{}},
		Capabilities:        Capabilities{Learning: canLearn, FormationAuthoring: canViewAuthoring && auth.HasOAuthScope(r.Context(), models.OAuthScopeFormationWrite)},
	}
	err = api.store.WithTenantReadTx(r.Context(), actor.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		if err := scoped.ValidatePrincipal(ctx, actor); err != nil {
			return err
		}
		// Staff identities need no synthetic learner profile. Their formation
		// authoring operates through the permission-checked MCP tools.
		if !canLearn {
			return nil
		}
		overview, err := scoped.GetLearnerContextOverview(ctx, actor.LearnerID, time.Now().UTC())
		if err != nil {
			return err
		}
		result.Context = projectContext(overview)
		if formations, ok := scoped.(storeport.LearnerFormationStore); ok {
			result.Formations, err = formations.ListLearnerFormations(ctx, actor, true, params.after, params.limit)
			if err != nil {
				return err
			}
			result.AvailableFormations, err = formations.ListLearnerFormations(ctx, actor, false, params.availableAfter, params.limit)
			result.Capabilities.CommonCatalog = err == nil
			return err
		}
		return nil
	})
	if err == nil && canViewAuthoring {
		if catalogue, ok := api.store.(interface {
			ListFormations(context.Context, models.Principal, string, int) (models.FormationPage, error)
		}); ok {
			// The catalogue validates the current principal again inside its
			// own tenant transaction and applies owner/assignment visibility.
			result.AuthoredFormations, err = catalogue.ListFormations(r.Context(), actor, params.authoredAfter, params.limit)
			if result.AuthoredFormations.Items == nil {
				result.AuthoredFormations.Items = []models.Formation{}
			}
		}
	}
	if errors.Is(err, storeport.ErrInvalidPrincipal) {
		writeError(w, http.StatusUnauthorized, "authentication_required")
		return
	}
	if err != nil {
		api.logger.Warn("native client overview unavailable")
		writeError(w, http.StatusInternalServerError, "overview_unavailable")
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		api.logger.Warn("native client overview encoding failed")
		writeError(w, http.StatusInternalServerError, "overview_unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func projectContext(source *storeport.LearnerContextOverview) Context {
	result := Context{
		Learner: Learner{ID: source.Learner.ID, Objective: source.Learner.Objective, CreatedAt: source.Learner.CreatedAt, LastActive: source.Learner.LastActive},
		Domains: []Domain{}, ConceptStates: []ConceptState{}, TodayInteractions: []InteractionCount{},
	}
	// Only the declared preference schema is copied. Arbitrary historical
	// profile fields, notes and integrations cannot hitchhike into the client.
	_ = json.Unmarshal([]byte(source.Learner.ProfileJSON), &result.Learner.Profile)
	for _, domain := range source.Domains {
		result.Domains = append(result.Domains, Domain{ID: domain.ID, Name: domain.Name, Archived: domain.Archived, HighStakes: domain.HighStakes, PriorityRank: domain.PriorityRank, ConceptCount: len(domain.Graph.Concepts), CreatedAt: domain.CreatedAt})
	}
	for _, state := range source.ConceptStates {
		result.ConceptStates = append(result.ConceptStates, ConceptState{DomainID: state.DomainID, Concept: state.Concept, Stability: state.Stability, CardState: state.CardState, LastReview: state.LastReview, PMastery: state.PMastery})
	}
	for _, count := range source.TodayInteractions {
		result.TodayInteractions = append(result.TodayInteractions, InteractionCount{DomainID: count.DomainID, Concept: count.Concept, Count: count.Count})
	}
	return result
}

type pageParams struct {
	after, availableAfter, authoredAfter string
	limit                                int
}

func parsePageParams(raw string) (pageParams, error) {
	result := pageParams{limit: 20}
	if len(raw) > 2048 {
		return result, errors.New("query too long")
	}
	query, err := url.ParseQuery(raw)
	if err != nil {
		return result, err
	}
	for key, values := range query {
		if len(values) != 1 || (key != "after" && key != "available_after" && key != "authored_after" && key != "limit") {
			return result, errors.New("unknown or repeated parameter")
		}
		value := values[0]
		if key == "limit" {
			result.limit, err = strconv.Atoi(value)
			if err != nil || result.limit < 1 || result.limit > 100 {
				return result, errors.New("invalid limit")
			}
			continue
		}
		if len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return result, errors.New("invalid cursor")
		}
		if key == "after" {
			result.after = value
		} else if key == "available_after" {
			result.availableAfter = value
		} else {
			result.authoredAfter = value
		}
	}
	return result, nil
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
