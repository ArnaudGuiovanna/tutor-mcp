// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package tools

import (
	"encoding/json"
	"testing"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestConversationContextPreventsImplicitCrossTrainingRouting(t *testing.T) {
	s, _ := setupToolsTest(t)
	p, err := s.GetPrincipalForLearner(t.Context(), "L_owner", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := auth.WithPrincipal(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	goDomain, err := s.CreateDomain(ctx, p.LearnerID, "Go", "", models.KnowledgeSpace{Concepts: []string{"variables"}, Prerequisites: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	python, err := s.CreateDomain(ctx, p.LearnerID, "Python", "", models.KnowledgeSpace{Concepts: []string{"variables"}, Prerequisites: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	bound := storeport.WithConversation(ctx, "explicit-conversation")
	if _, err = resolveDomain(bound, s, p.LearnerID, ""); err == nil {
		t.Fatal("unselected conversation routed to latest domain")
	}
	selection := 0
	if _, err = s.TrainingAction(ctx, p, "select_conversation_training", models.TrainingRequest{ID: "personal:" + goDomain.ID, ConversationID: "explicit-conversation", ExpectedSelection: &selection, Key: "select-go"}); err != nil {
		t.Fatal(err)
	}
	selected, err := resolveActivityDomain(bound, s, p.LearnerID, "", "")
	if err != nil || selected.ID != goDomain.ID {
		t.Fatal("activity routed to latest domain", err)
	}
	if _, err = resolveDomain(bound, s, p.LearnerID, python.ID); err == nil {
		t.Fatal("explicit wrong domain bypassed conversation selection")
	}
	different := storeport.WithConversation(ctx, "another-conversation")
	if _, err = resolveDomain(different, s, p.LearnerID, goDomain.ID); err == nil {
		t.Fatal("another conversation borrowed the first selection")
	}
}

func TestInitDomainStoresInitialDefinitionsAndSelectsTheConversation(t *testing.T) {
	_, deps := setupToolsTest(t)
	result := callTool(t, deps, RegisterTools, "L_owner", "init_domain", map[string]any{"name": "Go immédiat", "concepts": []string{"variables"}, "prerequisites": map[string][]string{}, "details": map[string]any{"variables": map[string]any{"label": "variables", "description": "Déclarer et utiliser une valeur", "level": "foundation", "outcomes": []map[string]string{{"statement": "Déclarer une variable"}}, "criteria": []map[string]string{{"description": "Le code compile"}}}}, "idempotency_key": "initial-details"})
	if result.IsError {
		t.Fatal(resultText(result))
	}
	var created struct {
		DomainID string `json:"domain_id"`
		Version  int    `json:"curriculum_version"`
	}
	if err := json.Unmarshal([]byte(resultText(result)), &created); err != nil {
		t.Fatal(err)
	}
	snapshot, err := deps.Store.GetCurriculumSnapshot(t.Context(), "L_owner", created.DomainID, 0)
	if err != nil || snapshot.Concepts[0].Description != "Déclarer et utiliser une valeur" || len(snapshot.Concepts[0].Outcomes) != 1 || created.Version != snapshot.Version {
		t.Fatalf("initial metadata: %#v %v", snapshot, err)
	}
}
