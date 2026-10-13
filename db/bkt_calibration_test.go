// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestBKTCalibrationHistoricalAppliedParametersAreNotSources(t *testing.T) {
	c := newWeightCohort(t)
	for n := 0; n < models.CollectiveWeightMinContributors; n++ {
		state := c.enroll(t, 0, consensus)
		state.Reps = 100
		if err := c.s.UpsertConceptState(t.Context(), state); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.publish(t); got != 0 {
		t.Fatalf("historical applied/base parameters became independent sources: %d", got)
	}
	var count int
	if err := c.s.queryRow(t.Context(), `SELECT COUNT(*) FROM concept_bkt_estimates`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("no estimate backfill is allowed: %d %v", count, err)
	}
}

func TestBKTCalibrationSourcesDoNotFollowAppliedStateAndInvalidateExactly(t *testing.T) {
	c := newWeightCohort(t)
	var states []*models.ConceptState
	for n := 0; n < models.CollectiveWeightMinContributors; n++ {
		states = append(states, c.enroll(t, 5, consensus))
	}
	if c.publish(t) != 1 {
		t.Fatal("valid independent cohort did not publish")
	}
	for _, state := range states {
		state.PLearn, state.PForget, state.PSlip, state.PGuess = .49, .49, .49, .49
		if err := c.s.UpsertConceptState(t.Context(), state); err != nil {
			t.Fatal(err)
		}
	}
	if c.publish(t) != 0 {
		t.Fatal("applied learner state fed back into the collective")
	}
	next := consensus
	next.PLearn = .45
	for _, state := range states {
		c.saveEstimate(t, state, next)
	}
	first := states[0]
	scope := c.scopeFor(first.LearnerID)
	prior, err := c.s.GetBKTCalibrationEvidence(t.Context(), scope, first, 20)
	if err != nil || prior.Observations != 5 {
		t.Fatalf("initial evidence: %+v %v", prior, err)
	}
	// Removing an older source must invalidate the digest despite retaining
	// the same latest interaction and all of the applied concept parameters.
	if _, err := c.s.exec(t.Context(), `UPDATE interactions SET curriculum_invalidated_version = 2
 WHERE id = (SELECT MIN(id) FROM interactions WHERE tenant_id = ? AND learner_id = ?)`, scope.TenantID, first.LearnerID); err != nil {
		t.Fatal(err)
	}
	actual, err := c.s.GetBKTCalibrationEvidence(t.Context(), scope, first, 20)
	if err != nil || actual.Observations != 4 || actual.LastInteractionID != prior.LastInteractionID || actual.EvidenceHash == prior.EvidenceHash {
		t.Fatalf("older invalidation did not change exact source: before=%+v after=%+v err=%v", prior, actual, err)
	}
	if err := c.s.SaveBKTCalibrationEstimate(t.Context(), scope, first, prior, next, time.Now().UTC()); err == nil {
		t.Fatal("stale evidence was accepted by the estimate writer")
	}
	if c.publish(t) != 0 || len(c.weights(t)) != 1 {
		t.Fatal("stale source still counted toward thirty contributors")
	}
	badgeObservation(t, c.s, c.members[0], c.enrolls[0], "numbers", models.ActivityPractice, time.Now().UTC().Add(-time.Minute), time.Time{}, time.Time{}, true, 0)
	c.saveEstimate(t, first, next)
	if c.publish(t) != 1 || len(c.weights(t)) != 2 {
		t.Fatal("fresh valid source did not restore eligibility")
	}
}

