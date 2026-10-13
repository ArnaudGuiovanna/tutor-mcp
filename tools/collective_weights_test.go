// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Exercise the real observation pipeline: the collective weight must reach the
// learner inside the interaction, be traced, and apply once per weight version.
func TestCollectiveWeightsReachTheLearnerThroughRealInteractions(t *testing.T) {
	runParameterPolicyStores(t, func(t *testing.T, f *parameterPolicyFixture) {
		owner, version, cohort := f.formation(t)
		p, enrollment := f.enroll(t, owner, cohort, "application")
		insertWeight := func(versionNumber int, learn float64) {
			t.Helper()
			f.exec(t, `INSERT INTO concept_collective_weights
 (tenant_id,formation_version_id,formation_concept_id,version,p_learn,p_forget,p_slip,p_guess,contributors,policy_version,computed_at)
 SELECT tenant_id,formation_version_id,id,?,?,.08,.2,.3,30,?,? FROM formation_concepts
 WHERE tenant_id=? AND formation_version_id=?`, versionNumber, learn, models.CollectiveWeightsPolicyVersion, time.Now().UTC(), owner.TenantID, version.ID)
		}
		start := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
		_, obs := f.record(t, p, enrollment.DomainID, start, true)
		if obs["bkt_collective_weights"] != nil {
			t.Fatal("no weight exists: nothing may be traced")
		}
		insertWeight(1, .3)
		state, obs := f.record(t, p, enrollment.DomainID, start.Add(24*time.Hour), true)
		trace, ok := obs["bkt_collective_weights"].(*models.CollectiveWeightApplication)
		if !ok || trace.Version != 1 || trace.Mode != models.CollectiveApplyBlend || trace.After.PGuess <= trace.Before.PGuess || state.Reps != 2 {
			t.Fatalf("application missing from real observation: %+v / %+v", obs, state)
		}
		_, obs = f.record(t, p, enrollment.DomainID, start.Add(30*time.Hour), true)
		if obs["bkt_collective_weights"] != nil {
			t.Fatal("a version must apply once")
		}
		insertWeight(2, .31)
		_, obs = f.record(t, p, enrollment.DomainID, start.Add(36*time.Hour), true)
		if trace, ok := obs["bkt_collective_weights"].(*models.CollectiveWeightApplication); !ok || trace.Version != 2 {
			t.Fatalf("new published version not applied: %+v", obs)
		}
		var applied, rows int
		if err := f.row(t, `SELECT COUNT(*),MAX(weight_version) FROM collective_weight_applications WHERE learner_id=?`, p.LearnerID).Scan(&rows, &applied); err != nil || rows != 1 || applied != 2 {
			t.Fatalf("application ledger rows=%d version=%d err=%v", rows, applied, err)
		}
	})
}

type calibrationEstimateForTest struct {
	models.BKTParams
	observations int
	lastID       int64
	digest       string
}

func (f *parameterPolicyFixture) estimate(t *testing.T, learner string) calibrationEstimateForTest {
	t.Helper()
	var estimate calibrationEstimateForTest
	if err := f.row(t, `SELECT p_learn,p_forget,p_slip,p_guess,admissible_observations,last_interaction_id,evidence_hash
 FROM concept_bkt_estimates WHERE learner_id = ? AND policy_version = ? ORDER BY curriculum_version DESC LIMIT 1`,
		learner, models.BKTIndividualEstimatePolicyVersion).Scan(&estimate.PLearn, &estimate.PForget, &estimate.PSlip, &estimate.PGuess, &estimate.observations, &estimate.lastID, &estimate.digest); err != nil {
		t.Fatal(err)
	}
	return estimate
}

func (f *parameterPolicyFixture) publishWeights(t *testing.T, owner models.Principal, want int) {
	t.Helper()
	n, err := f.s.RecomputeCollectiveWeights(t.Context(), owner.TenantScope(), time.Now().UTC())
	if err != nil || n != want {
		t.Fatalf("published weight versions=%d want %d, err=%v", n, want, err)
	}
}

