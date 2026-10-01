// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"sort"
	"testing"

	"tutor-mcp/models"
)

// Tenant tables whose rows a restore verification deliberately does not
// checksum. A new tenant table must be added to tenantChecksumSpecs or listed
// here with its reason, so a backup can never silently stop being verified.
var restoreChecksumExempt = map[string]string{
	// Derived data, recomputed by the worker after a restore.
	"cohort_statistics": "derived snapshot", "cohort_concept_statistics": "derived snapshot", "cohort_badge_statistics": "derived snapshot",
	"usage_rollups": "derived from usage_events, which is checksummed",
	// Short-lived credentials and sessions: a restore must not resurrect them.
	"account_tokens": "transient credential", "console_sessions": "transient credential", "login_challenges": "transient credential",
	"oauth_codes": "transient credential", "refresh_tokens": "transient credential", "learner_approved_clients": "re-consented after restore",
	"tool_call_idempotency": "transient replay cache",
	// Queues and delivery state: re-drained or re-run after a restore.
	"async_jobs": "queue", "outbox_events": "queue", "webhook_message_queue": "queue", "webhook_push_log": "delivery log",
	"webhook_delivery_transitions": "delivery log", "scheduled_alerts": "queue", "pending_consolidations": "queue",
	"worker_tenant_runs": "operational log", "narrative_mutations": "journal; narrative_objects is checksummed",
	// Routing, control-plane, commercial and compliance records with their own audit trail.
	"credential_tenant_routes": "routing digest", "invitation_tenant_routes": "routing digest", "service_account_routes": "routing digest",
	"support_access_routes": "routing digest", "service_accounts": "control plane", "support_access_grants": "control plane",
	"federated_identity_links": "control plane", "tenant_identity_providers": "control plane", "tenant_domains": "control plane",
	"tenant_integrations": "control plane", "tenant_integration_secret_versions": "secrets, restored from the keyring",
	"tenant_entitlements": "commercial", "tenant_feature_flags": "commercial", "tenant_subscriptions": "commercial",
	"entitlement_reservations": "commercial", "usage_corrections": "commercial", "billing_provider_events": "commercial",
	"tenant_retention_policies": "compliance", "retention_legal_holds": "compliance", "tenant_dsar_requests": "compliance",
	"tenant_dsar_phases": "compliance", "tenant_restore_manifests": "the manifest itself", "audit_events": "append-only audit trail",
	"catalog_admin_mutations": "audit-like journal", "tenant_invitations": "transient invitation",
	// Identifier mappings and per-learner records that are rebuilt from checksummed rows.
	"legacy_concept_mappings": "identifier mapping", "legacy_concept_sources": "identifier mapping", "legacy_domain_enrollments": "identifier mapping",
	"curriculum_concepts": "identities of checksummed curriculum_versions", "curriculum_metadata_ids": "identities of checksummed curriculum_versions",
	"curriculum_review_opinions": "review journal", "pedagogical_snapshots": "frozen decision context of checksummed decisions",
	"implementation_intentions": "learner plan, journal-like", "availability": "learner preference", "learners": "identity skeleton, verified through memberships",
}

func TestEveryTenantTableIsRestoreCheckedOrExplicitlyExempt(t *testing.T) {
	s := setupTestDB(t)
	rows, err := s.query(t.Context(), `SELECT DISTINCT m.name FROM sqlite_master m, pragma_table_info(m.name) p
 WHERE m.type = 'table' AND p.name = 'tenant_id' ORDER BY m.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	checked := map[string]bool{}
	for _, spec := range tenantChecksumSpecs {
		checked[spec.table] = true
	}
	var missing []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		if _, exempt := restoreChecksumExempt[table]; !checked[table] && !exempt {
			missing = append(missing, table)
		}
		if _, exempt := restoreChecksumExempt[table]; checked[table] && exempt {
			t.Fatalf("%s is both checksummed and exempt", table)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Fatalf("tenant tables are neither restore-verified nor justified: %v", missing)
	}
}

// The expanded checksum set must actually detect the loss of learning records.
func TestRestoreChecksumsDetectLostLearningRecords(t *testing.T) {
	c := newStatisticsCohort(t)
	c.add(t, .7)
	operator := models.ControlPlanePrincipal{ActorID: "restore-coverage", Roles: []string{models.RolePlatformAdmin}, Reason: "restore exercise", RequestID: "restore-coverage-1"}
	ctx := context.Background()
	tables, objects, err := c.s.ComputeTenantChecksums(ctx, operator, c.owner.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := c.s.RequestTenantRestoreVerification(ctx, operator, c.owner.TenantID, "backup-coverage", tables, objects)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.exec(ctx, `DELETE FROM concept_states WHERE tenant_id = ?`, c.owner.TenantID); err != nil {
		t.Fatal(err)
	}
	if matched, err := c.s.VerifyTenantRestore(ctx, operator, c.owner.TenantID, manifest.ID); err != nil || matched {
		t.Fatalf("a restore missing learner concept states must not verify: matched=%v err=%v", matched, err)
	}
}
