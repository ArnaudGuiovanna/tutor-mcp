// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"tutor-mcp/models"
)

func formationGlobalAccess(actor models.Principal) int {
	for _, role := range actor.Roles {
		if role == models.RoleOwner || role == models.RoleAdmin {
			return 1
		}
	}
	return 0
}

// alias is a closed caller-owned SQL identifier; identities remain bound values.
func formationVisibilitySQL(_ models.Principal, alias string) string {
	return `(? = 1 OR ` + alias + `.owner_membership_id = ? OR EXISTS (SELECT 1 FROM formation_trainers ft WHERE ft.tenant_id = ` + alias + `.tenant_id AND ft.formation_id = ` + alias + `.id AND ft.membership_id = ?))`
}

var catalogAcronymBoundary = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
var catalogWordBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// Retain pre-upgrade idempotency receipts when public JSON becomes snake_case.
// Never delete a receipt and thereby execute an already committed write again.
func decodeCatalogReplay(raw string, target any) error {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return err
	}
	var normalize func(any) any
	normalize = func(value any) any {
		switch v := value.(type) {
		case map[string]any:
			out := map[string]any{}
			for key, item := range v {
				key = strings.ToLower(catalogWordBoundary.ReplaceAllString(catalogAcronymBoundary.ReplaceAllString(key, "${1}_${2}"), "${1}_${2}"))
				out[key] = normalize(item)
			}
			return out
		case []any:
			for i := range v {
				v[i] = normalize(v[i])
			}
			return v
		default:
			return value
		}
	}
	encoded, err := json.Marshal(normalize(value))
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func (s *Store) authorizeCatalogReplay(ctx context.Context, actor models.Principal, operation string, request any, permission models.Permission) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	var resource struct {
		VersionID   string `json:"version_id"`
		FormationID string `json:"formation_id"`
		CohortID    string `json:"cohort_id"`
	}
	if err := decodeCatalogReplay(string(raw), &resource); err != nil {
		return err
	}
	kind, id := "", ""
	switch {
	case resource.VersionID != "":
		kind, id = "version", resource.VersionID
	case resource.FormationID != "":
		kind, id = "formation", resource.FormationID
	case resource.CohortID != "":
		kind, id = "cohort", resource.CohortID
	default:
		return nil // creation has no existing resource
	}
	_, err = s.formationAccess(ctx, actor, kind, id, permission, true)
	return err
}

func (s *Store) CloneFormationVersionIdempotent(ctx context.Context, actor models.Principal, key, versionID string) (*models.FormationVersionDetail, bool, error) {
	return runCatalogMutation(ctx, s, actor, key, "formation.clone", models.PermissionFormationWrite, struct{ VersionID string }{versionID}, func(txCtx context.Context, _ *Store) (*models.FormationVersionDetail, error) {
		return s.CloneFormationVersion(txCtx, actor, versionID)
	})
}

func (s *Store) ReplaceFormationContentIdempotent(ctx context.Context, actor models.Principal, key, versionID string, modules []models.FormationModuleInput, concepts []models.FormationConceptInput) (*models.FormationVersionDetail, bool, error) {
	request := struct {
		VersionID string
		Modules   []models.FormationModuleInput
		Concepts  []models.FormationConceptInput
	}{versionID, modules, concepts}
	return runCatalogMutation(ctx, s, actor, key, "formation.content.replace", models.PermissionFormationWrite, request, func(txCtx context.Context, _ *Store) (*models.FormationVersionDetail, error) {
		if err := s.ReplaceFormationContent(txCtx, actor, versionID, modules, concepts); err != nil {
			return nil, err
		}
		return s.GetFormationVersion(txCtx, actor, versionID)
	})
}

func (s *Store) AddFormationConceptsIdempotent(ctx context.Context, actor models.Principal, key, versionID string, modules []models.FormationModuleInput, concepts []models.FormationConceptInput) (*models.FormationVersionDetail, bool, error) {
	moduleKeys, conceptKeys := map[string]bool{}, map[string]bool{}
	for _, module := range modules {
		if moduleKeys[module.StableKey] {
			return nil, false, fmt.Errorf("duplicate module key")
		}
		moduleKeys[module.StableKey] = true
	}
	for _, concept := range concepts {
		if conceptKeys[concept.StableKey] {
			return nil, false, fmt.Errorf("duplicate concept key")
		}
		conceptKeys[concept.StableKey] = true
	}
	request := struct {
		VersionID string
		Modules   []models.FormationModuleInput
		Concepts  []models.FormationConceptInput
	}{versionID, modules, concepts}
	return runCatalogMutation(ctx, s, actor, key, "formation.concepts.upsert", models.PermissionFormationWrite, request, func(txCtx context.Context, _ *Store) (*models.FormationVersionDetail, error) {
		d, err := s.GetFormationVersion(txCtx, actor, versionID)
		if err != nil {
			return nil, err
		}
		mergedModules := []models.FormationModuleInput{}
		moduleIndex := map[string]int{}
		for _, m := range d.Modules {
			moduleIndex[m.StableKey] = len(mergedModules)
			mergedModules = append(mergedModules, m.FormationModuleInput)
		}
		for _, m := range modules {
			if i, ok := moduleIndex[m.StableKey]; ok {
				mergedModules[i] = m
			} else {
				moduleIndex[m.StableKey] = len(mergedModules)
				mergedModules = append(mergedModules, m)
			}
		}
		if len(mergedModules) == 0 {
			mergedModules = append(mergedModules, models.FormationModuleInput{StableKey: "main", Title: "Course"})
		}
		mergedConcepts := []models.FormationConceptInput{}
		conceptIndex := map[string]int{}
		for _, c := range d.Concepts {
			conceptIndex[c.StableKey] = len(mergedConcepts)
			mergedConcepts = append(mergedConcepts, c.FormationConceptInput)
		}
		for _, c := range concepts {
			if c.ModuleStableKey == "" {
				c.ModuleStableKey = mergedModules[0].StableKey
			}
			if i, ok := conceptIndex[c.StableKey]; ok {
				mergedConcepts[i] = c
			} else {
				conceptIndex[c.StableKey] = len(mergedConcepts)
				mergedConcepts = append(mergedConcepts, c)
			}
		}
		if err := s.ReplaceFormationContent(txCtx, actor, versionID, mergedModules, mergedConcepts); err != nil {
			return nil, err
		}
		return s.GetFormationVersion(txCtx, actor, versionID)
	})
}
