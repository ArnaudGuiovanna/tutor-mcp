package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/memory"
	"tutor-mcp/models"
)

func TestMemoryIndexCommandPostgresUsesRestrictedRuntimeRole(t *testing.T) {
	baseDSN := os.Getenv("TUTOR_TEST_PG_DSN")
	if baseDSN == "" {
		t.Skip("set TUTOR_TEST_PG_DSN for the PostgreSQL command regression")
	}
	admin, err := db.OpenPostgres(baseDSN, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	schema := "memory_index_command_" + suffix
	role := "memory_index_role_" + suffix
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	roleCreated := false
	defer func() {
		if roleCreated {
			if _, err := admin.Exec(`DROP OWNED BY ` + role + `; DROP ROLE ` + role); err != nil {
				t.Errorf("role cleanup: %v", err)
			}
		}
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Errorf("schema cleanup: %v", err)
		}
	}()
	separator := "?"
	if strings.Contains(baseDSN, "?") {
		separator = "&"
	}
	dsn := baseDSN + separator + "search_path=" + schema
	database, err := db.OpenPostgres(dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.MigratePostgres(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	persistence := db.NewStoreWithDialect(database, db.DialectPostgres)
	if err := persistence.EnsureInstallation(t.Context(), "institution"); err != nil {
		t.Fatal(err)
	}
	encodedKey := "test:" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := db.NewIntegrationSecretKeyring(encodedKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	persistence.SetIntegrationSecretKeyring(keyring)
	learner, err := persistence.CreateLearner(t.Context(), "command@example.test", "hash", "objective", "")
	if err != nil {
		t.Fatal(err)
	}
	limits, err := memory.LimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	object, _, err := persistence.CompareAndSwapNarrative(t.Context(), memory.NarrativeKey{LearnerID: learner.ID, Scope: memory.ScopeSession, Key: "2026-10-15T00-00-00Z"}, 0, "---\ndomain_id: course\n---\nprivate body", "", "", limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	options, err := parseCommand([]string{"memory-index", "--profile", "institution", "--tenant", models.LegacyTenantID}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("INTEGRATION_SECRET_KEYS", encodedKey)
	t.Setenv("INTEGRATION_SECRET_CURRENT_KEY_ID", "test")
	if err := runMemoryIndexCommand(t.Context(), options, io.Discard); err == nil || !strings.Contains(err.Error(), "runtime PostgreSQL role") {
		t.Fatalf("privileged command not rejected: %v", err)
	}
	if _, err := database.Exec(`CREATE ROLE ` + role + ` NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
		GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role + `;
		GRANT SELECT ON tenants,learners,enrollments,legacy_domain_enrollments,legacy_concept_mappings,narrative_objects,schema_migrations,installation TO ` + role + `;
		GRANT UPDATE(session_domain_digest,session_sort_key,session_indexed_version) ON narrative_objects TO ` + role); err != nil {
		t.Fatal(err)
	}
	roleCreated = true
	runtimeDSN := dsn + "&options=" + strings.ReplaceAll(url.QueryEscape("-c role="+role), "+", "%20")
	probe, err := db.OpenPostgres(runtimeDSN, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyPostgresRuntimeRole(context.Background(), probe); err != nil {
		probe.Close()
		t.Fatal(err)
	}
	var visible int
	if err := probe.QueryRow(`SELECT COUNT(*) FROM narrative_objects`).Scan(&visible); err != nil || visible != 0 {
		probe.Close()
		t.Fatalf("unscoped RLS visibility=%d,%v", visible, err)
	}
	probe.Close()
	t.Setenv("DATABASE_URL", runtimeDSN)
	var output bytes.Buffer
	if err := runMemoryIndexCommand(t.Context(), options, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"complete":true`) || !strings.Contains(output.String(), `"indexed":1`) {
		t.Fatalf("runtime command report=%s", output.String())
	}
	after, err := persistence.GetNarrative(t.Context(), object.NarrativeKey)
	if err != nil || after.Version != object.Version || after.Checksum != object.Checksum || after.Content != object.Content {
		t.Fatalf("runtime command altered object: %+v,%v", after, err)
	}
}