func TestBKTCalibrationExcludesSyntheticAndUnlinkedFeeds(t *testing.T) {
	c := newWeightCohort(t)
	state := c.enroll(t, 5, consensus)
	scope := c.scopeFor(state.LearnerID)
	before, err := c.s.GetBKTCalibrationEvidence(t.Context(), scope, state, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.s.CreateInteraction(t.Context(), &models.Interaction{LearnerID: state.LearnerID, DomainID: state.DomainID,
		Concept: state.Concept, ActivityType: string(models.ActivityPractice), Success: false, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	id := badgeObservation(t, c.s, c.members[0], c.enrolls[0], "numbers", models.ActivityPractice, time.Now().UTC().Add(-time.Minute), time.Time{}, time.Time{}, false, 0)
	if _, err := c.s.exec(t.Context(), `UPDATE assessment_attempts SET evaluation_provenance_json = ? WHERE id = ?`, `{"evaluation":{"synthetic":true}}`, id); err != nil {
		t.Fatal(err)
	}
	after, err := c.s.GetBKTCalibrationEvidence(t.Context(), scope, state, 20)
	if err != nil || !sameCalibrationSource(before, after) || len(after.Interactions) != 5 {
		t.Fatalf("inadmissible feeds changed calibration: before=%+v after=%+v err=%v", before, after, err)
	}
	for _, raw := range []string{`{"preview":true}`, `{"source":"simulation"}`, `{"metadata":{"mode":"synthetic"}}`, `{invalid`} {
		if calibrationProvenanceAdmissible(raw) {
			t.Fatalf("explicit preview/synthetic or malformed provenance accepted: %s", raw)
		}
	}
}

func TestBKTCalibrationV1WeightsAreHistoricalAndV2VersionsStayMonotonic(t *testing.T) {
	c := newWeightCohort(t)
	var first *models.ConceptState
	for n := 0; n < models.CollectiveWeightMinContributors; n++ {
		state := c.enroll(t, 5, consensus)
		if first == nil {
			first = state
		}
	}
	scope := c.scopeFor(first.LearnerID)
	evidence, err := c.s.GetBKTCalibrationEvidence(t.Context(), scope, first, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.exec(t.Context(), `INSERT INTO concept_collective_weights
 (tenant_id,formation_version_id,formation_concept_id,version,p_learn,p_forget,p_slip,p_guess,contributors,policy_version,computed_at)
 VALUES (?,?,?,7,.49,.49,.49,.49,30,'institution-collective-weights-v1',?)`, scope.TenantID, evidence.FormationVersionID, evidence.FormationConceptID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	first.PLearn, first.PForget, first.PSlip, first.PGuess = .49, .49, .49, .49
	if app, err := c.s.ApplyCollectiveWeights(t.Context(), scope, first, time.Now().UTC()); err != nil || app != nil || first.PLearn != .15 || first.PSlip != .1 {
		t.Fatalf("v1-only common domain must use fixed reference: %+v %+v %v", app, first, err)
	}
	if c.publish(t) != 1 {
		t.Fatal("first independent v2 did not publish")
	}
	weights := c.weights(t)
	if len(weights) != 2 || weights[0].Version != 7 || weights[1].Version != 8 || weights[1].PLearn != consensus.PLearn {
		t.Fatalf("new policy must retain history, allocate max+1, and start from independent source: %+v", weights)
	}
	if _, err := c.s.exec(t.Context(), `INSERT INTO collective_weight_applications
 (tenant_id,enrollment_id,formation_concept_id,learner_id,weight_version,mode,applied_at) VALUES (?,?,?,?,7,'blend',?)`, scope.TenantID, evidence.EnrollmentID, evidence.FormationConceptID, first.LearnerID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	first.PLearn = .49
	if app, err := c.s.ApplyCollectiveWeights(t.Context(), scope, first, time.Now().UTC()); err != nil || app == nil || app.Version != 8 || !close2(first.PLearn, .1875) {
		t.Fatalf("obsolete applied parameters contaminated v2 application: %+v %+v %v", app, first, err)
	}
}

func TestBKTCalibrationEstimateLifecycleAndVersionForeignKeys(t *testing.T) {
	c := newWeightCohort(t)
	state := c.enroll(t, 5, consensus)
	ctx := t.Context()
	other := formationStudent(t, c.s, "wrong-estimate-owner@calibration.test")
	if _, err := c.s.exec(ctx, `UPDATE concept_bkt_estimates SET learner_id = ? WHERE tenant_id = ?`, other.LearnerID, c.owner.TenantID); err == nil {
		t.Fatal("estimate accepted a different learner from the enrollment owner in the same tenant")
	}
	if _, err := c.s.exec(ctx, `UPDATE concept_bkt_estimates SET formation_version_id = 'foreign-version' WHERE tenant_id = ?`, c.owner.TenantID); err == nil {
		t.Fatal("estimate accepted a concept/enrollment from another formation version")
	}
	operator := models.ControlPlanePrincipal{ActorID: "calibration-restore", Roles: []string{models.RolePlatformAdmin}, Reason: "restore exercise", RequestID: "calibration-restore"}
	tables, objects, err := c.s.ComputeTenantChecksums(ctx, operator, c.owner.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := c.s.RequestTenantRestoreVerification(ctx, operator, c.owner.TenantID, "calibration-backup", tables, objects)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.exec(ctx, `UPDATE concept_bkt_estimates SET p_learn = .4 WHERE tenant_id = ?`, c.owner.TenantID); err != nil {
		t.Fatal(err)
	}
	if matched, err := c.s.VerifyTenantRestore(ctx, operator, c.owner.TenantID, manifest.ID); err != nil || matched {
		t.Fatalf("restore failed to detect lost calibration state: matched=%v err=%v", matched, err)
	}
	request, err := c.s.RequestTenantDSAR(ctx, c.owner, state.LearnerID, "erase", "learner request")
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce a request created before the new erasure checkpoint existed.
	if _, err := c.s.exec(ctx, `DELETE FROM tenant_dsar_phases WHERE tenant_id = ? AND request_id = ? AND phase = 'concept_bkt_estimates'`, c.owner.TenantID, request.ID); err != nil {
		t.Fatal(err)
	}
	worker := models.WorkerPrincipal{ActorID: "calibration-erasure"}
	scope := c.owner.TenantScope()
	scope.UserID, scope.MembershipID = "worker_"+worker.ActorID, "worker_process"
	done := false
	for batch := 0; batch < 200 && !done; batch++ {
		if done, _, err = c.s.ProcessTenantDSARErasureBatch(ctx, scope, worker, request.ID, 10, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := c.s.queryRow(ctx, `SELECT COUNT(*) FROM concept_bkt_estimates WHERE tenant_id = ? AND learner_id = ?`, c.owner.TenantID, state.LearnerID).Scan(&count); err != nil || !done || count != 0 {
		t.Fatalf("resumed erasure left calibration source: done=%v count=%d err=%v", done, count, err)
	}
}

func TestBKTCalibrationPostgresForcedRLS(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("requires PostgreSQL")
	}
	c := newWeightCohort(t)
	c.enroll(t, 5, consensus)
	s := c.s
	ctx := t.Context()
	var schema string
	if err := s.root.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	var enabled, forced bool
	if err := s.root.QueryRowContext(ctx, `SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid='concept_bkt_estimates'::regclass`).Scan(&enabled, &forced); err != nil || !enabled || !forced {
		t.Fatalf("RLS flags: %t %t %v", enabled, forced, err)
	}
	role := fmt.Sprintf(`"tutor_calibration_rls_%d"`, testDBCounter.Add(1))
	if _, err := s.root.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.root.ExecContext(context.Background(), `DROP OWNED BY `+role)
		_, _ = s.root.ExecContext(context.Background(), `DROP ROLE `+role)
	}()
	for _, grant := range []string{`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role, `GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role} {
		if _, err := s.root.ExecContext(ctx, grant); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE `+role); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant',$1,true)`, c.owner.TenantID); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT to_jsonb(row)::text FROM concept_bkt_estimates row LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant','foreign',true)`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM concept_bkt_estimates`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("foreign read: %d %v", count, err)
	}
	for _, query := range []string{`UPDATE concept_bkt_estimates SET p_learn=.4`, `DELETE FROM concept_bkt_estimates`} {
		result, err := tx.ExecContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if affected, _ := result.RowsAffected(); affected != 0 {
			t.Fatal("foreign mutation succeeded")
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO concept_bkt_estimates SELECT (jsonb_populate_record(NULL::concept_bkt_estimates,$1::jsonb)).*`, raw)
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("foreign insert must fail at tenant boundary: %v", err)
	}
}

var _ storeport.BKTCalibrationStore = (*Store)(nil)