func (f *parameterPolicyFixture) record(t *testing.T, p models.Principal, domain string, at time.Time, success bool) (*models.ConceptState, map[string]any) {
	t.Helper()
	input := f.prepare(t, p, domain, models.ActivityPractice, at, success)
	state, obs, err := f.apply(t, p, input, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return state, obs
}

// The source starts with real bound responses rather than injected p_* values.
// This fails with the old worker that aggregated applied concept states.
func TestCollectiveWeightsLearnFromRealResponsesWithoutAppliedParameterFeedback(t *testing.T) {
	runParameterPolicyStores(t, func(t *testing.T, f *parameterPolicyFixture) {
		owner, version, cohort := f.formation(t)
		var learners []models.Principal
		var enrollments []*models.Enrollment
		start := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Second)
		for learner := 0; learner < models.CollectiveWeightMinContributors; learner++ {
			p, enrollment := f.enroll(t, owner, cohort, fmt.Sprintf("contributor-%d", learner))
			learners, enrollments = append(learners, p), append(enrollments, enrollment)
			count := models.CollectiveWeightMinReps
			if learner == models.CollectiveWeightMinContributors-1 {
				count--
			}
			for response := 0; response < count; response++ {
				state, obs := f.record(t, p, enrollment.DomainID, start.Add(time.Duration(response)*24*time.Hour), true)
				if obs["bkt_parameter_policy"] != models.BKTCommonParameterPolicy || obs["bkt_collective_weights"] != nil {
					t.Fatalf("common domain should use stable reference before calibration: %+v", obs)
				}
				assertParameterValues(t, obs["bkt_effective_params"], models.BKTReferenceParams())
				source, ok := obs["bkt_calibration_source"].(map[string]any)
				if !ok || source["recorded"] != true || source["observations"] != response+1 || source["policy_version"] != models.BKTIndividualEstimatePolicyVersion {
					t.Fatalf("committed response not recorded as independent source: %+v", obs)
				}
				if state.Reps != response+1 || state.PMastery <= .1 {
					t.Fatalf("real response did not advance the learner: %+v", state)
				}
			}
		}
		f.publishWeights(t, owner, 0) // 30 learners, only 29 with five observations
		// These twins will provide identical admissible histories after
		// publication. One already has a legacy grade, so it receives a blend;
		// the other has no prior reviews and receives the collective as prior.
		priorLearner, priorEnrollment := f.enroll(t, owner, cohort, "source-twin-prior")
		blendLearner, blendEnrollment := f.enroll(t, owner, cohort, "source-twin-blend")
		legacy := interactionInput{DomainID: blendEnrollment.DomainID, Concept: "numbers", ActivityType: string(models.ActivityPractice), Success: true, Confidence: .8}
		if _, _, err := f.apply(t, blendLearner, legacy, start); err != nil {
			t.Fatal(err)
		}
		last := len(learners) - 1
		f.record(t, learners[last], enrollments[last].DomainID, start.Add(4*24*time.Hour), true)
		// Invalidation of an older observation must invalidate the saved source,
		// even though its last interaction and cached count still look eligible.
		var olderID int64
		if err := f.row(t, `SELECT MIN(id) FROM interactions WHERE learner_id=?`, learners[0].LearnerID).Scan(&olderID); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `UPDATE interactions SET curriculum_invalidated_version=2 WHERE id=?`, olderID)
		f.publishWeights(t, owner, 0)
		// Invalidation is immutable; a new admissible response replenishes the
		// learner's source rather than resurrecting the discarded observation.
		f.record(t, learners[0], enrollments[0].DomainID, start.Add(4*24*time.Hour+time.Hour), true)
		f.publishWeights(t, owner, 1)

		// Five confident successes independently estimate these values from the
		// fixed .15/.05/.1/.2 reference, including the current fifth response.
		want := models.BKTParams{PLearn: .16425, PForget: .05, PSlip: .08575, PGuess: .1905}
		for _, p := range learners {
			estimate := f.estimate(t, p.LearnerID)
			assertParameterValues(t, estimate.BKTParams, want)
			if estimate.observations != 5 || estimate.lastID == 0 || len(estimate.digest) != 64 {
				t.Fatalf("unverifiable independent source: %+v", estimate)
			}
		}
		var weight models.CollectiveWeight
		if err := f.row(t, `SELECT version,contributors,policy_version,p_learn,p_forget,p_slip,p_guess
 FROM concept_collective_weights WHERE tenant_id = ? AND formation_version_id = ?`, owner.TenantID, version.ID).
			Scan(&weight.Version, &weight.Contributors, &weight.PolicyVersion, &weight.PLearn, &weight.PForget, &weight.PSlip, &weight.PGuess); err != nil {
			t.Fatal(err)
		}
		if weight.Version != 1 || weight.Contributors != 30 || weight.PolicyVersion != models.CollectiveWeightsPolicyVersion {
			t.Fatalf("wrong collective source/version: %+v", weight)
		}
		assertParameterValues(t, weight.BKTParams, want)

		// Independent source estimates must be equal despite the legitimate
		// prior/blend difference in their applied collective parameters.
		var effective []models.BKTParams
		for i, twin := range []struct {
			p    models.Principal
			e    *models.Enrollment
			mode string
		}{{priorLearner, priorEnrollment, models.CollectiveApplyPrior}, {blendLearner, blendEnrollment, models.CollectiveApplyBlend}} {
			for response := 0; response < 5; response++ {
				state, obs := f.record(t, twin.p, twin.e.DomainID, start.Add(5*24*time.Hour+time.Duration(response)*time.Hour), true)
				if response == 0 {
					trace, ok := obs["bkt_collective_weights"].(*models.CollectiveWeightApplication)
					if !ok || trace.Version != 1 || trace.Mode != twin.mode {
						t.Fatalf("worker output did not reach the actual learner response: %+v", obs)
					}
					assertParameterValues(t, obs["bkt_effective_params"], trace.After)
					effective = append(effective, models.BKTParams{PLearn: state.PLearn, PForget: state.PForget, PSlip: state.PSlip, PGuess: state.PGuess})
				} else if obs["bkt_collective_weights"] != nil {
					t.Fatalf("twin %d applied a weight version twice", i)
				}
			}
		}
		if effective[0] == effective[1] {
			t.Fatal("the paired learners must have different applied parameters")
		}
		a, b := f.estimate(t, priorLearner.LearnerID), f.estimate(t, blendLearner.LearnerID)
		if a.BKTParams != b.BKTParams || a.observations != 5 || b.observations != 5 {
			t.Fatalf("applied collective/memory contaminated equal source profiles: %+v / %+v", a, b)
		}
		assertParameterValues(t, a.BKTParams, want)
		failedLearner, failedEnrollment := f.enroll(t, owner, cohort, "contrasting-failures")
		for response := 0; response < 5; response++ {
			f.record(t, failedLearner, failedEnrollment.DomainID, start.Add(5*24*time.Hour+time.Duration(response)*time.Hour), false)
		}
		contrast := f.estimate(t, failedLearner.LearnerID)
		assertParameterValues(t, contrast.BKTParams, models.BKTParams{PLearn: .1175, PForget: .05, PSlip: .1275, PGuess: .2225})
		for recompute := 0; recompute < 3; recompute++ {
			f.publishWeights(t, owner, 0)
			if after := f.estimate(t, priorLearner.LearnerID); after != a {
				t.Fatalf("collective computation changed its own source: %+v -> %+v", a, after)
			}
		}
		_, obs := f.record(t, priorLearner, priorEnrollment.DomainID, start.Add(6*24*time.Hour), true)
		if obs["bkt_collective_weights"] != nil {
			t.Fatal("a published weight version was applied twice")
		}
	})
}

