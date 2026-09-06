package tools

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
)

const definedAssessmentRubricJSON = `{
	"criteria":[
		{"id":"correctness","description":"Produces the exact numerical result with its unit.","max_score":2,
		 "anchors":[{"score":0,"description":"No correct result."},{"score":1,"description":"Correct number but missing unit."},{"score":2,"description":"Correct number and unit."}]},
		{"id":"reasoning","description":"Explains why the selected operation applies.","max_score":1}
	],
	"passing_score":2.5,
	"answer_key":"The expected result is 42 metres.\nEquivalent derivations are acceptable."
}`

func TestNormalizeAssessmentRubricPreservesGeneratedSemantics(t *testing.T) {
	rubric, err := normalizeAssessmentRubric(definedAssessmentRubricJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rubric.AnswerKey, "42 metres") || len(rubric.Criteria[0].Anchors) != 3 {
		t.Fatalf("generated answer key or anchors were discarded: %+v", rubric)
	}
	encoded, err := json.Marshal(assessmentRubricMap(rubric))
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := normalizeAssessmentRubric(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded, rubric) {
		t.Fatalf("canonical rubric lost semantic content during persistence: got=%+v want=%+v", reloaded, rubric)
	}
}

func TestNormalizeAssessmentRubricRejectsUndefinedOrUnsupportedRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "no description", raw: `{"criteria":[{"id":"x","max_score":1}],"passing_score":1}`, want: "description"},
		{name: "empty description", raw: `{"criteria":[{"id":"x","description":"  ","max_score":1}],"passing_score":1}`, want: "description"},
		{name: "required rule unsupported", raw: `{"criteria":[{"id":"x","description":"Correct result.","max_score":1,"required":true}],"passing_score":1}`, want: `unsupported field "required"`},
		{name: "levels unsupported", raw: `{"criteria":[{"id":"x","description":"Correct result.","max_score":1}],"passing_score":1,"levels":["pass"]}`, want: `unsupported field "levels"`},
		{name: "unknown answer key type", raw: `{"criteria":[{"id":"x","description":"Correct result.","max_score":1}],"passing_score":1,"answer_key":{"answer":42}}`, want: "answer_key"},
		{name: "unknown anchor rule", raw: `{"criteria":[{"id":"x","description":"Correct result.","max_score":1,"anchors":[{"score":1,"description":"Correct.","required":true}]}],"passing_score":1}`, want: `unsupported field "required"`},
		{name: "ambiguous numeric string", raw: `{"criteria":[{"id":"x","description":"Correct result.","max_score":"1"}],"passing_score":1}`, want: "JSON number"},
		{name: "duplicate criteria", raw: `{"criteria":[{"id":"x","description":"First.","max_score":1},{"id":"x","description":"Second.","max_score":1}],"passing_score":1}`, want: "unique"},
		{name: "unbounded passing score", raw: `{"criteria":[{"id":"x","description":"Correct result.","max_score":1}],"passing_score":2}`, want: "passing_score"},
		{name: "empty criteria", raw: `{"criteria":[],"passing_score":1}`, want: "at least one"},
		{name: "legacy shorthand", raw: `{"criteria":{"correctness":1},"passing_score":1}`, want: "array"},
		{name: "anchor above maximum", raw: `{"criteria":[{"id":"x","description":"Correct result.","max_score":1,"anchors":[{"score":2,"description":"Correct."}]}],"passing_score":1}`, want: "between zero and max_score"},
		{name: "anchor without description", raw: `{"criteria":[{"id":"x","description":"Correct result.","max_score":1,"anchors":[{"score":1}]}],"passing_score":1}`, want: "description"},
		{name: "duplicate anchor scores", raw: `{"criteria":[{"id":"x","description":"Correct result.","max_score":1,"anchors":[{"score":1,"description":"First."},{"score":1,"description":"Different."}]}],"passing_score":1}`, want: "unique"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeAssessmentRubric(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want explicit rejection containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateBoundAssessmentRubricRejectsNonFiniteTypedValues(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		rubric := models.AssessmentRubric{
			Criteria:     []models.AssessmentRubricCriterion{{ID: "x", Description: "Correct result.", MaxScore: value}},
			PassingScore: 1,
		}
		if err := validateBoundAssessmentRubric(rubric); err == nil {
			t.Fatalf("non-finite criterion maximum %v accepted", value)
		}
		rubric.Criteria[0].MaxScore = 1
		rubric.PassingScore = value
		if err := validateBoundAssessmentRubric(rubric); err == nil {
			t.Fatalf("non-finite passing score %v accepted", value)
		}
	}
}

func TestDeriveBoundAssessmentOutcomeRequiresCriterionEvidence(t *testing.T) {
	rubric, err := normalizeAssessmentRubric(definedAssessmentRubricJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		score string
		valid bool
	}{
		{name: "observations for every criterion", score: `{"criteria_scores":[{"id":"correctness","score":2,"evidence":"Learner answered 42 metres."},{"id":"reasoning","score":0.5,"evidence":"Operation identified but its justification is incomplete."}]}`, valid: true},
		{name: "missing observation", score: `{"criteria_scores":[{"id":"correctness","score":2},{"id":"reasoning","score":0.5,"evidence":"Partial justification."}]}`},
		{name: "empty observation", score: `{"criteria_scores":[{"id":"correctness","score":2,"evidence":"  "},{"id":"reasoning","score":0.5,"evidence":"Partial justification."}]}`},
		{name: "criterion not scored", score: `{"criteria_scores":[{"id":"correctness","score":2,"evidence":"Correct result."}]}`},
		{name: "score above maximum", score: `{"criteria_scores":[{"id":"correctness","score":3,"evidence":"Correct result."},{"id":"reasoning","score":0.5,"evidence":"Partial justification."}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			score, _, err := normalizeRubricScoreJSON(tc.score, assessmentRubricMap(rubric))
			if err != nil {
				t.Fatal(err)
			}
			total, passed, err := deriveBoundAssessmentOutcome(rubric, score)
			if tc.valid {
				if err != nil || total != 2.5 || !passed {
					t.Fatalf("total=%v passed=%v err=%v", total, passed, err)
				}
			} else if err == nil {
				t.Fatalf("unsupported bound score accepted: total=%v passed=%v", total, passed)
			}
		})
	}
}

func TestDeriveAssessmentOutcomeIsIndependentOfScoreOrder(t *testing.T) {
	rubric := map[string]any{
		"criteria":      []map[string]any{{"id": "a", "max_score": 1e16}, {"id": "b", "max_score": 1.0}, {"id": "c", "max_score": 1.0}},
		"passing_score": 1.0,
	}
	scores := []map[string]any{{"id": "a", "score": 1e16}, {"id": "b", "score": 1.0}, {"id": "c", "score": 1.0}}
	var first float64
	for i := 0; i < 100; i++ {
		total, passed, err := deriveAssessmentOutcome(rubric, map[string]any{"criteria_scores": scores})
		if err != nil || !passed {
			t.Fatalf("derive assessment outcome: total=%v passed=%v err=%v", total, passed, err)
		}
		if i == 0 {
			first = total
		} else if total != first {
			t.Fatalf("criterion/Go map iteration order changed total: got=%v first=%v", total, first)
		}
		scores[0], scores[2] = scores[2], scores[0]
	}
}

func TestBoundAssessmentRubricSurvivesEvaluationAndRequiresEvidence(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain := makeOwnerDomain(t, store, "L_owner", "math")
	ctx := context.Background()
	curriculum, err := store.EnsureCurriculumBaseline(ctx, "L_owner", domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session, err := store.OpenLearningSession(ctx, "L_owner", domain.ID, "", now)
	if err != nil {
		t.Fatal(err)
	}
	decision := &models.PedagogicalDecision{
		ID: "decision-rubric-test", LearnerID: "L_owner", DomainID: domain.ID,
		SessionID: session.ID, CurriculumVersion: curriculum.Version,
		PolicyVersion: models.PedagogicalPolicyVersion, CreatedAt: now,
		Contract: models.PedagogicalContract{
			DecisionID: "decision-rubric-test", CurriculumVersion: curriculum.Version,
			PolicyVersion: models.PedagogicalPolicyVersion,
			TargetConcept: "a", RecommendedActivityType: models.ActivityPractice,
			Competency: curriculumCompetency(curriculum, "a"),
		},
	}
	if err := store.CreatePedagogicalDecision(ctx, models.LegacyPrincipal("L_owner").TenantScope(), decision); err != nil {
		t.Fatal(err)
	}
	prepared := callTool(t, deps, registerPrepareAssessmentAttempt, "L_owner", "prepare_assessment_attempt", map[string]any{
		"domain_id": domain.ID, "session_id": session.ID, "decision_id": decision.ID,
		"concept": "a", "activity_type": "PRACTICE", "observable": "Apply concept a.",
		"task_text": "Calculate the distance and justify the operation.", "rubric_json": definedAssessmentRubricJSON,
	})
	if prepared.IsError {
		t.Fatalf("prepare bound assessment: %s", resultText(prepared))
	}
	out := decodeResult(t, prepared)
	attemptID, _ := out["attempt_id"].(string)
	submitted := callTool(t, deps, registerSubmitAssessmentAttempt, "L_owner", "submit_assessment_attempt", map[string]any{
		"attempt_id": attemptID, "learner_response": "42 metres, using multiplication.",
	})
	if submitted.IsError {
		t.Fatalf("submit bound assessment: %s", resultText(submitted))
	}
	args := assessmentEvaluationArgs(domain.ID, session.ID, attemptID, true)
	args["rubric_score_json"] = `{"criteria_scores":[{"id":"correctness","score":2},{"id":"reasoning","score":0.5,"evidence":"Partial justification."}]}`
	rejected := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", args)
	if !rejected.IsError || !strings.Contains(resultText(rejected), "evidence") {
		t.Fatalf("bound assessment accepted a score without evidence: %s", resultText(rejected))
	}
	attempt, err := store.GetAssessmentAttempt(ctx, "L_owner", attemptID)
	if err != nil || attempt.Status != models.AssessmentAttemptSubmitted {
		t.Fatalf("rejected score consumed attempt: attempt=%+v err=%v", attempt, err)
	}
	accepted := callTool(t, deps, registerRecordInteraction, "L_owner", "record_interaction", assessmentEvaluationArgs(domain.ID, session.ID, attemptID, true))
	if accepted.IsError {
		t.Fatalf("evaluate bound assessment: %s", resultText(accepted))
	}
	attempt, err = store.GetAssessmentAttempt(ctx, "L_owner", attemptID)
	if err != nil {
		t.Fatal(err)
	}
	rubric, err := normalizeAssessmentRubric(attempt.RubricJSON)
	if err != nil || rubric.AnswerKey == "" || len(rubric.Criteria[0].Anchors) != 3 {
		t.Fatalf("evaluation discarded the frozen rubric's semantic content: rubric=%+v err=%v", rubric, err)
	}
}
