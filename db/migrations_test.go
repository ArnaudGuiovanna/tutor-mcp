package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
)

// TestMigrate_Idempotent runs Migrate twice on a fresh in-memory database;
// the second invocation must be a no-op (no error, no duplicate-table or
// duplicate-column errors propagated). Then we assert that all expected
// tables and indexes exist by querying sqlite_master.
func TestMigrate_Idempotent(t *testing.T) {
	n := testDBCounter.Add(1)
	dsn := fmt.Sprintf("file:migrate_idempo_%d?mode=memory&cache=shared", n+10000)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate (must be idempotent): %v", err)
	}

	expectedTables := []string{
		"learners",
		"refresh_tokens",
		"domains",
		"concept_states",
		"interactions",
		"availability",
		"scheduled_alerts",
		"oauth_codes",
		"oauth_clients",
		"affect_states",
		"calibration_records",
		"transfer_records",
		"implementation_intentions",
		"learning_sessions",
		"webhook_message_queue",
		"webhook_push_log",
		"pedagogical_snapshots",
		"learner_approved_clients",
		"curriculum_versions",
		"curriculum_concepts",
		"curriculum_metadata_ids",
		"account_tokens",
	}
	for _, table := range expectedTables {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("expected table %q to exist: %v", table, err)
		}
	}

	expectedIndexes := []string{
		"idx_concept_states_learner",
		"idx_concept_states_review",
		"idx_interactions_learner_created",
		"idx_interactions_learner_concept",
		"idx_scheduled_alerts_learner_type",
		"idx_oauth_codes_expires",
		"idx_oauth_clients_expires_at",
		"idx_account_tokens_learner_purpose",
		"idx_affect_states_learner",
		"idx_calibration_records_learner",
		"idx_transfer_records_learner_concept",
		"idx_interactions_self_initiated",
		"idx_interactions_misconception",
		"idx_impl_intent_learner",
		"idx_learning_sessions_one_open",
		"idx_learning_sessions_learner_started",
		"idx_interactions_learner_session",
		"idx_impl_intent_learner_status",
		"idx_impl_intent_session",
		"idx_impl_intent_one_per_session",
		"idx_refresh_tokens_client_resource",
		"idx_domains_learner_high_stakes",
		"idx_assessment_attempts_human_review",
		"idx_wmq_dispatch",
		"idx_wmq_retry_dispatch",
		"idx_wmq_domain_active",
		"idx_webhook_push_log_open",
		"idx_pedagogical_snapshots_learner_created",
		"idx_pedagogical_snapshots_domain_concept",
		"idx_learners_email_lower",
		"idx_domains_learner_deleted",
		"idx_curriculum_versions_learner_domain",
		"idx_curriculum_concepts_learner_domain",
		"idx_curriculum_metadata_learner_domain",
	}
	for _, idx := range expectedIndexes {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name = ?`, idx,
		).Scan(&name)
		if err != nil {
			t.Errorf("expected index %q to exist: %v", idx, err)
		}
	}

	// Sanity: all migrated columns are queryable.
	if _, err := db.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, profile_json) VALUES ('m1','m@m','h','o','{}')`,
	); err != nil {
		t.Fatalf("insert with profile_json: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO interactions (learner_id, concept, activity_type, success, error_type, hints_requested, self_initiated, calibration_id, is_proactive_review, misconception_type, misconception_detail) VALUES ('m1','C','RECALL_EXERCISE',1,'',0,0,'',0,NULL,NULL)`,
	); err != nil {
		t.Fatalf("insert with v0.9/v0.10 columns: %v", err)
	}
	var interactionID int64
	if err := db.QueryRow(`SELECT id FROM interactions WHERE learner_id = 'm1'`).Scan(&interactionID); err != nil {
		t.Fatalf("read interaction id: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO pedagogical_snapshots (interaction_id, learner_id, domain_id, concept, activity_type, before_json, observation_json, after_json, decision_json, interpretation_brief)
		 VALUES (?, 'm1', 'd1', 'C', 'RECALL_EXERCISE', '{}', '{}', '{}', '{}', 'brief')`,
		interactionID,
	); err != nil {
		t.Fatalf("insert pedagogical snapshot with interpretation_brief: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO domains (id, learner_id, name, graph_json, personal_goal, archived, value_framings_json, last_value_axis) VALUES ('d1','m1','dn','{}','goal',0,'','')`,
	); err != nil {
		t.Fatalf("insert with domain framing columns: %v", err)
	}
}

func TestMigrationRevokesRefreshTokensWithoutClientOrFamilyBinding(t *testing.T) {
	n := testDBCounter.Add(1)
	dsn := fmt.Sprintf("file:migrate_unbound_refresh_%d?mode=memory&cache=shared", n+15000)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := ensureSchemaMigrationsTable(raw); err != nil {
		t.Fatal(err)
	}

	var revokeMigration migration
	for _, m := range buildMigrations() {
		if m.Version == "0029_revoke_unbound_refresh_tokens" {
			revokeMigration = m
			break
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %s: %v", m.Version, err)
		}
	}
	if revokeMigration.Version == "" {
		t.Fatal("refresh-token revocation migration not found")
	}

	now := time.Now().UTC()
	if _, err := raw.Exec(
		`INSERT INTO learners (id, email, password_hash, objective) VALUES ('legacy-refresh','legacy-refresh@test','h','o')`,
	); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		token, clientID, familyID string
	}{
		{token: "sha256:missing-client", familyID: "compromised-client-family"},
		{token: "sha256:adopted-descendant", clientID: "client-A", familyID: "compromised-client-family"},
		{token: "sha256:missing-family", clientID: "client-A"},
		{token: "legacy-plaintext", clientID: "client-A", familyID: "compromised-plaintext-family"},
		{token: "sha256:plaintext-descendant", clientID: "client-A", familyID: "compromised-plaintext-family"},
		{token: "sha256:fully-bound", clientID: "client-A", familyID: "fully-bound"},
	} {
		if _, err := raw.Exec(
			`INSERT INTO refresh_tokens
			 (token, learner_id, client_id, family_id, expires_at, created_at)
			 VALUES (?, 'legacy-refresh', ?, ?, ?, ?)`,
			row.token, row.clientID, row.familyID, now.Add(time.Hour), now,
		); err != nil {
			t.Fatalf("seed %s: %v", row.token, err)
		}
	}
	if err := applyMigration(raw, revokeMigration); err != nil {
		t.Fatal(err)
	}

	for _, token := range []string{
		"sha256:missing-client", "sha256:adopted-descendant", "sha256:missing-family",
		"legacy-plaintext", "sha256:plaintext-descendant",
	} {
		var revoked sql.NullTime
		if err := raw.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE token = ?`, token).Scan(&revoked); err != nil || !revoked.Valid {
			t.Fatalf("token %s was not revoked: revoked=%v err=%v", token, revoked, err)
		}
	}
	var validRevoked sql.NullTime
	if err := raw.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE token = 'sha256:fully-bound'`).Scan(&validRevoked); err != nil {
		t.Fatal(err)
	}
	if validRevoked.Valid {
		t.Fatal("fully bound refresh token was revoked")
	}
}

