// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func pedagogicalTurnFixture(t *testing.T) (*db.Store, *Deps, context.Context, models.Principal, *models.Domain, *models.ConversationContext) {
	t.Helper()
	var s *db.Store
	var deps *Deps
	if dsn := os.Getenv("TUTOR_TEST_PG_DSN"); dsn != "" {
		t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
		s = postgresToolsTestStore(t, dsn)
		deps = &Deps{Store: s, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		for _, id := range []string{"L_owner", "L_attacker"} {
			if _, err := s.RawDB().Exec(`INSERT INTO learners (id,email,password_hash,objective,created_at,email_verified_at) VALUES ($1,$2,'hash','test',$3,$3)`, id, id+"@test.com", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		s, deps = setupToolsTest(t)
	}
	p, err := s.GetPrincipalForLearner(t.Context(), "L_owner", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := auth.WithPrincipal(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx = auth.WithOAuthScope(ctx, models.OAuthScopeLearner)
	domain, err := s.CreateDomain(ctx, p.LearnerID, "Reasoning", "Understand causes", models.KnowledgeSpace{Concepts: []string{"premise", "inference"}, Prerequisites: map[string][]string{"inference": {"premise"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, concept := range domain.Graph.Concepts {
		if err := s.InsertConceptStateIfNotExists(ctx, models.NewConceptStateInDomain(p.LearnerID, domain.ID, concept)); err != nil {
			t.Fatal(err)
		}
	}
	zero := 0
	selected, err := s.TrainingAction(ctx, p, "select_conversation_training", models.TrainingRequest{ID: "personal:" + domain.ID, ConversationID: "turn-conversation", ExpectedSelection: &zero, Key: "select-turn"})
	if err != nil {
		t.Fatal(err)
	}
	return s, deps, ctx, p, domain, selected.(*models.ConversationContext)
}

func TestPedagogicalTurnContextIsReadOnlyAndVersionBound(t *testing.T) {
	s, deps, ctx, p, domain, selected := pedagogicalTurnFixture(t)
	before, err := s.GetConceptStatesByDomain(ctx, p.LearnerID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out *models.PedagogicalTurn
	err = s.WithTenantReadTx(ctx, p.TenantScope(), func(txctx context.Context, scoped storeport.Store) error {
		copy := *deps
		copy.Store = scoped
		var inner error
		out, inner = preparePedagogicalTurn(txctx, &copy, PreparePedagogicalTurnParams{ConversationID: selected.ConversationID, ExpectedSelection: &selected.SelectionVersion, ExpectedRevision: selected.Revision})
		return inner
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.SchemaVersion != models.PedagogicalTurnSchemaVersion || out.Mode != "context" || out.Session != nil || out.Decision != nil || out.Provenance.ModelUpdated || !reflect.DeepEqual(out.Conversation, selected) {
		t.Fatalf("context = %#v", out)
	}
	if out.Curriculum.Version != domain.GraphVersion || len(out.Curriculum.Concepts) != 2 || !reflect.DeepEqual(out.Curriculum.Prerequisites["inference"], []string{"premise"}) {
		t.Fatalf("curriculum = %#v", out.Curriculum)
	}
	if _, err := s.GetActiveLearningSession(ctx, p.LearnerID); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("context created session: %v", err)
	}
	after, err := s.GetConceptStatesByDomain(ctx, p.LearnerID, domain.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("context changed learner model: %v", err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "password") || strings.Contains(string(raw), "@test.com") {
		t.Fatal("private account fields leaked")
	}
	stale := selected.SelectionVersion + 1
	if _, err := preparePedagogicalTurn(ctx, deps, PreparePedagogicalTurnParams{ConversationID: selected.ConversationID, ExpectedSelection: &stale}); !errors.Is(err, storeport.ErrCurriculumVersionConflict) {
		t.Fatalf("stale selection = %v", err)
	}
	if _, err := preparePedagogicalTurn(ctx, deps, PreparePedagogicalTurnParams{ConversationID: selected.ConversationID, ExpectedRevision: "999"}); !errors.Is(err, storeport.ErrCurriculumVersionConflict) {
		t.Fatalf("stale curriculum = %v", err)
	}
}

func TestPedagogicalTurnSelectionAndPrincipalIsolation(t *testing.T) {
	_, deps, ctx, p, _, selected := pedagogicalTurnFixture(t)
	for _, test := range []struct {
		name      string
		principal models.Principal
		in        PreparePedagogicalTurnParams
		wantError bool
	}{
		{"read-only", func() models.Principal { copy := p; copy.Scopes = []string{models.OAuthScopeLearnerRead}; return copy }(), PreparePedagogicalTurnParams{ConversationID: selected.ConversationID}, false},
		{"read-only activity", func() models.Principal { copy := p; copy.Scopes = []string{models.OAuthScopeLearnerRead}; return copy }(), PreparePedagogicalTurnParams{ConversationID: selected.ConversationID, Mode: "activity", IdempotentMutationParams: IdempotentMutationParams{IdempotencyKey: "one"}}, true},
		{"institution without learner", func() models.Principal {
			copy := p
			copy.LearnerID = ""
			copy.Roles = []string{models.RoleOwner}
			copy.Scopes = []string{models.OAuthScopeFormationRead}
			return copy
		}(), PreparePedagogicalTurnParams{ConversationID: selected.ConversationID}, true},
		{"other learner", models.LegacyPrincipal("L_attacker"), PreparePedagogicalTurnParams{ConversationID: selected.ConversationID}, false},
		{"no conversation resolves the domain", p, PreparePedagogicalTurnParams{}, false},
		{"activity without key", p, PreparePedagogicalTurnParams{ConversationID: selected.ConversationID, Mode: "activity"}, false},
		{"selection without key", p, PreparePedagogicalTurnParams{ConversationID: selected.ConversationID, Mode: "activity", TrainingName: "Go"}, true},
		{"domain override of a conversation", p, PreparePedagogicalTurnParams{ConversationID: selected.ConversationID, DomainName: "Go"}, true},
		{"unselected activity", p, PreparePedagogicalTurnParams{ConversationID: "unselected", Mode: "activity", IdempotentMutationParams: IdempotentMutationParams{IdempotencyKey: "one"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			other, err := auth.WithPrincipal(ctx, test.principal)
			if err != nil {
				t.Fatal(err)
			}
			out, err := preparePedagogicalTurn(other, deps, test.in)
			if (err != nil) != test.wantError {
				t.Fatalf("out=%#v err=%v", out, err)
			}
			if test.name == "other learner" && (!out.NeedsSelection || out.Curriculum != nil || out.Conversation.DomainID != "") {
				t.Fatal("another learner borrowed selection")
			}
			if test.name == "unselected activity" && (!out.NeedsSelection || out.SelectionReason != "missing" || out.Decision != nil || out.Session != nil) {
				t.Fatal("unselected activity guessed a training or opened a session")
			}
			if test.name == "read-only" && out.Permissions.PrepareActivity {
				t.Fatal("write grant invented")
			}
		})
	}
	if _, err := preparePedagogicalTurn(storeport.WithConversation(ctx, "bound-other"), deps, PreparePedagogicalTurnParams{ConversationID: selected.ConversationID}); err == nil {
		t.Fatal("bound conversation bypassed")
	}
}

func TestPedagogicalTurnCanonicalActivityIsIdempotent(t *testing.T) {
	s, deps, ctx, p, domain, selected := pedagogicalTurnFixture(t)
	before, _ := s.GetConceptStatesByDomain(ctx, p.LearnerID, domain.ID)
	register := func(server *mcp.Server, deps *Deps) {
		addIdempotencyMiddleware(server, deps)
		registerPreparePedagogicalTurn(server, deps)
	}
	in := map[string]any{"conversation_id": selected.ConversationID, "mode": "activity", "expected_selection": selected.SelectionVersion, "expected_revision": selected.Revision, "idempotency_key": "prepared-once"}
	one := callTool(t, deps, register, p.LearnerID, "prepare_pedagogical_turn", in)
	if one.IsError {
		t.Fatal(resultText(one))
	}
	two := callTool(t, deps, register, p.LearnerID, "prepare_pedagogical_turn", in)
	if two.IsError || resultText(one) != resultText(two) {
		t.Fatalf("retry changed decision: %s / %s", resultText(one), resultText(two))
	}
	var out models.PedagogicalTurn
	if err := json.Unmarshal([]byte(resultText(one)), &out); err != nil {
		t.Fatal(err)
	}
	contract, ok := out.Decision["pedagogical_contract"].(map[string]any)
	if !ok || contract["decision_id"] == "" || out.Decision["decision_id"] != nil || contract["policy_version"] != models.PedagogicalPolicyVersion || out.Session == nil || out.Session.ID != out.Decision["session_id"] {
		t.Fatalf("canonical contract=%#v", out)
	}
	if _, ok := out.Decision["consolidation_request"]; ok {
		t.Fatal("foreground maintenance returned")
	}
	var count int
	if err := s.RawDB().QueryRow("SELECT COUNT(*) FROM pedagogical_decisions WHERE learner_id=$1", p.LearnerID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("decisions=%d err=%v", count, err)
	}
	after, _ := s.GetConceptStatesByDomain(ctx, p.LearnerID, domain.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("preparing activity changed mastery")
	}
}

func TestPedagogicalTurnCurriculumReductionPreservesClosureAndCriteria(t *testing.T) {
	snapshot := &models.CurriculumSnapshot{DomainID: "domain", Version: 7, Graph: models.KnowledgeSpace{Prerequisites: map[string][]string{}}}
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("c%02d", i)
		snapshot.Graph.Concepts = append(snapshot.Graph.Concepts, key)
		snapshot.Concepts = append(snapshot.Concepts, models.CurriculumConcept{ID: key, Key: key, Description: strings.Repeat("criterion must stay whole ", 50), Criteria: []models.CurriculumCriterion{{ID: "proof", Description: "all steps required"}}, Outcomes: []models.CurriculumOutcome{{ID: "explain", Statement: "explain each step"}}})
	}
	snapshot.Graph.Prerequisites["c39"] = []string{"c38"}
	snapshot.Graph.Prerequisites["c38"] = []string{"c37"}
	out, reduction, err := pedagogicalTurnCurriculum(snapshot, true, "c39", "canonical_engine")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Concepts) != 32 || reduction == nil || reduction.Available != 40 || reduction.Returned != 32 || !out.HighStakes {
		t.Fatalf("reduction=%#v", reduction)
	}
	seen := map[string]bool{}
	for _, concept := range out.Concepts {
		seen[concept.Key] = true
		for _, original := range snapshot.Concepts {
			if original.Key == concept.Key && !reflect.DeepEqual(original, concept) {
				t.Fatal("competency shortened")
			}
		}
	}
	for _, key := range []string{"c39", "c38", "c37"} {
		if !seen[key] {
			t.Fatalf("lost required prerequisite %s", key)
		}
	}
	for i := 1; i < 40; i++ {
		snapshot.Graph.Prerequisites[fmt.Sprintf("c%02d", i)] = []string{fmt.Sprintf("c%02d", i-1)}
	}
	if _, _, err := pedagogicalTurnCurriculum(snapshot, false, "c39", "attempt"); err == nil {
		t.Fatal("oversized mandatory closure silently truncated")
	}
}

func TestPedagogicalTurnAssessmentAndBudgetRemainImmutable(t *testing.T) {
	s, deps, ctx, p, domain, selected := pedagogicalTurnFixture(t)
	attemptID := seedEvaluatedAssessmentFixture(t, s, p.LearnerID, domain.ID, "premise", models.ActivityDiagnosticAssessment, true, time.Now().UTC(), "")
	before, err := s.GetAssessmentAttempt(ctx, p.LearnerID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := preparePedagogicalTurn(ctx, deps, PreparePedagogicalTurnParams{ConversationID: selected.ConversationID, AttemptID: attemptID})
	if err != nil {
		t.Fatal(err)
	}
	if out.Attempt.TaskText != before.TaskText || string(out.Attempt.Rubric) != before.RubricJSON || out.Attempt.ResponseText != before.ResponseText || out.Attempt.Status != before.Status {
		t.Fatal("frozen assessment changed")
	}
	after, _ := s.GetAssessmentAttempt(ctx, p.LearnerID, attemptID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("context mutated assessment")
	}
	out.Attempt.TaskText = strings.Repeat("x", pedagogicalTurnBudgetBytes)
	if _, err := boundedPedagogicalTurn(out); err == nil || len(out.Attempt.TaskText) != pedagogicalTurnBudgetBytes {
		t.Fatal("oversized attempt silently truncated")
	}
	other, err := s.CreateDomain(ctx, p.LearnerID, "Other", "", models.KnowledgeSpace{Concepts: []string{"premise"}, Prerequisites: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	foreign := seedEvaluatedAssessmentFixture(t, s, p.LearnerID, other.ID, "premise", models.ActivityDiagnosticAssessment, true, time.Now().UTC(), "")
	if _, err := preparePedagogicalTurn(ctx, deps, PreparePedagogicalTurnParams{ConversationID: selected.ConversationID, AttemptID: foreign}); err == nil {
		t.Fatal("cross-domain assessment leaked")
	}
}
