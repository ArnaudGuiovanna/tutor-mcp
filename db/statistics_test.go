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

type statisticsCohort struct {
	s       *Store
	owner   models.Principal
	reader  models.Principal
	cohort  *models.Cohort
	version string
	members []models.Principal
	enrolls []*models.Enrollment
}

func (c *statisticsCohort) add(t *testing.T, mastery float64) {
	t.Helper()
	student := formationStudent(t, c.s, fmt.Sprintf("stat%d@statistics.test", len(c.members)))
	e, err := c.s.EnrollMembership(t.Context(), c.owner, c.cohort.ID, student.MembershipID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	state := models.NewConceptStateInDomain(student.LearnerID, e.DomainID, "numbers")
	state.PMastery, state.Reps = mastery, 3
	if err := c.s.UpsertConceptState(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	c.members, c.enrolls = append(c.members, student), append(c.enrolls, e)
}

func (c *statisticsCohort) recompute(t *testing.T) {
	t.Helper()
	if _, err := c.s.RecomputeInstitutionStatistics(t.Context(), c.owner.TenantScope(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func (c *statisticsCohort) read(t *testing.T) *models.CohortStatistics {
	t.Helper()
	out, err := c.s.GetCohortStatistics(t.Context(), c.reader, c.cohort.ID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func newStatisticsCohort(t *testing.T) *statisticsCohort {
	s := setupTestDB(t)
	owner := ownerPrincipal(t, s)
	detail, cohort := seedFormationBridge(t, s, owner)
	return &statisticsCohort{s: s, owner: owner, reader: progressReader(owner), cohort: cohort, version: detail.Version.ID}
}

func near(v *float64, want float64) bool { return v != nil && math.Abs(*v-want) < 1e-9 }

func TestStatisticsNotYetComputedStaleAndThreshold(t *testing.T) {
	c := newStatisticsCohort(t)
	if out := c.read(t); out.Status != models.StatisticsNotYetComputed || out.ComputedAt != nil || out.Contributors.Value != nil {
		t.Fatalf("never computed: %+v", out)
	}
	for i, mastery := range []float64{.1, .2, .3, .4} {
		c.add(t, mastery)
		c.recompute(t)
		out := c.read(t)
		if i == 3 {
			if out.Status != models.StatisticsInsufficientData || out.ComputedAt == nil || out.Stale {
				t.Fatalf("four learners: %+v", out)
			}
			for _, cell := range []models.StatInt{out.Contributors, out.Participants, out.CompletedLearners} {
				if cell.Value != nil || cell.Status != models.StatisticsInsufficientData {
					t.Fatalf("below-threshold cell: %+v", out)
				}
			}
			if out.MeanMastery.Value != nil || out.MedianMastery.Value != nil || len(out.Concepts) != 0 || len(out.Badges) != 0 {
				t.Fatalf("below-threshold cohort leaked figures: %+v", out)
			}
		}
	}
	c.add(t, .9)
	c.recompute(t)
	out := c.read(t)
	if out.Status != models.StatisticsAvailable || out.Contributors.Value == nil || *out.Contributors.Value != 5 ||
		!near(out.MeanMastery.Value, .38) || !near(out.MedianMastery.Value, .3) {
		t.Fatalf("five learners: %+v", out)
	}
	if out.SynthesisGuidance == "" || out.PolicyVersion != models.StatisticsPolicyVersion || out.MinimumContributors != 5 {
		t.Fatalf("provenance: %+v", out)
	}
	seen := 0
	for _, concept := range out.Concepts {
		seen++
		switch concept.StableKey {
		case "numbers":
			if concept.ObservedLearners.Value == nil || *concept.ObservedLearners.Value != 5 || !near(concept.MeanMastery.Value, .38) {
				t.Fatalf("numbers: %+v", concept)
			}
		case "equations":
			if concept.ObservedLearners.Value != nil || concept.MeanMastery.Value != nil || concept.MedianMastery.Value != nil ||
				concept.ObservedLearners.Status != models.StatisticsInsufficientData {
				t.Fatalf("unobserved concept must be suppressed, not zero: %+v", concept)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("concepts: %d", seen)
	}
	// Recomputation is idempotent and refreshes the timestamp.
	first := *out.ComputedAt
	time.Sleep(10 * time.Millisecond)
	c.recompute(t)
	again := c.read(t)
	if !again.ComputedAt.After(first) || *again.Contributors.Value != 5 || !near(again.MeanMastery.Value, .38) {
		t.Fatalf("idempotent recompute: %+v", again)
	}
	var rows int
	if err := c.s.queryRow(t.Context(), `SELECT COUNT(*) FROM cohort_statistics WHERE tenant_id = ?`, c.owner.TenantID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("duplicate snapshot rows: %d %v", rows, err)
	}
	if _, err := c.s.exec(t.Context(), `UPDATE cohort_statistics SET computed_at = ? WHERE tenant_id = ?`, time.Now().UTC().Add(-7*time.Hour), c.owner.TenantID); err != nil {
		t.Fatal(err)
	}
	if out := c.read(t); !out.Stale || out.Status != models.StatisticsAvailable {
		t.Fatalf("stale snapshot: %+v", out)
	}
}

func TestStatisticsComplementarySuppressionAndBadges(t *testing.T) {
	c := newStatisticsCohort(t)
	for i := 0; i < 6; i++ {
		c.add(t, .5)
	}
	setCompleted := func(n int) {
		t.Helper()
		for i, e := range c.enrolls {
			status := "active"
			if i < n {
				status = "completed"
			}
			if _, err := c.s.exec(t.Context(), `UPDATE enrollments SET status = ? WHERE tenant_id = ? AND id = ?`, status, c.owner.TenantID, e.ID); err != nil {
				t.Fatal(err)
			}
		}
		c.recompute(t)
	}
	for _, tc := range []struct {
		completed int
		want      *int
	}{{0, intPtr(0)}, {1, nil}, {4, nil}, {5, nil}, {6, intPtr(6)}} {
		setCompleted(tc.completed)
		got := c.read(t).CompletedLearners
		if (got.Value == nil) != (tc.want == nil) || (tc.want != nil && *got.Value != *tc.want) {
			t.Fatalf("completed=%d of 6: %+v", tc.completed, got)
		}
		if tc.want == nil && got.Status != models.StatisticsInsufficientData {
			t.Fatalf("suppressed cell must say so: %+v", got)
		}
	}
	setCompleted(0)
	award := func(i int, kind string, days int) {
		t.Helper()
		e := c.enrolls[i]
		if _, err := c.s.exec(t.Context(), `INSERT INTO learning_badges
 (id, tenant_id, learner_id, enrollment_id, formation_version_id, concept_id, label, kind, retention_days, policy_version, achieved_at, recorded_at)
 VALUES (?, ?, ?, ?, ?, ?, 'x', ?, ?, 'p', ?, ?)`,
			fmt.Sprintf("%s-%s-%d", e.ID, kind, days), c.owner.TenantID, c.members[i].LearnerID, e.ID, c.version, map[bool]string{true: "", false: "c"}[kind == models.BadgeFormation], kind, days, time.Now().UTC(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ { // five of six: the one non-holder must stay hidden
		award(i, models.BadgeMastery, 0)
	}
	for i := 0; i < 6; i++ {
		award(i, models.BadgeFormation, 0)
	}
	award(0, models.BadgeRetention, 7)
	c.recompute(t)
	byKey := map[string]models.StatInt{}
	for _, b := range c.read(t).Badges {
		byKey[fmt.Sprintf("%s/%d", b.Kind, b.RetentionDays)] = b.Learners
	}
	if len(byKey) != 5 {
		t.Fatalf("badge cells: %v", byKey)
	}
	if v := byKey["mastery/0"]; v.Value != nil || v.Status != models.StatisticsInsufficientData {
		t.Fatalf("5 of 6 holders reveals a lone non-holder: %+v", v)
	}
	if v := byKey["formation_completed/0"]; v.Value == nil || *v.Value != 6 {
		t.Fatalf("all six holders is not a small group: %+v", v)
	}
	if v := byKey["retention/7"]; v.Value != nil {
		t.Fatalf("a single holder must be suppressed: %+v", v)
	}
	if v := byKey["retention/30"]; v.Value == nil || *v.Value != 0 {
		t.Fatalf("no holder is a true zero: %+v", v)
	}
}

func intPtr(n int) *int { return &n }

func TestStatisticsFallingUnderThresholdOverwritesFigures(t *testing.T) {
	c := newStatisticsCohort(t)
	for i := 0; i < 5; i++ {
		c.add(t, .6)
	}
	c.recompute(t)
	if c.read(t).Contributors.Value == nil {
		t.Fatal("five learners must be reported")
	}
	if _, _, err := c.s.LeaveFormation(t.Context(), c.members[4], "stat-leave", c.cohort.ID); err != nil {
		t.Fatal(err)
	}
	c.recompute(t)
	out := c.read(t)
	if out.Status != models.StatisticsInsufficientData || out.Contributors.Value != nil || out.MeanMastery.Value != nil || len(out.Concepts) != 0 {
		t.Fatalf("departure must drop the cohort under the threshold: %+v", out)
	}
	var stored int
	if err := c.s.queryRow(t.Context(), `SELECT COUNT(*) FROM cohort_concept_statistics WHERE tenant_id = ? AND (observed_learners IS NOT NULL OR mean_mastery IS NOT NULL)`, c.owner.TenantID).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("stale figures remained stored: %d %v", stored, err)
	}
}

func TestStatisticsAuthorizationAndPaging(t *testing.T) {
	c := newStatisticsCohort(t)
	for i := 0; i < 5; i++ {
		c.add(t, .6)
	}
	c.recompute(t)
	page, err := c.s.GetCohortStatistics(t.Context(), c.reader, c.cohort.ID, "", 1)
	if err != nil || len(page.Concepts) != 1 || page.NextConceptAfter == "" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	next, err := c.s.GetCohortStatistics(t.Context(), c.reader, c.cohort.ID, page.NextConceptAfter, 1)
	if err != nil || len(next.Concepts) != 1 || next.Concepts[0].ConceptID == page.Concepts[0].ConceptID || next.NextConceptAfter != "" {
		t.Fatalf("second page: %+v %v", next, err)
	}
	if _, err := c.s.GetCohortStatistics(t.Context(), c.reader, c.cohort.ID, "", 101); !errors.Is(err, storeport.ErrInvalidProgressRequest) {
		t.Fatalf("unbounded page: %v", err)
	}
	trainer := progressReader(reviewRoleForTest(t, c.s, "stats-trainer", models.RoleTrainer))
	if _, err := c.s.GetCohortStatistics(t.Context(), trainer, c.cohort.ID, "", 10); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("unassigned trainer: %v", err)
	}
	if err := c.s.AssignCohortTrainer(t.Context(), c.owner, c.cohort.ID, trainer.MembershipID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.GetCohortStatistics(t.Context(), trainer, c.cohort.ID, "", 10); err != nil {
		t.Fatal(err)
	}
	if err := c.s.SetCohortTrainerAssignment(t.Context(), c.owner, c.cohort.ID, "stats-trainer@test.com", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.GetCohortStatistics(t.Context(), trainer, c.cohort.ID, "", 10); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("revoked trainer: %v", err)
	}
	for _, scope := range []string{models.OAuthScopeLearner, models.OAuthScopeFormationRead} {
		bad := c.reader
		bad.Scopes = []string{scope}
		if _, err := c.s.GetCohortStatistics(t.Context(), bad, c.cohort.ID, "", 10); !errors.Is(err, storeport.ErrInvalidPrincipal) {
			t.Fatalf("scope %s: %v", scope, err)
		}
	}
	if _, err := c.s.GetCohortStatistics(t.Context(), progressReader(c.members[0]), c.cohort.ID, "", 10); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("learner read: %v", err)
	}
	if _, err := c.s.GetCohortStatistics(t.Context(), c.reader, "missing", "", 10); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("missing cohort: %v", err)
	}
}