func TestMigrationPurgesAuthorizationCodesWithoutExactS256Binding(t *testing.T) {
	n := testDBCounter.Add(1)
	dsn := fmt.Sprintf("file:migrate_unbound_codes_%d?mode=memory&cache=shared", n+16000)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := ensureSchemaMigrationsTable(raw); err != nil {
		t.Fatal(err)
	}

	var purge migration
	for _, m := range buildMigrations() {
		if m.Version == "0031_purge_unbound_oauth_codes" {
			purge = m
			break
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %s: %v", m.Version, err)
		}
	}
	if purge.Version == "" {
		t.Fatal("authorization-code purge migration not found")
	}

	if _, err := raw.Exec(
		`INSERT INTO learners (id, email, password_hash, objective) VALUES ('legacy-code','legacy-code@test','h','o')`,
	); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	rows := []struct {
		code, challenge, method, clientID, redirectURI string
	}{
		{"valid", "challenge", "S256", "client-A", "https://client.test/callback"},
		{"missing-client", "challenge", "S256", "", "https://client.test/callback"},
		{"missing-redirect", "challenge", "S256", "client-A", ""},
		{"plain-pkce", "challenge", "plain", "client-A", "https://client.test/callback"},
		{"missing-challenge", "", "S256", "client-A", "https://client.test/callback"},
	}
	for _, row := range rows {
		if _, err := raw.Exec(
			`INSERT INTO oauth_codes
			 (code, learner_id, code_challenge, code_challenge_method, client_id, redirect_uri, expires_at)
			 VALUES (?, 'legacy-code', ?, ?, ?, ?, ?)`,
			row.code, row.challenge, row.method, row.clientID, row.redirectURI, expires,
		); err != nil {
			t.Fatalf("seed %s: %v", row.code, err)
		}
	}
	if err := applyMigration(raw, purge); err != nil {
		t.Fatal(err)
	}

	var remaining []string
	codeRows, err := raw.Query(`SELECT code FROM oauth_codes ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	defer codeRows.Close()
	for codeRows.Next() {
		var code string
		if err := codeRows.Scan(&code); err != nil {
			t.Fatal(err)
		}
		remaining = append(remaining, code)
	}
	if len(remaining) != 1 || remaining[0] != "valid" {
		t.Fatalf("authorization codes remaining after migration = %v, want [valid]", remaining)
	}
}

func TestMigrationRejectsOAuthCredentialsWithoutResourceBinding(t *testing.T) {
	n := testDBCounter.Add(1)
	dsn := fmt.Sprintf("file:migrate_resource_binding_%d?mode=memory&cache=shared", n+17000)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := ensureSchemaMigrationsTable(raw); err != nil {
		t.Fatal(err)
	}

	var resourceMigration migration
	for _, m := range buildMigrations() {
		if m.Version == "0032_bind_oauth_credentials_to_resource" {
			resourceMigration = m
			break
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %s: %v", m.Version, err)
		}
	}
	if resourceMigration.Version == "" {
		t.Fatal("OAuth resource-binding migration not found")
	}

	if _, err := raw.Exec(
		`INSERT INTO learners (id, email, password_hash, objective)
		 VALUES ('legacy-resource','legacy-resource@test','h','o')`,
	); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	if _, err := raw.Exec(
		`INSERT INTO oauth_codes
		 (code, learner_id, code_challenge, code_challenge_method, client_id, redirect_uri, expires_at)
		 VALUES ('legacy-code-resource', 'legacy-resource', 'challenge', 'S256',
		         'client-A', 'https://client.test/callback', ?)`,
		expires,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`INSERT INTO refresh_tokens
		 (token, learner_id, client_id, family_id, expires_at, created_at)
		 VALUES ('sha256:legacy-resource', 'legacy-resource', 'client-A',
		         'legacy-resource-family', ?, CURRENT_TIMESTAMP)`,
		expires,
	); err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(raw, resourceMigration); err != nil {
		t.Fatal(err)
	}
	var codes int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM oauth_codes WHERE code = 'legacy-code-resource'`).Scan(&codes); err != nil {
		t.Fatal(err)
	}
	if codes != 0 {
		t.Fatalf("legacy authorization code survived resource migration")
	}
	var resource string
	var revoked sql.NullTime
	if err := raw.QueryRow(
		`SELECT resource, revoked_at FROM refresh_tokens WHERE token = 'sha256:legacy-resource'`,
	).Scan(&resource, &revoked); err != nil {
		t.Fatal(err)
	}
	if resource != "" || !revoked.Valid {
		t.Fatalf("legacy refresh token resource=%q revoked=%v, want blank and revoked", resource, revoked.Valid)
	}
}

