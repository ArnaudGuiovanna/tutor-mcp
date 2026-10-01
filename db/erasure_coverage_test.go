// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"sort"
	"testing"
	"time"

	"tutor-mcp/models"
)

// Tables that carry a learner identifier but are intentionally not deleted by
// an erasure request. A new table with a learner_id column must either be
// erased by dsarLearnerTables or be listed here with its reason.
var erasureRetainedByDesign = map[string]string{
	"learners":                  "identity skeleton: email, password, objective, profile and webhook are scrubbed",
	"enrollments":               "relational skeleton: objectives are scrubbed and the enrollment is cancelled",
	"domains":                   "relational skeleton: name, goal and graph are scrubbed and the domain is archived",
	"tenant_memberships":        "tenant membership skeleton, no learner content",
	"tenant_dsar_requests":      "the erasure request itself is the accountability record",
	"retention_legal_holds":     "a legal hold must outlive an erasure request",
	"legacy_domain_enrollments": "identifier mapping only",
	"legacy_concept_sources":    "identifier mapping only",
	"credential_tenant_routes":  "digest-only routing row, no learner content",
	// Append-only by trigger: PostgreSQL and SQLite refuse their deletion. They
	// hold the curriculum structure (concept keys, outcomes) of a domain that is
	// archived and whose name and goal are scrubbed. Curriculum text typed by a
	// learner in a self-created domain is not erased; see docs/installation.md.
	"curriculum_versions":     "immutable curriculum history, see above",
	"curriculum_concepts":     "immutable curriculum identities, see above",
	"curriculum_metadata_ids": "immutable curriculum identities, see above",
	"hobby_account_links":     "hobby profile only; institution tenants never create them",
}

func TestEveryLearnerTableIsErasedOrExplicitlyRetained(t *testing.T) {
	s := setupTestDB(t)
	rows, err := s.query(t.Context(), `SELECT DISTINCT m.name FROM sqlite_master m, pragma_table_info(m.name) p
 WHERE m.type = 'table' AND p.name = 'learner_id' ORDER BY m.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		if _, erased := dsarLearnerTables[table]; erased {
			continue
		}
		if _, kept := erasureRetainedByDesign[table]; kept {
			continue
		}
		missing = append(missing, table)
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Fatalf("tables holding learner data are neither erased nor justified: %v", missing)
	}
	for table := range erasureRetainedByDesign {
		if _, erased := dsarLearnerTables[table]; erased {
			t.Fatalf("%s is both erased and retained", table)
		}
	}
	phases := map[string]bool{"scrub_learner": true}
	for _, phase := range dsarErasurePhases {
		phases[phase] = true
	}
	for table := range dsarLearnerTables {
		if !phases[table] {
			t.Fatalf("%s has a column mapping but no erasure phase", table)
		}
	}
	for _, phase := range dsarLatePhases {
		if !phases[phase] {
			t.Fatalf("late phase %s is not part of the erasure order", phase)
		}
	}
}

func TestErasureRemovesEnrollmentStateCredentialsAndStopsCountingTheLearner(t *testing.T) {
	c := newStatisticsCohort(t)
	for i := 0; i < 5; i++ {
		c.add(t, .6)
	}
	c.recompute(t)
	if out := c.read(t); out.Contributors.Value == nil || out.Status != models.StatisticsAvailable {
		t.Fatalf("five learners must be reported before the erasure: %+v", out)
	}
	victim, enrollment := c.members[0], c.enrolls[0]
	if _, err := c.s.exec(t.Context(), `UPDATE enrollments SET objectives_json = '{"goal":"private objective"}' WHERE tenant_id = ? AND id = ?`, c.owner.TenantID, enrollment.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.CreateRefreshToken(t.Context(), victim.LearnerID, "client-1", "https://resource.test"); err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := c.s.queryRow(t.Context(), `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = ? AND learner_id = ?`, c.owner.TenantID, victim.LearnerID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("learner_concept_states") == 0 || count("refresh_tokens") == 0 {
		t.Fatal("fixture must hold canonical state and a credential")
	}
	request, err := c.s.RequestTenantDSAR(t.Context(), c.owner, victim.LearnerID, "erase", "learner request")
	if err != nil {
		t.Fatal(err)
	}
	worker := models.WorkerPrincipal{ActorID: "erasure-coverage-worker"}
	scope := c.owner.TenantScope()
	scope.UserID, scope.MembershipID = "worker_"+worker.ActorID, "worker_process"
	done := false
	for batch := 0; batch < 200 && !done; batch++ {
		if done, _, err = c.s.ProcessTenantDSARErasureBatch(t.Context(), scope, worker, request.ID, 10, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if !done {
		t.Fatal("erasure did not finish")
	}
	for table := range dsarLearnerTables {
		if n := count(table); n != 0 {
			t.Fatalf("%s still holds %d rows of the erased learner", table, n)
		}
	}
	var status, objectives string
	var seat int
	if err := c.s.queryRow(t.Context(), `SELECT status, objectives_json, seat_reserved FROM enrollments WHERE tenant_id = ? AND id = ?`, c.owner.TenantID, enrollment.ID).Scan(&status, &objectives, &seat); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || objectives != "{}" || seat != 0 {
		t.Fatalf("enrollment skeleton: %s %s %d", status, objectives, seat)
	}
	var name, goal string
	var deleted *time.Time
	if err := c.s.queryRow(t.Context(), `SELECT name, personal_goal, deleted_at FROM domains WHERE tenant_id = ? AND id = ?`, c.owner.TenantID, enrollment.DomainID).Scan(&name, &goal, &deleted); err != nil {
		t.Fatal(err)
	}
	if name != "erased" || goal != "" || deleted == nil {
		t.Fatalf("domain was not scrubbed: %q %q %v", name, goal, deleted)
	}
	// No hourly wait: the aggregates already exclude the erased learner, and the
	// cohort is back under the anonymity threshold.
	if out := c.read(t); out.Status != models.StatisticsInsufficientData || out.Contributors.Value != nil {
		t.Fatalf("erased learner still counted in the cohort statistics: %+v", out)
	}
}
