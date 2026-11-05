// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/models"
)

func registerTransactionalPedagogicalTurn(server *mcp.Server, deps *Deps) {
	addIdempotencyMiddleware(server, deps)
	server.AddReceivingMiddleware(toolTenantTransactionMiddleware(deps))
	registerPreparePedagogicalTurn(server, deps)
}

func TestPedagogicalTurnSelectsBeforeDecisionAndReplaysWholeTurn(t *testing.T) {
	s, deps, ctx, p, old, selected := pedagogicalTurnFixture(t)
	target, err := s.CreateDomain(ctx, p.LearnerID, "Harness", "Understand the harness lifecycle", models.KnowledgeSpace{Concepts: []string{"lifecycle"}, Prerequisites: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertConceptStateIfNotExists(ctx, models.NewConceptStateInDomain(p.LearnerID, target.ID, "lifecycle")); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetConceptStatesByDomain(ctx, p.LearnerID, target.ID)
	in := map[string]any{"conversation_id": selected.ConversationID, "mode": "activity", "training_name": " harness ", "expected_selection": selected.SelectionVersion, "expected_revision": selected.Revision, "idempotency_key": "resume-whole-turn"}
	one := callTool(t, deps, registerTransactionalPedagogicalTurn, p.LearnerID, "prepare_pedagogical_turn", in)
	if one.IsError {
		t.Fatal(resultText(one))
	}
	var out models.PedagogicalTurn
	if err := json.Unmarshal([]byte(resultText(one)), &out); err != nil {
		t.Fatal(err)
	}
	if out.NeedsSelection || out.Training == nil || out.Training.ID != "personal:"+target.ID || out.Training.Description != target.PersonalGoal || out.Conversation.SelectionVersion != selected.SelectionVersion+1 || out.Curriculum.DomainID != target.ID || out.Activity["domain_id"] != target.ID || out.Session == nil || out.Session.DomainID != target.ID {
		t.Fatalf("resume did not bind target before decision: %#v", out)
	}
	if out.Presentation == "" || out.PresentationReason == "" {
		t.Fatal("no server presentation contract")
	}
	two := callTool(t, deps, registerTransactionalPedagogicalTurn, p.LearnerID, "prepare_pedagogical_turn", in)
	if two.IsError || resultText(one) != resultText(two) {
		t.Fatalf("retry changed whole turn: %s", resultText(two))
	}
	var decisions, oldDecisions int
	if err := s.RawDB().QueryRow("SELECT COUNT(*) FROM pedagogical_decisions WHERE learner_id=$1", p.LearnerID).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if err := s.RawDB().QueryRow("SELECT COUNT(*) FROM pedagogical_decisions WHERE learner_id=$1 AND domain_id=$2", p.LearnerID, old.ID).Scan(&oldDecisions); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 || oldDecisions != 0 {
		t.Fatalf("decisions=%d old=%d", decisions, oldDecisions)
	}
	after, _ := s.GetConceptStatesByDomain(ctx, p.LearnerID, target.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("resume created learner progress")
	}
	in["idempotency_key"] = "stale-resume"
	if res := callTool(t, deps, registerTransactionalPedagogicalTurn, p.LearnerID, "prepare_pedagogical_turn", in); !res.IsError {
		t.Fatal("stale pre-selection version accepted")
	}
}

func TestPedagogicalTurnSelectionRollsBackOnInvalidAttempt(t *testing.T) {
	s, deps, ctx, p, domain, selected := pedagogicalTurnFixture(t)
	in := map[string]any{"conversation_id": "rollback-resume", "mode": "activity", "training_id": "personal:" + domain.ID, "expected_selection": 0, "attempt_id": "missing-attempt", "idempotency_key": "rollback-resume"}
	res := callTool(t, deps, registerTransactionalPedagogicalTurn, p.LearnerID, "prepare_pedagogical_turn", in)
	if !res.IsError {
		t.Fatal("invalid assessment accepted")
	}
	current, err := s.TrainingAction(ctx, p, "get_conversation_context", models.TrainingRequest{ConversationID: "rollback-resume"})
	if err != nil || current.(*models.ConversationContext).SelectionVersion != 0 {
		t.Fatalf("partial selection survived: %#v %v", current, err)
	}
	for _, table := range []string{"pedagogical_decisions", "learning_sessions", "tool_call_idempotency"} {
		var count int
		if err := s.RawDB().QueryRow("SELECT COUNT(*) FROM "+table+" WHERE learner_id=$1", p.LearnerID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s=%d err=%v", table, count, err)
		}
	}
	// A corrected request with the same key is possible after an atomic failure.
	delete(in, "attempt_id")
	if res := callTool(t, deps, registerTransactionalPedagogicalTurn, p.LearnerID, "prepare_pedagogical_turn", in); res.IsError {
		t.Fatal(resultText(res))
	}
	unchanged, err := s.TrainingAction(ctx, p, "get_conversation_context", models.TrainingRequest{ConversationID: selected.ConversationID})
	if err != nil || !reflect.DeepEqual(unchanged, selected) {
		t.Fatal("another conversation changed")
	}
}

func TestPedagogicalTurnAmbiguityAndMissingTargetDoNotSelect(t *testing.T) {
	s, deps, ctx, p, _, selected := pedagogicalTurnFixture(t)
	for i := 0; i < 2; i++ {
		if _, err := s.CreateDomain(ctx, p.LearnerID, "Same name", "", models.KnowledgeSpace{Concepts: []string{"one"}, Prerequisites: map[string][]string{}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ name, reason string }{{"Same name", "ambiguous"}, {"unknown", "not_found"}, {"", "missing"}} {
		in := map[string]any{"conversation_id": "resume-choice", "mode": "activity", "idempotency_key": "choice-" + test.reason}
		if test.name != "" {
			in["training_name"] = test.name
		}
		res := callTool(t, deps, registerTransactionalPedagogicalTurn, p.LearnerID, "prepare_pedagogical_turn", in)
		if res.IsError {
			t.Fatal(resultText(res))
		}
		var out models.PedagogicalTurn
		if err := json.Unmarshal([]byte(resultText(res)), &out); err != nil {
			t.Fatal(err)
		}
		if !out.NeedsSelection || out.SelectionReason != test.reason || out.Conversation.SelectionVersion != 0 || out.Session != nil || out.Activity != nil || out.Training != nil {
			t.Fatalf("choice changed pedagogy: %#v", out)
		}
		if test.reason == "ambiguous" && len(out.TrainingChoices) != 2 {
			t.Fatal("lost homonymous candidates")
		}
	}
	for _, in := range []map[string]any{
		{"conversation_id": selected.ConversationID, "training_id": selected.TrainingID},
		{"conversation_id": selected.ConversationID, "mode": "activity", "idempotency_key": "two-targets", "training_id": selected.TrainingID, "training_name": selected.Name},
		{"conversation_id": selected.ConversationID, "mode": "activity", "idempotency_key": "blank-name", "training_name": "  "},
	} {
		if res := callTool(t, deps, registerTransactionalPedagogicalTurn, p.LearnerID, "prepare_pedagogical_turn", in); !res.IsError {
			t.Fatal("invalid selector accepted")
		}
	}
}

func TestPedagogicalTurnPresentationPreservesCanonicalActivities(t *testing.T) {
	for _, test := range []struct {
		activity models.ActivityType
		want     string
	}{
		{models.ActivityNewConcept, "instruction"},
		{models.ActivityRecall, "assessment"}, {models.ActivityDiagnosticAssessment, "assessment"},
		{models.ActivityPractice, "assessment"}, {models.ActivityMasteryChallenge, "assessment"},
		{models.ActivityDebuggingCase, "assessment"}, {models.ActivityDebugMisconception, "assessment"},
		{models.ActivityFeynmanPrompt, "assessment"}, {models.ActivityTransferProbe, "assessment"},
		{models.ActivityRest, "guidance"}, {models.ActivityCloseSession, "guidance"}, {models.ActivitySetupDomain, "guidance"},
	} {
		presentation, reason := pedagogicalTurnPresentation(test.activity)
		if presentation != test.want || reason == "" {
			t.Fatalf("%s: %s %s", test.activity, presentation, reason)
		}
	}
}

func TestPedagogicalTurnBundledMutationHonorsRevokedToolPermission(t *testing.T) {
	_, deps, ctx, _, domain, selected := pedagogicalTurnFixture(t)
	for _, test := range []struct {
		name, policy, mode, target string
		denied                     bool
	}{
		{"selection denied", `{"default":"allow","tools":{"select_conversation_training":"deny"}}`, "activity", "personal:" + domain.ID, true},
		{"decision denied", `{"default":"allow","tools":{"get_next_activity":"deny"}}`, "activity", "", true},
		{"context remains read only", `{"default":"allow","tools":{"select_conversation_training":"deny","get_next_activity":"deny"}}`, "context", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, err := parseToolPolicy(test.policy)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(ctx, toolPolicyKey{}, policy)
			in := PreparePedagogicalTurnParams{ConversationID: selected.ConversationID, Mode: test.mode, TrainingID: test.target, IdempotentMutationParams: IdempotentMutationParams{IdempotencyKey: "policy-resume"}}
			raw, _ := json.Marshal(in)
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "prepare_pedagogical_turn", Arguments: raw}}
			executed := false
			handler := ToolPolicyMiddleware()(func(context.Context, string, mcp.Request) (mcp.Result, error) {
				executed = true
				return jsonResult(map[string]bool{"reached_replay": true})
			})
			res, err := handler(ctx, "tools/call", req)
			if err != nil || res.(*mcp.CallToolResult).IsError != test.denied || executed == test.denied {
				t.Fatalf("policy execution=%v result=%v err=%v", executed, res, err)
			}
			if _, err := preparePedagogicalTurn(ctx, deps, in); (err != nil) != test.denied {
				t.Fatalf("direct handler bypassed policy: %v", err)
			}
		})
	}
}

func TestPedagogicalTurnConcurrentSelectionHasOneDecision(t *testing.T) {
	s, deps, ctx, p, first, _ := pedagogicalTurnFixture(t)
	second, err := s.CreateDomain(ctx, p.LearnerID, "Second course", "", models.KnowledgeSpace{Concepts: []string{"other"}, Prerequisites: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertConceptStateIfNotExists(ctx, models.NewConceptStateInDomain(p.LearnerID, second.ID, "other")); err != nil {
		t.Fatal(err)
	}
	handler := toolTenantTransactionMiddleware(deps)(idempotencyMiddleware(deps)(func(ctx context.Context, _ string, req mcp.Request) (mcp.Result, error) {
		var in PreparePedagogicalTurnParams
		if err := json.Unmarshal(req.(*mcp.CallToolRequest).Params.Arguments, &in); err != nil {
			return errorResult(err.Error())
		}
		out, err := preparePedagogicalTurn(ctx, deps, in)
		if err != nil {
			return errorResult(err.Error())
		}
		return jsonResult(out)
	}))
	type answer struct {
		result mcp.Result
		err    error
	}
	start := make(chan struct{})
	answers := make(chan answer, 2)
	for _, domain := range []string{first.ID, second.ID} {
		raw, _ := json.Marshal(map[string]any{"conversation_id": "concurrent-resume", "mode": "activity", "training_id": "personal:" + domain, "expected_selection": 0, "idempotency_key": "concurrent-" + domain})
		request := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "prepare_pedagogical_turn", Arguments: raw}}
		go func() {
			<-start
			result, err := handler(ctx, "tools/call", request)
			answers <- answer{result, err}
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		answer := <-answers
		if answer.err != nil {
			t.Fatal(answer.err)
		}
		if !answer.result.(*mcp.CallToolResult).IsError {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent choices produced %d successful decisions", successes)
	}
	var count int
	if err := s.RawDB().QueryRow("SELECT COUNT(*) FROM pedagogical_decisions WHERE learner_id=$1", p.LearnerID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("decision count=%d %v", count, err)
	}
	current, err := s.TrainingAction(ctx, p, "get_conversation_context", models.TrainingRequest{ConversationID: "concurrent-resume"})
	if err != nil {
		t.Fatal(err)
	}
	selected := current.(*models.ConversationContext)
	session, err := s.GetActiveLearningSession(ctx, p.LearnerID)
	if err != nil || selected.SelectionVersion != 1 || session.DomainID != selected.DomainID {
		t.Fatalf("selection/session split: %#v %#v %v", selected, session, err)
	}
}