func TestMigrationBackfillsStructuredWebhookDomainScope(t *testing.T) {
	n := testDBCounter.Add(1)
	dsn := fmt.Sprintf("file:migrate_webhook_domain_%d?mode=memory&cache=shared", n+17000)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := ensureSchemaMigrationsTable(raw); err != nil {
		t.Fatal(err)
	}

	var scopeMigration migration
	for _, m := range buildMigrations() {
		if m.Version == "0030_webhook_domain_scope" {
			scopeMigration = m
			break
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %s: %v", m.Version, err)
		}
	}
	if scopeMigration.Version == "" {
		t.Fatal("webhook domain migration not found")
	}
	if _, err := raw.Exec(
		`INSERT INTO learners (id, email, password_hash, objective) VALUES ('legacy-webhook','legacy-webhook@test','h','o')`,
	); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, row := range []struct{ kind, content string }{
		{models.WebhookKindDailyRecap, `{"domain_id":"structured-domain","why_now":"review"}`},
		{"olm:kind-domain", "legacy raw OLM"},
		{"daily_motivation", "not JSON"},
	} {
		if _, err := raw.Exec(
			`INSERT INTO webhook_message_queue
			 (learner_id, kind, scheduled_for, content, status, created_at, max_attempts, next_attempt_at)
			 VALUES ('legacy-webhook', ?, ?, ?, 'pending', ?, 5, ?)`,
			row.kind, now, row.content, now, now,
		); err != nil {
			t.Fatalf("seed %s: %v", row.kind, err)
		}
	}
	if err := applyMigration(raw, scopeMigration); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]string{
		models.WebhookKindDailyRecap: "structured-domain",
		"olm:kind-domain":            "kind-domain",
		"daily_motivation":           "",
	} {
		var got string
		if err := raw.QueryRow(`SELECT domain_id FROM webhook_message_queue WHERE kind = ?`, kind).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("domain_id for %s = %q, want %q", kind, got, want)
		}
	}
}

