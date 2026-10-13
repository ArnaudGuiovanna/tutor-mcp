// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type parameterPolicyFixture struct {
	s        *db.Store
	deps     *Deps
	postgres bool
}

func (f *parameterPolicyFixture) callTool(t *testing.T, p models.Principal, name string, args any) map[string]any {
	t.Helper()
	register := func(server *mcp.Server, deps *Deps) {
		RegisterTools(server, deps)
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				authCtx, err := auth.WithPrincipal(ctx, p)
				if err != nil {
					return nil, err
				}
				return next(auth.WithOAuthScope(authCtx, models.OAuthScopeLearner), method, req)
			}
		})
	}
	result := callTool(t, f.deps, register, "", name, args)
	if result.IsError {
		t.Fatalf("%s: %s", name, resultText(result))
	}
	return decodeResult(t, result)
}

func runParameterPolicyStores(t *testing.T, run func(*testing.T, *parameterPolicyFixture)) {
	t.Helper()
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := &parameterPolicyFixture{postgres: driver == "postgres"}
			if f.postgres {
				dsn := os.Getenv("TUTOR_TEST_PG_DSN")
				if dsn == "" {
					t.Skip("set TUTOR_TEST_PG_DSN to exercise the PostgreSQL parameter policy")
				}
				t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
				f.s = postgresToolsTestStore(t, dsn)
				f.deps = &Deps{Store: f.s, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			} else {
				f.s, f.deps = setupToolsTest(t)
			}
			run(t, f)
		})
	}
}

