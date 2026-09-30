// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func badgeObservation(t *testing.T, s *Store, p models.Principal, e *models.Enrollment, key string, activity models.ActivityType, at, last, due time.Time, pass bool, hints int) string {
	t.Helper()
	ctx := t.Context()
	curriculum, err := s.GetCurriculumSnapshot(ctx, p.LearnerID, e.DomainID, 0)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.OpenLearningSession(ctx, p.LearnerID, e.DomainID, "", at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var competency *models.CurriculumConcept
	for i := range curriculum.Concepts {
		if curriculum.Concepts[i].Key == key {
			competency = &curriculum.Concepts[i]
		}
	}
	if competency == nil {
		t.Fatal("missing concept")
	}
	id := "badge-test-" + rand.Text()
	d := &models.PedagogicalDecision{ID: "decision-" + id, LearnerID: p.LearnerID, DomainID: e.DomainID, SessionID: session.ID, CurriculumVersion: curriculum.Version, PolicyVersion: models.PedagogicalPolicyVersion, CreatedAt: at.Add(-time.Minute), ContextJSON: "{}"}
	d.Contract = models.PedagogicalContract{DecisionID: d.ID, PolicyVersion: d.PolicyVersion, CurriculumVersion: d.CurriculumVersion, TargetConcept: key, RecommendedActivityType: activity, Competency: competency, LearningEventProtocol: models.LearningEventProtocol}
	if err := s.CreatePedagogicalDecision(ctx, p.TenantScope(), d); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(competency)
	a := &models.AssessmentAttempt{ID: id, LearnerID: p.LearnerID, DomainID: e.DomainID, ConceptID: key, SessionID: session.ID, ActivityID: "activity-" + id, ActivityVersion: 1, ActivityType: string(activity), DecisionID: d.ID, CurriculumVersion: curriculum.Version, CurriculumConceptJSON: string(raw), Observable: "Apply independently", TaskText: "Frozen task", RubricJSON: `{"criteria":[{"id":"correctness","description":"A correct response.","max_score":1}],"passing_score":1}`, PassingScore: 1, CreatedAt: at.Add(-time.Minute)}
	if err := s.CreateAssessmentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitAssessmentAttempt(ctx, p.LearnerID, id, "Learner response", "hash", at); err != nil {
		t.Fatal(err)
	}
	score, total := completeBoundScore, 1.0
	if !pass {
		score, total = `{"criteria_scores":[{"id":"correctness","score":0,"evidence":"Not correct."}]}`, 0
	}
	if err := s.CompleteAssessmentEvaluation(ctx, p.LearnerID, id, score, "host", models.EvaluationMethodHostLLM, "{}", total, pass, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	i := &models.Interaction{LearnerID: p.LearnerID, DomainID: e.DomainID, Concept: key, SessionID: session.ID, ActivityType: string(activity), Success: pass, HintsRequested: hints, AssessmentAttemptID: id, CreatedAt: at.Add(time.Second)}
	if err := s.CreateInteraction(ctx, i); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(map[string]any{"last_review": last, "next_review": due})
	obs, _ := json.Marshal(map[string]any{"fsrs_update_applied": true, "fsrs_observation_at": at})
	after, _ := json.Marshal(map[string]any{"last_review": at, "stability": 30})
	if err := s.CreatePedagogicalSnapshot(ctx, &models.PedagogicalSnapshot{InteractionID: i.ID, LearnerID: p.LearnerID, DomainID: e.DomainID, Concept: key, ActivityType: string(activity), BeforeJSON: string(before), ObservationJSON: string(obs), AfterJSON: string(after), DecisionJSON: "{}", CreatedAt: at.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestInstitutionBadgesEvidenceMilestonesAndHistory(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	detail, cohort := seedFormationBridge(t, s, owner)
	p := formationStudent(t, s, "badges@test.com")
	e, err := s.EnrollMembership(ctx, owner, cohort.ID, p.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-100 * 24 * time.Hour).Truncate(time.Second)
	// Estimates and practice cannot produce mastery or completion badges.
	cs := models.NewConceptStateInDomain(p.LearnerID, e.DomainID, "numbers")
	cs.PMastery = .999
	if err := s.UpsertConceptState(ctx, cs); err != nil {
		t.Fatal(err)
	}
	badgeObservation(t, s, p, e, "numbers", models.ActivityPractice, start.Add(-time.Hour), time.Time{}, time.Time{}, true, 0)
	page, err := s.GetEnrollmentBadges(ctx, p, e.ID, "", 100, false)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("estimate/practice awarded: %+v %v", page, err)
	}
	first := badgeObservation(t, s, p, e, "numbers", models.ActivityMasteryChallenge, start, time.Time{}, time.Time{}, true, 0)
	page, err = s.GetEnrollmentBadges(ctx, p, e.ID, "", 100, false)
	if err != nil || len(page.Items) != 1 || page.Items[0].Kind != models.BadgeMastery || page.Items[0].Evidence[0].AttemptID != first {
		t.Fatalf("mastery: %+v %v", page, err)
	}
	badgeObservation(t, s, p, e, "equations", models.ActivityMasteryChallenge, start.Add(time.Minute), time.Time{}, time.Time{}, true, 0)
	page, err = s.GetEnrollmentBadges(ctx, p, e.ID, "", 100, false)
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("completion requires all concepts: %+v %v", page, err)
	}
	last := start
	for _, days := range []int{1, 3, 7, 30, 90} {
		at := start.Add(time.Duration(days) * 24 * time.Hour)
		badgeObservation(t, s, p, e, "numbers", models.ActivityRecall, at, last, at, true, 0)
		last = at
		page, err = s.GetEnrollmentBadges(ctx, p, e.ID, "", 100, false)
		if err != nil {
			t.Fatal(err)
		}
		want := 3
		if days >= 7 {
			want++
		}
		if days >= 30 {
			want++
		}
		if days >= 90 {
			want++
		}
		if len(page.Items) != want {
			t.Fatalf("day %d: got %d want %d: %+v", days, len(page.Items), want, page)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.awardInstitutionBadges(ctx, p.LearnerID, e.DomainID, "numbers", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	after := ""
	for {
		page, err := s.GetEnrollmentBadges(ctx, p, e.ID, after, 1, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range page.Items {
			if seen[b.ID] || b.EvidenceStatus != "valid" || b.VersionID != detail.Version.ID {
				t.Fatalf("bad provenance/replay: %+v", b)
			}
			seen[b.ID] = true
		}
		after = page.NextAfter
		if after == "" {
			break
		}
	}
	if len(seen) != 6 {
		t.Fatalf("pagination/replay: %d", len(seen))
	}
	other := formationStudent(t, s, "other-badges@test.com")
	if _, err := s.GetEnrollmentBadges(ctx, other, e.ID, "", 10, false); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("other learner: %v", err)
	}
	if _, err := s.GetEnrollmentBadges(ctx, progressReader(owner), e.ID, "", 10, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEnrollmentBadges(ctx, owner, e.ID, "", 10, true); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("missing progress scope: %v", err)
	}
	if _, _, err := s.LeaveFormation(ctx, p, "badge-leave", cohort.ID); err != nil {
		t.Fatal(err)
	}
	page, err = s.GetEnrollmentBadges(ctx, p, e.ID, "", 100, false)
	if err != nil || len(page.Items) != 6 {
		t.Fatalf("left enrollment lost badges: %v %v", page, err)
	}
	s.SetHostLLMDemonstrationPolicy(false)
	page, err = s.GetEnrollmentBadges(ctx, p, e.ID, "", 100, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range page.Items {
		if b.Kind != models.BadgeRetention && b.EvidenceStatus != "changed_or_unavailable" {
			t.Fatalf("changed trust silently retained: %+v", b)
		}
	}
	if _, err := s.SetMembershipAuthorization(ctx, p.TenantScope(), models.MembershipStatusSuspended, []string{models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEnrollmentBadges(ctx, p, e.ID, "", 10, false); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("revoked membership: %v", err)
	}
}

func TestInstitutionBadgesTrustAndTransactionRollback(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	_, cohort := seedFormationBridge(t, s, owner)
	p := formationStudent(t, s, "rollback-badges@test.com")
	e, err := s.EnrollMembership(ctx, owner, cohort.ID, p.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	s.SetHostLLMDemonstrationPolicy(false)
	badgeObservation(t, s, p, e, "numbers", models.ActivityMasteryChallenge, time.Now().Add(-time.Hour), time.Time{}, time.Time{}, true, 0)
	page, err := s.GetEnrollmentBadges(ctx, p, e.ID, "", 10, false)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("untrusted host awarded mastery")
	}
	s.SetHostLLMDemonstrationPolicy(true)
	sentinel := errors.New("rollback")
	err = s.WithTenantTx(ctx, p.TenantScope(), func(_ context.Context, tx storeport.Store) error {
		badgeObservation(t, tx.(*Store), p, e, "equations", models.ActivityMasteryChallenge, time.Now().Add(-time.Minute), time.Time{}, time.Time{}, true, 0)
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	page, err = s.GetEnrollmentBadges(ctx, p, e.ID, "", 10, false)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("rollback left badges: %+v %v", page, err)
	}
	badgeObservation(t, s, p, e, "equations", models.ActivityMasteryChallenge, time.Now().Add(-time.Second*3), time.Time{}, time.Time{}, false, 0)
	page, err = s.GetEnrollmentBadges(ctx, p, e.ID, "", 10, false)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("failed challenge completed formation: %+v %v", page, err)
	}
}

func TestRetentionBadgeRunRejectsFalseEvidence(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	good := func(day int, prior int) retentionBadgeObservation {
		at, last := start.Add(time.Duration(day)*24*time.Hour), start.Add(time.Duration(prior)*24*time.Hour)
		before, _ := json.Marshal(map[string]any{"last_review": last, "next_review": at})
		obs, _ := json.Marshal(map[string]any{"fsrs_update_applied": true, "fsrs_observation_at": at})
		after, _ := json.Marshal(map[string]any{"last_review": at, "stability": 10})
		return retentionBadgeObservation{proof: models.BadgeEvidence{AttemptID: fmt.Sprint(day), RespondedAt: at, EvaluatedAt: at.Add(time.Second)}, activity: string(models.ActivityRecall), beforeJSON: string(before), observationJSON: string(obs), afterJSON: string(after), protocol: models.LearningEventProtocol, priorExposure: &last, passed: true, valid: true}
	}
	for _, tc := range []struct {
		name   string
		change func(*retentionBadgeObservation)
	}{
		{"failure", func(o *retentionBadgeObservation) { o.passed = false; o.valid = false }},
		{"hints", func(o *retentionBadgeObservation) { o.hints = 1 }},
		{"unlinked", func(o *retentionBadgeObservation) { o.valid = false }},
		{"legacy_grading_time", func(o *retentionBadgeObservation) { o.protocol = "" }},
		{"early_review", func(o *retentionBadgeObservation) {
			o.beforeJSON = `{"last_review":"2026-01-04T00:00:00Z","next_review":"2026-02-01T00:00:00Z"}`
		}},
		{"immediate_exposure", func(o *retentionBadgeObservation) {
			prior := o.proof.RespondedAt.Add(-time.Hour)
			o.priorExposure = &prior
		}},
		{"grading_delay", func(o *retentionBadgeObservation) { o.proof.RespondedAt = start.Add(time.Hour) }},
		{"duplicate", func(o *retentionBadgeObservation) { o.proof.AttemptID = "3" }},
		{"fsrs_skipped", func(o *retentionBadgeObservation) { o.observationJSON = `{"fsrs_update_applied":false}` }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observations := []retentionBadgeObservation{good(1, 0), good(3, 1), good(7, 3)}
			tc.change(&observations[2])
			proofs, _ := retentionBadgeRun(observations, start)
			if len(proofs) >= 3 {
				t.Fatal("unsupported third recall accepted")
			}
		})
	}
	observations := []retentionBadgeObservation{good(1, 0), good(3, 1), good(7, 3)}
	proofs, from := retentionBadgeRun(observations, start)
	if len(proofs) != 3 || proofs[2].RespondedAt.Sub(from) != 7*24*time.Hour {
		t.Fatal("seven-day boundary rejected")
	}
	_, from = retentionBadgeRun(observations, start.Add(24*time.Hour))
	if !from.Equal(start.Add(24 * time.Hour)) {
		t.Fatal("inherited FSRS review lengthened the new enrollment period")
	}
	failure := good(8, 7)
	failure.passed = false
	failure.valid = false
	observations = append(observations, failure, good(90, 8))
	proofs, from = retentionBadgeRun(observations, start)
	if len(proofs) != 1 || !from.Equal(start.Add(8*24*time.Hour)) {
		t.Fatal("failure did not restart run")
	}
}

func TestInstitutionBadgesPostgresForcedRLS(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("requires PostgreSQL")
	}
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	_, cohort := seedFormationBridge(t, s, owner)
	p := formationStudent(t, s, "rls-badges@test.com")
	e, err := s.EnrollMembership(ctx, owner, cohort.ID, p.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := s.root.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf(`"tutor_badges_rls_%d"`, testDBCounter.Add(1))
	if _, err := s.root.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.root.ExecContext(context.Background(), `DROP OWNED BY `+role)
		_, _ = s.root.ExecContext(context.Background(), `DROP ROLE `+role)
	}()
	for _, grant := range []string{`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role, `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + role} {
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
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', $1, true)`, p.TenantID); err != nil {
		t.Fatal(err)
	}
	scope := p.TenantScope()
	limited := &Store{db: tx, dialect: DialectPostgres, tenantScope: &scope, hostLLMDemonstrates: true}
	badgeObservation(t, limited, p, e, "numbers", models.ActivityMasteryChallenge, time.Now().Add(-time.Minute), time.Time{}, time.Time{}, true, 0)
	page, err := limited.GetEnrollmentBadges(ctx, p, e.ID, "", 10, false)
	if err != nil || len(page.Items) != 1 || page.Items[0].EvidenceStatus != "valid" {
		t.Fatalf("RLS award/read: %+v %v", page, err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', 'foreign', true)`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"learning_badges", "learning_badge_evidence"} {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("RLS read %s: %d %v", table, n, err)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM `+table)
		if err != nil {
			t.Fatal(err)
		}
		affected, _ := result.RowsAffected()
		if affected != 0 {
			t.Fatal("foreign badge deletion")
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO learning_badge_evidence (tenant_id,badge_id,learner_id,attempt_id,concept_id,snapshot_id,responded_at,evaluated_at,evaluation_method) VALUES ($1,$2,$3,'forged','concept',0,$4,$4,'host_llm')`, p.TenantID, page.Items[0].ID, p.LearnerID, time.Now()); err == nil {
		t.Fatal("cross-tenant evidence insertion")
	}
}

func TestInstitutionBadgesAdjudicationMigrationAndErasure(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	owner := ownerPrincipal(t, s)
	detail, cohort := seedFormationBridge(t, s, owner)
	p := formationStudent(t, s, "history-badges@test.com")
	e, err := s.EnrollMembership(ctx, owner, cohort.ID, p.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	s.SetHostLLMDemonstrationPolicy(false)
	id := badgeObservation(t, s, p, e, "numbers", models.ActivityMasteryChallenge, time.Now().Add(-time.Minute), time.Time{}, time.Time{}, true, 0)
	review := recordReviewForTest(t, s, reviewWriter(owner), id, "badge-review")
	adjudicator, sign := adjudicatorForTest(t, s, owner.TenantID)
	if _, _, err := adjudicator.AdjudicateAssessment(ctx, reviewWriter(owner), id, sign(review, "badge-accept", "accept", 0)); err != nil {
		t.Fatal(err)
	}
	page, err := s.GetEnrollmentBadges(ctx, p, e.ID, "", 10, false)
	if err != nil || len(page.Items) != 1 || page.Items[0].Evidence[0].EvaluationMethod != models.EvaluationMethodExternal || page.Items[0].EvidenceStatus != "valid" {
		t.Fatalf("external badge: %+v %v", page, err)
	}
	if _, _, err := adjudicator.AdjudicateAssessment(ctx, reviewWriter(owner), id, sign(review, "badge-reject", "reject", 1)); err != nil {
		t.Fatal(err)
	}
	page, err = s.GetEnrollmentBadges(ctx, p, e.ID, "", 10, false)
	if err != nil || len(page.Items) != 1 || page.Items[0].EvidenceStatus != "changed_or_unavailable" {
		t.Fatalf("withdrawn evidence hidden: %+v %v", page, err)
	}
	clone, err := s.CloneFormationVersion(ctx, owner, detail.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishFormationVersion(ctx, owner, clone.Version.ID); err != nil {
		t.Fatal(err)
	}
	next, err := s.CreateCohort(ctx, owner, clone.Version.ID, "Next version", 5, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := s.MigrateFormationEnrollment(ctx, owner, e.ID, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	page, err = s.GetEnrollmentBadges(ctx, p, migration.Enrollment.ID, "", 10, false)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("migration inherited badges: %+v %v", page, err)
	}
	page, err = s.GetEnrollmentBadges(ctx, p, e.ID, "", 10, false)
	if err != nil || len(page.Items) != 1 || page.Items[0].VersionID != detail.Version.ID {
		t.Fatal("historical award moved version")
	}
	request, err := s.RequestTenantDSAR(ctx, owner, p.LearnerID, "erase", "learner request")
	if err != nil {
		t.Fatal(err)
	}
	// A pending request from before the upgrade must acquire the new phases.
	if _, err := s.exec(ctx, `DELETE FROM tenant_dsar_phases WHERE tenant_id = ? AND request_id = ? AND phase IN ('learning_badges','learning_badge_evidence')`, p.TenantID, request.ID); err != nil {
		t.Fatal(err)
	}
	worker := models.WorkerPrincipal{ActorID: "badge-erasure-worker"}
	scope := p.TenantScope()
	scope.UserID, scope.MembershipID = "worker_"+worker.ActorID, "worker_process"
	complete := false
	for batch := 0; batch < 100 && !complete; batch++ {
		complete, _, err = s.ProcessTenantDSARErasureBatch(ctx, scope, worker, request.ID, 10, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
	}
	if !complete {
		t.Fatal("erasure did not finish")
	}
	for _, table := range []string{"learning_badges", "learning_badge_evidence"} {
		var count int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM `+table+` WHERE learner_id = ?`, p.LearnerID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("badge data not erased: %s %d %v", table, count, err)
		}
	}
}