func TestSessionMigrations_PreserveLegacyRowsWithoutInventingAssociations(t *testing.T) {
	n := testDBCounter.Add(1)
	dsn := fmt.Sprintf("file:migrate_sessions_legacy_%d?mode=memory&cache=shared", n+20000)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := ensureSchemaMigrationsTable(raw); err != nil {
		t.Fatal(err)
	}

	migrations := buildMigrations()
	for _, migration := range migrations {
		if migration.Version == "0019_link_interactions_to_sessions" {
			break
		}
		if err := applyMigration(raw, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Version, err)
		}
	}
	now := time.Now().UTC()
	if _, err := raw.Exec(`INSERT INTO learners (id, email, password_hash, objective, created_at)
		VALUES ('legacy-session-learner','legacy-session@test','h','o',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO interactions
		(learner_id, concept, activity_type, success, created_at)
		VALUES ('legacy-session-learner','c','PRACTICE',1,?)`, now); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		trigger string
		honored any
	}{
		{"pending", nil},
		{"honored", 1},
		{"missed", 0},
	} {
		if _, err := raw.Exec(`INSERT INTO implementation_intentions
			(learner_id, domain_id, trigger_text, action_text, honored, created_at)
			VALUES ('legacy-session-learner','',?,'practice',?,?)`, row.trigger, row.honored, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, migration := range migrations {
		if migration.Version != "0019_link_interactions_to_sessions" &&
			migration.Version != "0020_intention_lifecycle_and_session" {
			continue
		}
		if err := applyMigration(raw, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Version, err)
		}
	}
	var sessionID sql.NullString
	if err := raw.QueryRow(`SELECT session_id FROM interactions WHERE learner_id = 'legacy-session-learner'`).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if sessionID.Valid {
		t.Fatalf("migration invented a session association: %q", sessionID.String)
	}
	rows, err := raw.Query(`SELECT trigger_text, status, session_id, resolved_at, updated_at
		FROM implementation_intentions WHERE learner_id = 'legacy-session-learner' ORDER BY trigger_text`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]string{"pending": "pending", "honored": "honored", "missed": "missed"}
	for rows.Next() {
		var trigger, status string
		var linked sql.NullString
		var resolved, updated sql.NullTime
		if err := rows.Scan(&trigger, &status, &linked, &resolved, &updated); err != nil {
			t.Fatal(err)
		}
		if status != want[trigger] || linked.Valid || !updated.Valid {
			t.Fatalf("legacy intention backfill trigger=%s status=%s linked=%v updated=%v", trigger, status, linked, updated)
		}
		if (status == "pending") == resolved.Valid {
			t.Fatalf("resolved_at mismatch for %s: %v", status, resolved)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrate_ConcurrentSerializesSchemaMigrations(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "race.db")
	const runners = 4

	dbs := make([]*sql.DB, runners)
	for i := range dbs {
		db, err := OpenDB(dbPath)
		if err != nil {
			t.Fatalf("open db %d: %v", i, err)
		}
		dbs[i] = db
		t.Cleanup(func() { db.Close() })
	}

	start := make(chan struct{})
	errs := make([]error, runners)
	var wg sync.WaitGroup
	for i, db := range dbs {
		wg.Add(1)
		go func(i int, db *sql.DB) {
			defer wg.Done()
			<-start
			errs[i] = Migrate(db)
		}(i, db)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			t.Fatalf("Migrate runner %d surfaced schema_migrations race: %v", i, err)
		}
		t.Fatalf("Migrate runner %d: %v", i, err)
	}

	var rowCount int
	if err := dbs[0].QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rowCount); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if rowCount != len(buildMigrations()) {
		t.Fatalf("schema_migrations row count = %d, want %d", rowCount, len(buildMigrations()))
	}
}