func (f *parameterPolicyFixture) sql(query string) string {
	if !f.postgres {
		return query
	}
	index := 0
	var out strings.Builder
	for _, r := range query {
		if r == '?' {
			index++
			fmt.Fprintf(&out, "$%d", index)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func (f *parameterPolicyFixture) row(t *testing.T, query string, args ...any) *sql.Row {
	t.Helper()
	return f.s.RawDB().QueryRowContext(t.Context(), f.sql(query), args...)
}

func (f *parameterPolicyFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.s.RawDB().ExecContext(t.Context(), f.sql(query), args...); err != nil {
		t.Fatal(err)
	}
}

func (f *parameterPolicyFixture) learner(t *testing.T, label string) models.Principal {
	t.Helper()
	l, err := f.s.CreateLearner(t.Context(), label+"-"+rand.Text()+"@parameters.test", "hash", "Learn", "")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE learners SET email_verified_at = ? WHERE id = ?`, time.Now().UTC(), l.ID)
	p, err := f.s.GetPrincipalForLearner(t.Context(), l.ID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *parameterPolicyFixture) formation(t *testing.T) (models.Principal, *models.FormationVersion, *models.Cohort) {
	t.Helper()
	f.deps.Institution = true
	owner := f.learner(t, "owner")
	if _, err := f.s.SetMembershipAuthorization(t.Context(), owner.TenantScope(), models.MembershipStatusActive, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RecordMembershipMFAVerification(t.Context(), owner.TenantScope(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	owner, err := f.s.GetPrincipalForLearner(t.Context(), owner.LearnerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	_, version, err := f.s.CreateFormationDraft(t.Context(), owner, "Parameter policy course", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.AddFormationConceptsIdempotent(t.Context(), owner, "content-"+version.ID, version.ID, nil, []models.FormationConceptInput{{StableKey: "numbers", Label: "Numbers"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PublishFormationVersion(t.Context(), owner, version.ID); err != nil {
		t.Fatal(err)
	}
	cohort, err := f.s.CreateCohort(t.Context(), owner, version.ID, "Calibration", 100, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return owner, version, cohort
}

func (f *parameterPolicyFixture) enroll(t *testing.T, owner models.Principal, cohort *models.Cohort, label string) (models.Principal, *models.Enrollment) {
	t.Helper()
	p := f.learner(t, label)
	e, err := f.s.EnrollMembership(t.Context(), owner, cohort.ID, p.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	return p, e
}

// Freeze the real runtime contract and rubric before committing the response.
// Unlike injected concept states, this creates the durable decision, attempt
// and committed-response event from which calibration may actually learn.
func (f *parameterPolicyFixture) prepare(t *testing.T, p models.Principal, domainID string, activity models.ActivityType, at time.Time, success bool) interactionInput {
	t.Helper()
	curriculum, err := f.s.GetCurriculumSnapshot(t.Context(), p.LearnerID, domainID, 0)
	if err != nil {
		t.Fatal(err)
	}
	competency := curriculum.Concepts[0]
	raw, err := json.Marshal(competency)
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.s.OpenLearningSession(t.Context(), p.LearnerID, domainID, "", at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	id := "parameter-decision-" + rand.Text()
	d := &models.PedagogicalDecision{ID: id, LearnerID: p.LearnerID, DomainID: domainID, SessionID: session.ID, CurriculumVersion: curriculum.Version, PolicyVersion: models.PedagogicalPolicyVersion, CreatedAt: at.Add(-time.Minute), ContextJSON: "{}"}
	d.Contract = models.PedagogicalContract{DecisionID: id, PolicyVersion: d.PolicyVersion, CurriculumVersion: d.CurriculumVersion, TargetConcept: competency.Key, RecommendedActivityType: activity, Competency: &competency, LearningEventProtocol: models.LearningEventProtocol}
	if err := f.s.CreatePedagogicalDecision(t.Context(), p.TenantScope(), d); err != nil {
		t.Fatal(err)
	}
	a := &models.AssessmentAttempt{ID: "attempt-" + id, LearnerID: p.LearnerID, DomainID: domainID, ConceptID: competency.Key, SessionID: session.ID, ActivityID: id, ActivityVersion: 1, ActivityType: string(activity), DecisionID: id, CurriculumVersion: curriculum.Version, CurriculumConceptJSON: string(raw), Observable: "Independent response", TaskText: "Name the larger number: 2 or 4", RubricJSON: `{"criteria":[{"id":"correct","description":"Correct response","max_score":1}],"passing_score":1}`, PassingScore: 1, CreatedAt: at.Add(-time.Minute)}
	if err := f.s.CreateAssessmentAttempt(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	response, score := "4", 1.0
	if !success {
		response, score = "2", 0
	}
	if err := f.s.SubmitAssessmentAttempt(t.Context(), p.LearnerID, a.ID, response, "", at); err != nil {
		t.Fatal(err)
	}
	return interactionInput{DomainID: domainID, SessionID: session.ID, Concept: competency.Key, ActivityType: string(activity), AssessmentAttemptID: a.ID, Success: success, Confidence: .8, ResponseTimeSeconds: 5, AssessmentScore: score, EvaluatorID: "host", EvaluationMethod: models.EvaluationMethodHostLLM, RubricScoreJSON: fmt.Sprintf(`{"criteria_scores":[{"id":"correct","score":%g,"evidence":"Committed learner response."}]}`, score)}
}

func (f *parameterPolicyFixture) apply(t *testing.T, p models.Principal, input interactionInput, at time.Time) (*models.ConceptState, map[string]any, error) {
	t.Helper()
	var state *models.ConceptState
	var observation map[string]any
	err := f.s.WithTenantTx(t.Context(), p.TenantScope(), func(ctx context.Context, _ storeport.Store) error {
		authCtx, err := auth.WithPrincipal(ctx, p)
		if err != nil {
			return err
		}
		state, observation, err = applyInteraction(authCtx, f.deps, p.LearnerID, input, at)
		return err
	})
	return state, observation, err
}

func assertParameterValues(t *testing.T, got any, want models.BKTParams) {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var params models.BKTParams
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]float64{{params.PLearn, want.PLearn}, {params.PForget, want.PForget}, {params.PSlip, want.PSlip}, {params.PGuess, want.PGuess}} {
		if math.Abs(pair[0]-pair[1]) > 1e-12 {
			t.Fatalf("effective parameters %+v, want %+v (trace %s)", params, want, raw)
		}
	}
}

func TestPersonalBKTParametersStayFixedWhileMasteryAndMemoryEvolve(t *testing.T) {
	runParameterPolicyStores(t, func(t *testing.T, f *parameterPolicyFixture) {
		p := f.learner(t, "personal")
		domain, err := f.s.CreateDomain(t.Context(), p.LearnerID, "Personal numbers", "", models.KnowledgeSpace{Concepts: []string{"numbers"}, Prerequisites: map[string][]string{}})
		if err != nil {
			t.Fatal(err)
		}
		// Pre-upgrade applied parameters must not become the personal reference.
		seed := models.NewConceptStateInDomain(p.LearnerID, domain.ID, "numbers")
		seed.PLearn, seed.PForget, seed.PSlip, seed.PGuess = .35, .12, .3, .4
		if err := f.s.UpsertConceptState(t.Context(), seed); err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			activity models.ActivityType
			success  bool
			error    string
			hints    int
			learning bool
		}{
			{models.ActivityPractice, true, "", 0, true},
			{models.ActivityPractice, false, "SYNTAX_ERROR", 3, true},
			{models.ActivityDiagnosticAssessment, false, "KNOWLEDGE_GAP", 0, false},
			{models.ActivityMasteryChallenge, true, "", 0, false},
			{models.ActivityRecall, false, "LOGIC_ERROR", 1, true},
			{models.ActivityPractice, true, "", 0, true},
		}
		start := time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Second)
		mastery := seed.PMastery
		var firstStability float64
		var firstNextReview time.Time
		for i, c := range cases {
			at := start.Add(time.Duration(i) * 24 * time.Hour)
			input := f.prepare(t, p, domain.ID, c.activity, at, c.success)
			input.ErrorType, input.HintsRequested = c.error, c.hints
			state, obs, err := f.apply(t, p, input, at.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if obs["bkt_parameter_policy"] != models.BKTPersonalParameterPolicy {
				t.Fatalf("personal policy absent: %+v", obs)
			}
			assertParameterValues(t, obs["bkt_reference_params"], models.BKTReferenceParams())
			assertParameterValues(t, obs["bkt_effective_params"], models.BKTReferenceParams())
			assertParameterValues(t, models.BKTParams{PLearn: state.PLearn, PForget: state.PForget, PSlip: state.PSlip, PGuess: state.PGuess}, models.BKTReferenceParams())
			if obs["bkt_calibration_source"] != nil || obs["bkt_collective_weights"] != nil {
				t.Fatalf("personal observations must stay outside common calibration: %+v", obs)
			}
			likelihoodMastered, likelihoodUnmastered := .1, .8
			if c.success {
				likelihoodMastered, likelihoodUnmastered = .9, .2
			}
			mastery = mastery * likelihoodMastered / (mastery*likelihoodMastered + (1-mastery)*likelihoodUnmastered)
			if c.learning {
				mastery = mastery*.95 + (1-mastery)*.15
			}
			if math.Abs(state.PMastery-mastery) > 1e-12 || obs["bkt_transition_applied"] != c.learning {
				t.Fatalf("step %d: mastery=%g want %g, transition=%v", i, state.PMastery, mastery, obs["bkt_transition_applied"])
			}
			if state.Reps != i+1 || state.LastReview == nil || !state.LastReview.Equal(at) || state.NextReview == nil {
				t.Fatalf("memory must advance at committed response time: %+v", state)
			}
			if i == 0 {
				firstStability, firstNextReview = state.Stability, *state.NextReview
			} else if i == len(cases)-1 && (state.Stability == firstStability || state.NextReview.Equal(firstNextReview)) {
				t.Fatal("fixed parameters froze memory or review scheduling")
			}
		}
		var sources int
		if err := f.row(t, `SELECT COUNT(*) FROM concept_bkt_estimates WHERE learner_id = ?`, p.LearnerID).Scan(&sources); err != nil || sources != 0 {
			t.Fatalf("personal calibration sources=%d err=%v", sources, err)
		}
		history, err := f.s.GetRecentInteractionsInDomain(t.Context(), p.LearnerID, domain.ID, "numbers", 20)
		if err != nil || len(history) != len(cases) {
			t.Fatalf("personal observations=%d err=%v", len(history), err)
		}
		for _, interaction := range history {
			if interaction.BKTSlip == nil || interaction.BKTGuess == nil || *interaction.BKTSlip != .1 || *interaction.BKTGuess != .2 {
				t.Fatalf("persisted observation changed fixed effective parameters: %+v", interaction)
			}
		}
	})
}

func TestCommonBKTCalibrationSourceThroughFrozenMCPAssessmentHandlers(t *testing.T) {
	runParameterPolicyStores(t, func(t *testing.T, f *parameterPolicyFixture) {
		owner, _, cohort := f.formation(t)
		p, enrollment := f.enroll(t, owner, cohort, "mcp-source")
		call := func(name string, args any) map[string]any {
			t.Helper()
			return f.callTool(t, p, name, args)
		}
		for i := 0; i < 2; i++ {
			activity := call("get_next_activity", map[string]any{"domain_id": enrollment.DomainID})
			decisionID, _ := activity["decision_id"].(string)
			decision, err := f.s.GetPedagogicalDecision(t.Context(), p.TenantScope(), decisionID)
			if err != nil {
				t.Fatal(err)
			}
			prepared := call("prepare_assessment_attempt", map[string]any{
				"domain_id": enrollment.DomainID, "session_id": decision.SessionID, "decision_id": decisionID,
				"concept": decision.Contract.TargetConcept, "activity_type": string(decision.Contract.RecommendedActivityType),
				"observable": "Compare independently", "task_text": "Which is larger, 2 or 4?",
				"rubric_json": `{"criteria":[{"id":"correct","description":"Answer 4","max_score":1}],"passing_score":1}`,
			})
			attemptID := prepared["attempt_id"].(string)
			call("submit_assessment_attempt", map[string]any{"attempt_id": attemptID, "learner_response": "4"})
			args := map[string]any{
				"domain_id": enrollment.DomainID, "session_id": decision.SessionID,
				"concept": decision.Contract.TargetConcept, "activity_type": string(decision.Contract.RecommendedActivityType),
				"attempt_id": attemptID, "success": true, "confidence": .8, "response_time_seconds": 5,
				"evaluator_id": "mcp-host", "evaluation_method": "host_llm",
				"rubric_score_json": `{"criteria_scores":[{"id":"correct","score":1,"evidence":"The learner committed the answer 4."}]}`,
				"idempotency_key":   fmt.Sprintf("mcp-calibration-evaluation-%d", i),
			}
			graded := call("record_interaction", args)
			obs := graded["observation"].(map[string]any)
			if obs["bkt_parameter_policy"] != models.BKTCommonParameterPolicy {
				t.Fatalf("MCP did not expose common parameter policy: %+v", obs)
			}
			assertParameterValues(t, obs["bkt_effective_params"], models.BKTReferenceParams())
			source := obs["bkt_calibration_source"].(map[string]any)
			if source["recorded"] != true || source["observations"] != float64(i+1) {
				t.Fatalf("MCP response omitted its real calibration source: %+v", obs)
			}
			before := f.estimate(t, p.LearnerID)
			call("record_interaction", args) // idempotent retry must not add a sample
			call("record_learning_event", map[string]any{"domain_id": enrollment.DomainID, "concept": decision.Contract.TargetConcept, "attempt_id": attemptID, "event_key": fmt.Sprintf("feedback-%d", i), "kind": "feedback"})
			if after := f.estimate(t, p.LearnerID); after != before {
				t.Fatalf("MCP retry or feedback changed the calibration source: %+v -> %+v", before, after)
			}
		}
		if got := f.estimate(t, p.LearnerID); got.observations != 2 {
			t.Fatalf("MCP created %d admissible observations, want 2", got.observations)
		}
	})
}

func TestNativeInstitutionFreeDomainUsesFixedPersonalParameters(t *testing.T) {
	runParameterPolicyStores(t, func(t *testing.T, f *parameterPolicyFixture) {
		f.deps.Institution = true
		credential := models.SignupCredential{Token: "native-free-domain-" + rand.Text()}
		if err := f.s.CreatePendingSignup(t.Context(), credential, "owner-"+rand.Text()+"@native-free.test", "Free domain institution", "native-free-domain", time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		membership, err := f.s.CompleteSignup(t.Context(), credential, "plan_legacy", "", "hash")
		if err != nil {
			t.Fatal(err)
		}
		scope := models.TenantScope{TenantID: membership.TenantID, UserID: membership.UserID, MembershipID: membership.ID}
		if _, err := f.s.RecordMembershipMFAVerification(t.Context(), scope, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		owner, err := f.s.GetPrincipal(t.Context(), scope, []string{models.OAuthScopeLearner})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.UpdateTenantMember(t.Context(), owner, owner.MembershipID, models.MembershipStatusActive, []string{models.RoleOwner, models.RoleLearner}); err != nil {
			t.Fatal(err)
		}
		if err := f.row(t, `SELECT learner_id FROM tenant_memberships WHERE tenant_id=? AND id=?`, scope.TenantID, scope.MembershipID).Scan(&scope.LearnerID); err != nil {
			t.Fatal(err)
		}
		owner, err = f.s.GetPrincipal(t.Context(), scope, []string{models.OAuthScopeLearner})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.SetInstitutionLearningPolicy(t.Context(), owner, true); err != nil {
			t.Fatal(err)
		}
		created := f.callTool(t, owner, "init_domain", map[string]any{"name": "Own private numbers", "concepts": []string{"numbers"}, "prerequisites": map[string][]string{}})
		domain, err := f.s.GetLearnerDomainByID(t.Context(), owner.LearnerID, created["domain_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
		for i, success := range []bool{true, false} {
			at := start.Add(time.Duration(i) * 24 * time.Hour)
			input := f.prepare(t, owner, domain.ID, models.ActivityPractice, at, success)
			input.ErrorType, input.HintsRequested = "SYNTAX_ERROR", 2
			state, obs, err := f.apply(t, owner, input, at.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if obs["bkt_parameter_policy"] != models.BKTPersonalParameterPolicy || obs["bkt_calibration_source"] != nil || obs["bkt_collective_weights"] != nil {
				t.Fatalf("private formation bridge was classified as common: %+v", obs)
			}
			assertParameterValues(t, obs["bkt_effective_params"], models.BKTReferenceParams())
			if state.Reps != i+1 || state.NextReview == nil {
				t.Fatalf("fixed private policy stopped memory updates: %+v", state)
			}
		}
		var estimates int
		if err := f.row(t, `SELECT COUNT(*) FROM concept_bkt_estimates WHERE tenant_id=?`, owner.TenantID).Scan(&estimates); err != nil || estimates != 0 {
			t.Fatalf("native private domain created common source rows: %d %v", estimates, err)
		}
	})
}
