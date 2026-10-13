// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestBKTCalibrationWindowInvalidationAndCandidateBound(t *testing.T) {
	c := newWeightCohort(t)
	state := c.enroll(t, 25, consensus)
	scope := c.scopeFor(state.LearnerID)
	before, err := c.s.GetBKTCalibrationEvidence(t.Context(), scope, state, models.BKTCalibrationProfileWindow)
	if err != nil || before.Observations != models.BKTCalibrationProfileWindow || len(before.Interactions) != models.BKTCalibrationProfileWindow {
		t.Fatalf("profile was not bounded: %+v %v", before, err)
	}
	// A source older than the actual estimator window has no causal effect.
	if _, err := c.s.exec(t.Context(), `UPDATE interactions SET curriculum_invalidated_version = 2
 WHERE id = (SELECT MIN(id) FROM interactions WHERE tenant_id = ? AND learner_id = ?)`, scope.TenantID, state.LearnerID); err != nil {
		t.Fatal(err)
	}
	outside, err := c.s.GetBKTCalibrationEvidence(t.Context(), scope, state, models.BKTCalibrationProfileWindow)
	if err != nil || !sameCalibrationSource(before, outside) {
		t.Fatalf("unused historical source changed estimator evidence: %+v %v", outside, err)
	}
	// An invalidated row inside the used window is replaced by an older valid
	// row. Count/latest ID stay equal, but the evidence digest must change.
	insideID := before.Interactions[len(before.Interactions)-1].ID
	if _, err := c.s.exec(t.Context(), `UPDATE interactions SET curriculum_invalidated_version = 2 WHERE id = ?`, insideID); err != nil {
		t.Fatal(err)
	}
	inside, err := c.s.GetBKTCalibrationEvidence(t.Context(), scope, state, models.BKTCalibrationProfileWindow)
	if err != nil || inside.Observations != before.Observations || inside.LastInteractionID != before.LastInteractionID || inside.EvidenceHash == before.EvidenceHash {
		t.Fatalf("used-window invalidation was not distinguished: %+v %v", inside, err)
	}
	if err := c.s.SaveBKTCalibrationEstimate(t.Context(), scope, state, before, consensus, time.Now().UTC()); err == nil {
		t.Fatal("stale equal-count/equal-latest evidence was saved")
	}
	// Place more than one candidate horizon of explicitly synthetic committed
	// responses ahead of the real evidence. The bounded search must not walk
	// arbitrarily far back to retrieve those older responses.
	start := time.Now().UTC().Add(-6 * time.Hour)
	for n := 0; n < models.BKTCalibrationCandidateHorizon+1; n++ {
		id := badgeObservation(t, c.s, c.members[0], c.enrolls[0], "numbers", models.ActivityPractice,
			start.Add(time.Duration(n)*time.Minute), time.Time{}, time.Time{}, true, 0)
		if _, err := c.s.exec(t.Context(), `UPDATE assessment_attempts SET evaluation_provenance_json = '{"preview":true}' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	bounded, err := c.s.GetBKTCalibrationEvidence(t.Context(), scope, state, models.BKTCalibrationProfileWindow)
	if err != nil || bounded.Observations != 0 || len(bounded.Interactions) != 0 {
		t.Fatalf("source search exceeded its candidate horizon: %+v %v", bounded, err)
	}
	classification, err := c.s.GetBKTCalibrationEvidence(t.Context(), scope, state, 0)
	if err != nil || classification == nil || classification.Observations != 0 || classification.EvidenceHash != "" || classification.Interactions != nil {
		t.Fatalf("cheap common classification read an evidence window: %+v %v", classification, err)
	}
}

func TestBKTCalibrationFormationVersionsDoNotPoolAndMigrationStartsFresh(t *testing.T) {
	c := newWeightCohort(t)
	clone, err := c.s.CloneFormationVersion(t.Context(), c.owner, c.version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.PublishFormationVersion(t.Context(), c.owner, clone.Version.ID); err != nil {
		t.Fatal(err)
	}
	nextCohort, err := c.s.CreateCohort(t.Context(), c.owner, clone.Version.ID, "Next version", 100, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var first *models.ConceptState
	for n := 0; n < models.CollectiveWeightMinContributors-1; n++ {
		state := c.enroll(t, 5, consensus)
		if first == nil {
			first = state
		}
	}
	next := &weightCohort{statisticsCohort: &statisticsCohort{s: c.s, owner: c.owner, cohort: nextCohort, version: clone.Version.ID}}
	next.enroll(t, 5, consensus)
	if c.publish(t) != 0 {
		t.Fatal("29 old-version learners and one new-version learner were pooled")
	}
	c.enroll(t, 5, consensus)
	if c.publish(t) != 1 {
		t.Fatal("thirty comparable learners on the old version did not qualify")
	}
	migration, err := c.s.MigrateFormationEnrollment(t.Context(), c.owner, c.enrolls[0].ID, nextCohort.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := c.s.GetBKTCalibrationEvidence(t.Context(), c.scopeFor(first.LearnerID), first, models.BKTCalibrationProfileWindow)
	if err != nil || evidence == nil || evidence.EnrollmentID != migration.Enrollment.ID || evidence.FormationVersionID != clone.Version.ID || evidence.Observations != 0 {
		t.Fatalf("migration copied old evidence to the new version: %+v %v", evidence, err)
	}
	var count int
	if err := c.s.queryRow(t.Context(), `SELECT COUNT(*) FROM concept_bkt_estimates WHERE tenant_id = ? AND enrollment_id = ?`, c.owner.TenantID, migration.Enrollment.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration copied independent estimates: %d %v", count, err)
	}
	// Bring the new version to thirty contributors without changing the old
	// source weight; the publications must remain independently identifiable.
	for n := 1; n < models.CollectiveWeightMinContributors; n++ {
		next.enroll(t, 5, consensus)
	}
	if c.publish(t) != 1 {
		t.Fatal("thirty comparable learners on the new version did not qualify independently")
	}
	if err := c.s.queryRow(t.Context(), `SELECT COUNT(DISTINCT formation_version_id) FROM concept_collective_weights WHERE tenant_id = ?`, c.owner.TenantID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("version-specific publications lost their scope: %d %v", count, err)
	}
}