func TestMigrateContextRetriesSQLiteLockContention(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "retry-lock.db")
	lockOwner, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lockOwner.Close() })
	contender, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	if err := Migrate(lockOwner); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	// Force the first BEGIN EXCLUSIVE attempt to report SQLITE_BUSY quickly;
	// MigrateContext must retry rather than inheriting this short OLTP budget.
	if _, err := contender.Exec(`PRAGMA busy_timeout=20`); err != nil {
		t.Fatalf("set contender busy timeout: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := lockOwner.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("hold exclusive lock: %v", err)
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- MigrateContext(ctx, contender)
	}()
	<-started
	time.Sleep(120 * time.Millisecond)
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("release exclusive lock: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("migration did not recover after lock release: %v", err)
	}
}

// TestMigrate_DropsPFAColumns is the regression guard for issue #55.
// Two scenarios are exercised in the same test so that the assertion
// holds regardless of whether a deployed DB started fresh (post-#55) or
// upgraded from a pre-#55 schema that still had the columns:
//
//  1. Fresh DB: schema.sql no longer declares pfa_successes / pfa_failures.
//     After Migrate(), table_info(concept_states) must not list them.
//  2. Upgrade DB: the columns are inserted manually before Migrate(),
//     simulating a pre-#55 database. Migrate() must drop them via the
//     incremental ALTER TABLE ... DROP COLUMN entries (idempotent).
//
// If either column reappears in either scenario the test fails — that
// catches accidental re-introduction in schema.sql and accidental
// removal of the DROP COLUMN migration entries.
func TestMigrate_DropsPFAColumns(t *testing.T) {
	pfaCols := []string{"pfa_successes", "pfa_failures"}

	hasColumn := func(t *testing.T, db *sql.DB, table, col string) bool {
		t.Helper()
		rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
		if err != nil {
			t.Fatalf("PRAGMA table_info(%s): %v", table, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				cid     int
				name    string
				ctype   string
				notnull int
				dflt    sql.NullString
				pk      int
			)
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				t.Fatalf("scan table_info row: %v", err)
			}
			if name == col {
				return true
			}
		}
		return rows.Err() == nil && false
	}

	// Scenario 1: fresh DB.
	t.Run("fresh", func(t *testing.T) {
		n := testDBCounter.Add(1)
		dsn := fmt.Sprintf("file:migrate_pfa_fresh_%d?mode=memory&cache=shared", n+10100)
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		if err := Migrate(db); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		for _, col := range pfaCols {
			if hasColumn(t, db, "concept_states", col) {
				t.Errorf("concept_states.%s must not exist in fresh schema", col)
			}
		}
	})

	// Scenario 2: pre-#55 DB upgraded.
	//
	// Under the schema_migrations + checksum system the DROP COLUMN
	// migrations run exactly once per database, recorded by version.
	// To simulate a real pre-#55 DB we apply the base schema directly
	// and seed the legacy columns BEFORE Migrate runs — that mirrors
	// the upgrade path a deployed v0.2 DB would actually take.
	t.Run("upgrade_from_pre_55", func(t *testing.T) {
		n := testDBCounter.Add(1)
		dsn := fmt.Sprintf("file:migrate_pfa_upgrade_%d?mode=memory&cache=shared", n+10200)
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { db.Close() })

		// Apply the embedded base schema directly. CREATE TABLE IF NOT
		// EXISTS keeps it compatible with Migrate's own bookkeeping run
		// later in the test.
		if _, err := db.Exec(schemaSQL); err != nil {
			t.Fatalf("apply base schema: %v", err)
		}
		// Seed the legacy columns to simulate a pre-#55 DB.
		for _, col := range pfaCols {
			if _, err := db.Exec(fmt.Sprintf(
				"ALTER TABLE concept_states ADD COLUMN %s REAL DEFAULT 0.0", col,
			)); err != nil {
				t.Fatalf("seed legacy column %s: %v", col, err)
			}
			if !hasColumn(t, db, "concept_states", col) {
				t.Fatalf("seed column %s should exist", col)
			}
		}
		// Migrate must drop them as part of the versioned ALTER list.
		if err := Migrate(db); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		for _, col := range pfaCols {
			if hasColumn(t, db, "concept_states", col) {
				t.Errorf("concept_states.%s should have been dropped by Migrate", col)
			}
		}
		// Subsequent Migrate calls must remain no-ops (checksums match,
		// no migration body is re-executed).
		if err := Migrate(db); err != nil {
			t.Fatalf("second Migrate (idempotent): %v", err)
		}
	})
}