func TestBKTCalibrationSourceIsAtomicSingleUseAndExcludesNonComparableResponses(t *testing.T) {
	runParameterPolicyStores(t, func(t *testing.T, f *parameterPolicyFixture) {
		owner, _, cohort := f.formation(t)
		p, enrollment := f.enroll(t, owner, cohort, "source-integrity")
		start := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
		input := f.prepare(t, p, enrollment.DomainID, models.ActivityPractice, start, true)
		// A Go error after apply rolls back evaluation, estimator and model
		// together without PostgreSQL's automatic abort after an SQL error.
		boom := errors.New("later application failure")
		err := f.s.WithTenantTx(t.Context(), p.TenantScope(), func(ctx context.Context, _ storeport.Store) error {
			authCtx, err := auth.WithPrincipal(ctx, p)
			if err != nil {
				return err
			}
			_, _, err = applyInteraction(authCtx, f.deps, p.LearnerID, input, start.Add(time.Second))
			if err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("rollback test: %v", err)
		}
		var rows int
		if err := f.row(t, `SELECT COUNT(*) FROM concept_bkt_estimates WHERE learner_id=?`, p.LearnerID).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("rolled-back source persisted: %d %v", rows, err)
		}
		attempt, err := f.s.GetAssessmentAttempt(t.Context(), p.LearnerID, input.AssessmentAttemptID)
		if err != nil || attempt.Status != models.AssessmentAttemptSubmitted {
			t.Fatalf("failed evaluation consumed attempt: %+v %v", attempt, err)
		}
		if _, _, err := f.apply(t, p, input, start.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		before := f.estimate(t, p.LearnerID)
		if _, _, err := f.apply(t, p, input, start.Add(2*time.Second)); err == nil {
			t.Fatal("duplicate evaluator consumed one committed response twice")
		}
		if after := f.estimate(t, p.LearnerID); after != before || after.observations != 1 {
			t.Fatalf("retry altered independent source: %+v -> %+v", before, after)
		}
		// Preview and synthetic responses are stored as learner observations but
		// excluded from the common estimator, even after an ordinary response.
		for i, provenance := range []string{`{"preview":true}`, `{"metadata":{"source":"synthetic"}}`} {
			at := start.Add(time.Duration(i+1) * time.Hour)
			input := f.prepare(t, p, enrollment.DomainID, models.ActivityPractice, at, false)
			input.EvaluationProvenanceJSON = provenance
			_, obs, err := f.apply(t, p, input, at.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			source := obs["bkt_calibration_source"].(map[string]any)
			if source["recorded"] != false || source["observations"] != 1 || f.estimate(t, p.LearnerID) != before {
				t.Fatalf("noncomparable preview/synthetic observation entered common source: %+v", source)
			}
		}
		// Unbound legacy grades and delivered feedback cannot add a contributor.
		legacy := interactionInput{DomainID: enrollment.DomainID, Concept: "numbers", ActivityType: string(models.ActivityPractice), Success: false, Confidence: .95, ErrorType: "SYNTAX_ERROR"}
		if _, _, err := f.apply(t, p, legacy, start.Add(4*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.s.RecordLearningEvent(t.Context(), p, models.LearningEventRequest{EventKey: "feedback-once", DomainID: enrollment.DomainID, ConceptID: "numbers", AttemptID: input.AssessmentAttemptID, Kind: "feedback"}); err != nil {
			t.Fatal(err)
		}
		if after := f.estimate(t, p.LearnerID); after != before {
			t.Fatalf("legacy grade or delivered feedback altered independent source: %+v -> %+v", before, after)
		}
		_, obs := f.record(t, p, enrollment.DomainID, start.Add(5*time.Hour), true)
		source := obs["bkt_calibration_source"].(map[string]any)
		if source["observations"] != 2 || source["recorded"] != true {
			t.Fatalf("filtered source failed to resume: %+v", source)
		}
		after := f.estimate(t, p.LearnerID)
		if after.observations != 2 || math.Abs(after.PLearn-.1557) > 1e-12 || math.Abs(after.PSlip-.0943) > 1e-12 {
			t.Fatalf("excluded observations contaminated next profile: %+v", after)
		}
	})
}
