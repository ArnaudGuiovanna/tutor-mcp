// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type weightCohort struct {
	*statisticsCohort
	byLearner []string
}

func newWeightCohort(t *testing.T) *weightCohort {
	s := setupTestDB(t)
	owner := ownerPrincipal(t, s)
	detail, _ := seedFormationBridge(t, s, owner)
	cohort, err := s.CreateCohort(t.Context(), owner, detail.Version.ID, "Large", 100, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &weightCohort{statisticsCohort: &statisticsCohort{s: s, owner: owner, reader: progressReader(owner), cohort: cohort, version: detail.Version.ID}}
}

// enroll adds one learner whose "numbers" state has the given reviews and parameters.
func (c *weightCohort) enroll(t *testing.T, reps int, p models.BKTParams) *models.ConceptState {
	t.Helper()
	student := formationStudent(t, c.s, fmt.Sprintf("weight%d@weights.test", len(c.members)))
	e, err := c.s.EnrollMembership(t.Context(), c.owner, c.cohort.ID, student.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	state := models.NewConceptStateInDomain(student.LearnerID, e.DomainID, "numbers")
	state.Reps, state.PMastery = reps, .5
	state.PLearn, state.PForget, state.PSlip, state.PGuess = p.PLearn, p.PForget, p.PSlip, p.PGuess
	if err := c.s.UpsertConceptState(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	c.members, c.enrolls = append(c.members, student), append(c.enrolls, e)
	return state
}

func (c *weightCohort) publish(t *testing.T) int {
	t.Helper()
	n, err := c.s.RecomputeCollectiveWeights(t.Context(), c.owner.TenantScope(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (c *weightCohort) weights(t *testing.T) []models.CollectiveWeight {
	t.Helper()
	rows, err := c.s.query(t.Context(), `SELECT version, p_learn, p_forget, p_slip, p_guess, contributors, policy_version
 FROM concept_collective_weights WHERE tenant_id = ? ORDER BY version`, c.owner.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []models.CollectiveWeight
	for rows.Next() {
		var w models.CollectiveWeight
		if err := rows.Scan(&w.Version, &w.PLearn, &w.PForget, &w.PSlip, &w.PGuess, &w.Contributors, &w.PolicyVersion); err != nil {
			t.Fatal(err)
		}
		out = append(out, w)
	}
	return out
}

func (c *weightCohort) scopeFor(learner string) models.TenantScope {
	scope := c.owner.TenantScope()
	scope.LearnerID = learner
	return scope
}

func close2(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

var consensus = models.BKTParams{PLearn: .3, PForget: .08, PSlip: .2, PGuess: .3}

func TestCollectiveWeightsThresholdOutlierAndMinimumReviews(t *testing.T) {
	c := newWeightCohort(t)
	for i := 0; i < 28; i++ {
		c.enroll(t, 5, consensus)
	}
	c.enroll(t, 4, consensus) // one review short: not a contributor
	if n := c.publish(t); n != 0 || len(c.weights(t)) != 0 {
		t.Fatalf("29 learners, 28 qualifying, must not publish: %d", n)
	}
	c.enroll(t, 5, models.BKTParams{PLearn: .5, PForget: .5, PSlip: .49, PGuess: .5}) // outlier, 29th qualifying
	if n := c.publish(t); n != 0 {
		t.Fatalf("29 qualifying learners must not publish: %d", n)
	}
	c.enroll(t, 5, consensus)
	if n := c.publish(t); n != 1 {
		t.Fatalf("30 qualifying learners must publish once: %d", n)
	}
	w := c.weights(t)
	if len(w) != 1 || w[0].Version != 1 || w[0].Contributors != 30 || w[0].PolicyVersion != models.CollectiveWeightsPolicyVersion ||
		!close2(w[0].PLearn, .3) || !close2(w[0].PSlip, .2) || !close2(w[0].PGuess, .3) {
		t.Fatalf("median must ignore the outlier: %+v", w)
	}
	if n := c.publish(t); n != 0 || len(c.weights(t)) != 1 {
		t.Fatalf("an unchanged consensus must not publish a new version: %d", n)
	}
}

func TestCollectiveWeightsHysteresisStepCapAndAppendOnly(t *testing.T) {
	c := newWeightCohort(t)
	var states []*models.ConceptState
	for i := 0; i < 30; i++ {
		states = append(states, c.enroll(t, 5, consensus))
	}
	c.publish(t)
	move := func(learn float64) {
		for _, st := range states {
			st.PLearn = learn
			if err := c.s.UpsertConceptState(t.Context(), st); err != nil {
				t.Fatal(err)
			}
		}
	}
	move(.305) // below the hysteresis band
	if n := c.publish(t); n != 0 {
		t.Fatalf("a 0.005 move must not publish: %d", n)
	}
	move(.45) // a large jump is limited to the step cap
	if n := c.publish(t); n != 1 {
		t.Fatalf("large move: %d", n)
	}
	w := c.weights(t)
	if len(w) != 2 || w[0].Version != 1 || !close2(w[0].PLearn, .3) || !close2(w[1].PLearn, .32) || !close2(w[1].PSlip, .2) {
		t.Fatalf("append-only history with a capped step: %+v", w)
	}
}

func TestCollectiveWeightsAppliedOncePerVersionAndOnlyToParameters(t *testing.T) {
	c := newWeightCohort(t)
	for i := 0; i < 30; i++ {
		c.enroll(t, 5, consensus)
	}
	c.publish(t)
	existing := c.enroll(t, 5, models.BKTParams{PLearn: .15, PForget: .05, PSlip: .1, PGuess: .2})
	fresh := c.enroll(t, 0, models.BKTParams{PLearn: .15, PForget: .05, PSlip: .1, PGuess: .2})
	apply := func(state *models.ConceptState) *models.CollectiveWeightApplication {
		t.Helper()
		var out *models.CollectiveWeightApplication
		if err := c.s.WithTx(t.Context(), func(tx storeport.Store) error {
			var err error
			out, err = tx.(storeport.CollectiveWeightStore).ApplyCollectiveWeights(t.Context(), c.scopeFor(state.LearnerID), state, time.Now().UTC())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	pm, reps, stability := existing.PMastery, existing.Reps, existing.Stability
	got := apply(existing)
	if got == nil || got.Mode != models.CollectiveApplyBlend || got.Version != 1 ||
		!close2(existing.PLearn, .1875) || !close2(existing.PSlip, .125) || !close2(existing.PGuess, .225) || !close2(existing.PForget, .0575) {
		t.Fatalf("reviewed learner must move a bounded 25%% toward the collective: %+v %+v", got, existing)
	}
	if existing.PMastery != pm || existing.Reps != reps || existing.Stability != stability {
		t.Fatal("collective weights must never change mastery or scheduling")
	}
	again := *existing
	if apply(&again) != nil || again.PLearn != existing.PLearn {
		t.Fatal("a weight version must be applied at most once")
	}
	if got := apply(fresh); got == nil || got.Mode != models.CollectiveApplyPrior ||
		!close2(fresh.PLearn, .3) || !close2(fresh.PSlip, .2) || !close2(fresh.PGuess, .3) || !close2(fresh.PForget, .08) {
		t.Fatalf("a learner without reviews takes the collective as prior: %+v %+v", got, fresh)
	}
	// A rolled-back interaction leaves no ledger entry, so the weight still applies.
	rollback := c.enroll(t, 5, models.BKTParams{PLearn: .15, PForget: .05, PSlip: .1, PGuess: .2})
	boom := errors.New("interaction failed")
	err := c.s.WithTx(t.Context(), func(tx storeport.Store) error {
		if _, err := tx.(storeport.CollectiveWeightStore).ApplyCollectiveWeights(t.Context(), c.scopeFor(rollback.LearnerID), rollback, time.Now().UTC()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("rollback: %v", err)
	}
	retry := models.NewConceptStateInDomain(rollback.LearnerID, rollback.DomainID, "numbers")
	retry.Reps = 5
	if apply(retry) == nil {
		t.Fatal("rollback must undo the ledger entry")
	}
	var ledger int
	if err := c.s.queryRow(t.Context(), `SELECT COUNT(*) FROM collective_weight_applications WHERE tenant_id = ?`, c.owner.TenantID).Scan(&ledger); err != nil || ledger != 3 {
		t.Fatalf("ledger rows: %d %v", ledger, err)
	}
	// A second weight version applies once more.
	if _, err := c.s.exec(t.Context(), `INSERT INTO concept_collective_weights
 (tenant_id, formation_version_id, formation_concept_id, version, p_learn, p_forget, p_slip, p_guess, contributors, policy_version, computed_at)
 SELECT tenant_id, formation_version_id, formation_concept_id, 2, .31, p_forget, p_slip, p_guess, contributors, policy_version, computed_at
 FROM concept_collective_weights WHERE tenant_id = ? AND version = 1`, c.owner.TenantID); err != nil {
		t.Fatal(err)
	}
	if got := apply(existing); got == nil || got.Version != 2 {
		t.Fatalf("new version must apply once more: %+v", got)
	}
}

func TestCollectiveWeightsIgnoreNonInstitutionDomainsAndShowToStaff(t *testing.T) {
	c := newWeightCohort(t)
	for i := 0; i < 30; i++ {
		c.enroll(t, 5, consensus)
	}
	c.publish(t)
	legacy := models.NewConceptStateInDomain("L1", "", "numbers")
	if err := c.s.WithTx(t.Context(), func(tx storeport.Store) error {
		out, err := tx.(storeport.CollectiveWeightStore).ApplyCollectiveWeights(t.Context(), c.scopeFor(legacy.LearnerID), legacy, time.Now().UTC())
		if out != nil {
			t.Fatalf("legacy domain must be untouched: %+v", out)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	foreign := models.TenantScope{TenantID: "other-tenant", UserID: c.owner.UserID, MembershipID: c.owner.MembershipID, LearnerID: c.members[0].LearnerID}
	mine := models.NewConceptStateInDomain(c.members[0].LearnerID, c.enrolls[0].DomainID, "numbers")
	mine.Reps = 5
	if err := c.s.WithTx(t.Context(), func(tx storeport.Store) error {
		out, err := tx.(storeport.CollectiveWeightStore).ApplyCollectiveWeights(t.Context(), foreign, mine, time.Now().UTC())
		if out != nil {
			t.Fatalf("a scope from another tenant must not apply weights: %+v", out)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c.recompute(t)
	stats := c.read(t)
	found := false
	for _, concept := range stats.Concepts {
		if concept.StableKey == "numbers" && concept.CollectiveWeight != nil && concept.CollectiveWeight.Version == 1 &&
			concept.CollectiveWeight.Contributors == 30 && close2(concept.CollectiveWeight.PGuess, .3) {
			found = true
		}
		if concept.StableKey == "equations" && concept.CollectiveWeight != nil {
			t.Fatalf("a concept without thirty learners has no weight: %+v", concept)
		}
	}
	if !found {
		t.Fatalf("staff must see the current weight: %+v", stats.Concepts)
	}
}