// TestMigrate_DropsLegacyLearnerProfileFields is the issue #61 regression
// guard. The data migration in db/migrations.go must scrub the `level`,
// `background` and `learning_style` keys out of pre-existing profile_json
// blobs because no production component reads them and leaving them in the
// blob causes an unbounded write-only key surface.
//
// We seed two rows that mirror the historical shape (one with all three
// keys plus an unrelated key that must survive, one with a partial subset),
// run Migrate, then assert json_extract returns NULL for the dropped keys
// and the unrelated key is intact.
func TestMigrate_DropsLegacyLearnerProfileFields(t *testing.T) {
	n := testDBCounter.Add(1)
	dsn := fmt.Sprintf("file:migrate_drop_legacy_%d?mode=memory&cache=shared", n+20000)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Apply the embedded base schema directly (CREATE TABLE IF NOT EXISTS
	// so it stays compatible with the schema_migrations bookkeeping in
	// Migrate). Seeding legacy rows BEFORE Migrate is what we need to test
	// here — the data-scrub migration is now versioned and runs exactly
	// once, on the first Migrate it sees, so the seed must precede it
	// (the same shape any deployed pre-#61 DB had at upgrade time).
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("apply base schema: %v", err)
	}
	seed := []struct {
		id      string
		profile string
	}{
		{
			id: "legacy_full",
			profile: `{"level":"intermediate","background":"engineer",` +
				`"learning_style":"visual","language":"fr","device":"laptop"}`,
		},
		{
			id:      "legacy_partial",
			profile: `{"level":"beginner","autonomy_score":0.6}`,
		},
		{
			id:      "clean",
			profile: `{"language":"en"}`,
		},
	}
	for _, s := range seed {
		_, err := db.Exec(
			`INSERT INTO learners (id, email, password_hash, objective, profile_json) VALUES (?, ?, 'h', 'o', ?)`,
			s.id, s.id+"@x", s.profile,
		)
		if err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate (must scrub legacy keys): %v", err)
	}

	// Each dropped key must be absent (json_extract returns NULL) on
	// every row.
	droppedKeys := []string{"$.level", "$.background", "$.learning_style"}
	for _, s := range seed {
		for _, key := range droppedKeys {
			var v sql.NullString
			err := db.QueryRow(
				`SELECT json_extract(profile_json, ?) FROM learners WHERE id = ?`,
				key, s.id,
			).Scan(&v)
			if err != nil {
				t.Fatalf("query %s %s: %v", s.id, key, err)
			}
			if v.Valid {
				t.Errorf("learner %s: key %s should have been scrubbed, got %q", s.id, key, v.String)
			}
		}
	}

	// Unrelated keys must survive on the rows that originally carried them.
	preserved := []struct {
		id   string
		key  string
		want string
	}{
		{"legacy_full", "$.language", "fr"},
		{"legacy_full", "$.device", "laptop"},
		{"clean", "$.language", "en"},
	}
	for _, p := range preserved {
		var v sql.NullString
		err := db.QueryRow(
			`SELECT json_extract(profile_json, ?) FROM learners WHERE id = ?`,
			p.key, p.id,
		).Scan(&v)
		if err != nil {
			t.Fatalf("query %s %s: %v", p.id, p.key, err)
		}
		if !v.Valid || v.String != p.want {
			t.Errorf("learner %s: key %s should equal %q, got valid=%v value=%q", p.id, p.key, p.want, v.Valid, v.String)
		}
	}

	// legacy_partial: autonomy_score must survive, level must be gone.
	var auto sql.NullFloat64
	if err := db.QueryRow(
		`SELECT json_extract(profile_json, '$.autonomy_score') FROM learners WHERE id = 'legacy_partial'`,
	).Scan(&auto); err != nil {
		t.Fatalf("query autonomy_score: %v", err)
	}
	if !auto.Valid || auto.Float64 != 0.6 {
		t.Errorf("legacy_partial autonomy_score should equal 0.6, got valid=%v value=%v", auto.Valid, auto.Float64)
	}

	// Idempotence: running Migrate again on already-scrubbed rows is a
	// no-op (no error, no spurious changes).
	if err := Migrate(db); err != nil {
		t.Fatalf("third Migrate (idempotent on scrubbed rows): %v", err)
	}
}

// TestOpenDB_Memory exercises the OpenDB helper. OpenDB appends `?_pragma=...`
// to the path before opening, so we use a file-backed temp DB to keep the DSN
// shape simple.
func TestOpenDB_Memory(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir + "/open.db")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

// TestOpenDB_BadPath ensures the error path is exercised when ping fails.
func TestOpenDB_BadPath(t *testing.T) {
	// "/proc/0/forbidden" is not openable as a file-backed sqlite db on Linux.
	_, err := OpenDB("/proc/0/forbidden-not-a-real-sqlite-file")
	if err == nil {
		t.Fatal("expected error for unreachable path")
	}
}

// openMigrateTestDB returns a fresh in-memory sqlite handle scoped to the
// current test. Sub-issue #65 tests need to inspect the schema_migrations
// table directly, so they bypass the higher-level Store helper.
func openMigrateTestDB(t *testing.T, suffix string) *sql.DB {
	t.Helper()
	n := testDBCounter.Add(1)
	dsn := fmt.Sprintf("file:memdb_%s_%s_%d?mode=memory&cache=shared", t.Name(), suffix, n)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMigrate_RecordsSchemaMigrationsRows guards the bookkeeping contract:
// after Migrate() succeeds on a fresh database the schema_migrations table
// must exist and contain one row per migration in buildMigrations(), each
// with a non-empty checksum that matches the in-source body. This is the
// "fresh DB" arm of sub-issue #65.
func TestMigrate_RecordsSchemaMigrationsRows(t *testing.T) {
	db := openMigrateTestDB(t, "fresh")
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// schema_migrations must exist as a table.
	var name string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='schema_migrations'`,
	).Scan(&name); err != nil {
		t.Fatalf("schema_migrations table missing: %v", err)
	}

	// Row count == migration count.
	expected := buildMigrations()
	var rowCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != len(expected) {
		t.Fatalf("schema_migrations row count = %d, want %d", rowCount, len(expected))
	}

	// Every migration is recorded with the correct, non-empty checksum.
	for _, m := range expected {
		var (
			storedChecksum string
			appliedAt      sql.NullTime
		)
		err := db.QueryRow(
			`SELECT checksum, applied_at FROM schema_migrations WHERE version = ?`, m.Version,
		).Scan(&storedChecksum, &appliedAt)
		if err != nil {
			t.Fatalf("missing row for version %q: %v", m.Version, err)
		}
		if storedChecksum == "" {
			t.Errorf("version %q: checksum is empty", m.Version)
		}
		if storedChecksum != m.checksum() {
			t.Errorf("version %q: stored checksum %q != current %q",
				m.Version, storedChecksum, m.checksum())
		}
		if !appliedAt.Valid {
			t.Errorf("version %q: applied_at is NULL", m.Version)
		}
	}
}

// TestMigrate_ReRunIsNoOp asserts that running Migrate twice does not insert
// duplicate schema_migrations rows and does not change applied_at — proving
// the second pass took the "checksum already matches, skip" branch rather
// than re-executing every body.
func TestMigrate_ReRunIsNoOp(t *testing.T) {
	db := openMigrateTestDB(t, "rerun")
	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	// Snapshot rows after the first pass.
	type row struct {
		version   string
		checksum  string
		appliedAt string
	}
	snap := func() []row {
		rows, err := db.Query(`SELECT version, checksum, applied_at FROM schema_migrations ORDER BY version`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.version, &r.checksum, &r.appliedAt); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, r)
		}
		return out
	}

	before := snap()
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	after := snap()

	if len(before) != len(after) {
		t.Fatalf("row count changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("row %d drifted on re-run: before=%+v after=%+v", i, before[i], after[i])
		}
	}
}

// TestApplyMigration_NonAtomicBody_IsRolledBack_Issue118 is the regression
// guard for issue #118. A migration body that succeeds partway and then
// fails on a later statement must leave NO schema artefacts behind: neither
// the partially-created tables/columns from the body nor a row in
// schema_migrations claiming the migration succeeded. Without the
// per-migration transaction added in #118 the first statement's effect
// persists and operators are left with a half-migrated database.
//
// We synthesise a migration whose body has two statements: the first
// creates table `issue118_x`, the second is invalid SQL. After
// applyMigration returns an error we assert (a) the table does NOT exist,
// and (b) schema_migrations has no row for the synthetic version.
func TestApplyMigration_NonAtomicBody_IsRolledBack_Issue118(t *testing.T) {
	db := openMigrateTestDB(t, "atomic_body")
	if err := ensureSchemaMigrationsTable(db); err != nil {
		t.Fatalf("ensureSchemaMigrationsTable: %v", err)
	}

	m := migration{
		Version: "9999_issue118_synthetic",
		Body: `CREATE TABLE issue118_x (a INTEGER);
` +
			`CREATE TABLE issue118_x INVALID SYNTAX;`,
	}

	err := applyMigration(db, m)
	if err == nil {
		t.Fatal("expected applyMigration to return an error for invalid second statement, got nil")
	}

	// (a) The first statement's table must NOT survive — the whole body
	// has to be rolled back as a unit.
	var tableName string
	scanErr := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='issue118_x'`,
	).Scan(&tableName)
	if scanErr == nil {
		t.Errorf("table issue118_x should not exist after failed migration, but it does")
	} else if scanErr != sql.ErrNoRows {
		t.Fatalf("unexpected error checking table existence: %v", scanErr)
	}

	// (b) schema_migrations must not record the failed migration.
	var version string
	scanErr = db.QueryRow(
		`SELECT version FROM schema_migrations WHERE version = ?`, m.Version,
	).Scan(&version)
	if scanErr == nil {
		t.Errorf("schema_migrations should not contain row for failed version %q", m.Version)
	} else if scanErr != sql.ErrNoRows {
		t.Fatalf("unexpected error reading schema_migrations: %v", scanErr)
	}
}

// TestApplyMigration_IgnoreExecErrors_StillRecords is the regression guard
// for the IgnoreExecErrors path under the per-migration transaction added in
// issue #118. When a migration's body fails (e.g. a legacy ALTER that
// targets a column that does not exist) and IgnoreExecErrors is true, the
// row in schema_migrations must still be inserted so subsequent runs treat
// the migration as applied. Without careful handling, the failed body
// statement aborts the transaction and the bookkeeping INSERT also fails.
func TestApplyMigration_IgnoreExecErrors_StillRecords(t *testing.T) {
	db := openMigrateTestDB(t, "ignore_exec_errors")
	if err := ensureSchemaMigrationsTable(db); err != nil {
		t.Fatalf("ensureSchemaMigrationsTable: %v", err)
	}
	// Seed a small base table so the failing ALTER has a concrete target.
	if _, err := db.Exec(`CREATE TABLE issue118_t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("seed base table: %v", err)
	}

	m := migration{
		Version:          "9999_issue118_ignore_exec_errors",
		Body:             `ALTER TABLE issue118_t DROP COLUMN does_not_exist`,
		IgnoreExecErrors: true,
	}

	if err := applyMigration(db, m); err != nil {
		t.Fatalf("applyMigration with IgnoreExecErrors must not return an error, got: %v", err)
	}

	var version string
	if err := db.QueryRow(
		`SELECT version FROM schema_migrations WHERE version = ?`, m.Version,
	).Scan(&version); err != nil {
		t.Fatalf("schema_migrations must record the IgnoreExecErrors migration even when body fails: %v", err)
	}
	if version != m.Version {
		t.Errorf("recorded version = %q, want %q", version, m.Version)
	}
}

// TestMigrate_DetectsChecksumDrift simulates an operator (or a corrupted
// replica) editing a migration body after it was applied. The next call to
// Migrate must refuse to start and surface a "checksum mismatch" error so
// the drift is visible rather than silently accepted.
func TestMigrate_DetectsChecksumDrift(t *testing.T) {
	db := openMigrateTestDB(t, "drift")
	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	// Pick the base schema migration and corrupt its stored checksum to
	// simulate the source body having changed since it was applied.
	const tampered = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	res, err := db.Exec(
		`UPDATE schema_migrations SET checksum = ? WHERE version = '0001_base_schema'`, tampered,
	)
	if err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("expected to tamper 1 row, got %d", n)
	}

	err = Migrate(db)
	if err == nil {
		t.Fatal("expected checksum-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error %q does not contain 'checksum mismatch'", err.Error())
	}
}

func TestSplitSQLStatementsPreservesQuotedFunctionBody(t *testing.T) {
	sqlText := `CREATE FUNCTION f() RETURNS trigger LANGUAGE plpgsql AS $body$
BEGIN
  RAISE EXCEPTION 'immutable; still one statement';
  RETURN NULL;
END;
$body$;
CREATE TRIGGER t BEFORE UPDATE ON x FOR EACH STATEMENT EXECUTE FUNCTION f();`
	statements := splitSQLStatements(sqlText)
	if len(statements) != 2 {
		t.Fatalf("statement count = %d, want 2: %#v", len(statements), statements)
	}
	if !strings.Contains(statements[0], "RAISE EXCEPTION") || !strings.Contains(statements[0], "RETURN NULL;") {
		t.Fatalf("function body was split: %q", statements[0])
	}
	if !strings.HasPrefix(statements[1], "CREATE TRIGGER") {
		t.Fatalf("second statement = %q", statements[1])
	}
}
