package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type countedSessionBackend struct {
	memory.NarrativeStore
	index          memory.SessionNarrativeIndex
	reads, selects int
}

func (c *countedSessionBackend) GetNarrative(ctx context.Context, key memory.NarrativeKey) (*memory.NarrativeObject, error) {
	if key.Scope == memory.ScopeSession {
		c.reads++
	}
	return c.NarrativeStore.GetNarrative(ctx, key)
}
func (c *countedSessionBackend) ListRecentSessionNarratives(ctx context.Context, scope models.TenantScope, learner, domain string, limit int) ([]memory.NarrativeMetadata, error) {
	c.selects++
	return c.index.ListRecentSessionNarratives(ctx, scope, learner, domain, limit)
}

func sessionIndexRuntimeStore(t *testing.T, admin *Store) *Store {
	t.Helper()
	if admin.dialect != DialectPostgres {
		return admin
	}
	var schema string
	if err := admin.root.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf("session_index_runtime_%d", testDBCounter.Add(1))
	if _, err := admin.root.Exec(`CREATE ROLE ` + role + ` NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
		GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role + `;
		GRANT SELECT ON tenants,learners,enrollments,legacy_domain_enrollments,legacy_concept_mappings,narrative_objects,schema_migrations,installation TO ` + role + `;
		GRANT UPDATE(session_domain_digest,session_sort_key,session_indexed_version) ON narrative_objects TO ` + role); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(os.Getenv("TUTOR_TEST_PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	pool := stdlib.OpenDB(*config, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE `+role)
		return err
	}))
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = pool.Close()
		if _, err := admin.root.Exec(`DROP OWNED BY ` + role + `;DROP ROLE ` + role); err != nil {
			t.Errorf("runtime cleanup: %v", err)
		}
	})
	if err := VerifyPostgresRuntimeRole(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM narrative_objects`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unscoped narrative visibility=%d,%v", count, err)
	}
	if _, err := pool.Exec(`SELECT password_hash FROM users`); err == nil {
		t.Fatal("index runtime received global credential privileges")
	}
	runtime := NewStoreWithDialect(pool, DialectPostgres)
	runtime.SetIntegrationSecretKeyring(admin.secretKeyring)
	return runtime
}

func seedIndexedSession(t *testing.T, s *Store, key memory.NarrativeKey, domain, body string) *memory.NarrativeObject {
	t.Helper()
	content := "---\ndomain_id: " + domain + "\n---\n" + body
	object, _, err := s.CompareAndSwapNarrative(t.Context(), key, 0, content, "", "", narrativeTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func TestSessionNarrativeIndexCanonicalSelectionAndScope(t *testing.T) {
	s := narrativeTestStore(t)
	scope := models.LegacyPrincipal("L1").TenantScope()
	d1, err := s.CreateDomain(t.Context(), "L1", "one", "", models.KnowledgeSpace{})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.CreateDomain(t.Context(), "L1", "two", "", models.KnowledgeSpace{})
	if err != nil {
		t.Fatal(err)
	}
	key := memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeSession, Key: "2026-10-10T00-00-00Z"}
	first := seedIndexedSession(t, s, key, d1.ID, "recovery")
	for _, enrollment := range []string{"domain_enrollment_" + d1.ID, "domain_enrollment_" + d2.ID} {
		duplicate := first.NarrativeKey
		duplicate.EnrollmentID = enrollment
		seedIndexedSession(t, s, duplicate, d1.ID, enrollment)
	}
	// Bulk fixture bypasses quota scans, but each envelope is authenticated to
	// its real canonical key and the same projection maintained by CAS writes.
	err = s.WithTenantTx(t.Context(), scope, func(ctx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		for i := 0; i < 2048; i++ {
			foreign := first.NarrativeKey
			foreign.Key = time.Date(2026, 10, 11, 0, 0, i, 0, time.UTC).Format("2006-01-02T15-04-05Z")
			content := "---\ndomain_id: foreign\n---\nunrelated"
			ciphertext, err := s.secretKeyring.encryptWithAAD(content, narrativeObjectAADv2(foreign))
			if err != nil {
				return err
			}
			_, err = txs.exec(ctx, `INSERT INTO narrative_objects(tenant_id,enrollment_id,learner_id,scope,domain_id,object_key,ciphertext,key_id,aad_version,version,checksum,size_bytes,created_at,updated_at,session_domain_digest,session_sort_key,session_indexed_version)
				VALUES(?,?,?,'session','',?,?,?,2,1,?,?,?,?,?,?,1)`, foreign.TenantID, foreign.EnrollmentID, foreign.LearnerID, foreign.Key, ciphertext, integrationSecretKeyID(ciphertext), narrativeChecksum(content), len(content), time.Now().UTC(), time.Now().UTC(), memory.SessionDomainDigest("foreign"), time.Date(2026, 10, 11, 0, 0, i, 0, time.UTC).Format("2006-01-02T15:04:05.000000000Z"))
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := sessionIndexRuntimeStore(t, s)
	backend := &countedSessionBackend{NarrativeStore: runtime, index: runtime}
	memory.ConfigureNarrativeStore(backend)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
	var saved context.Context
	err = runtime.WithTenantTx(t.Context(), scope, func(ctx context.Context, scoped storeport.Store) error {
		saved = ctx
		for _, access := range []*Store{runtime, scoped.(*Store)} {
			rows, err := access.ListRecentSessionNarratives(ctx, scope, "L1", d1.ID, 3)
			if err != nil || len(rows) != 3 {
				return fmt.Errorf("canonical selection=%+v err=%w", rows, err)
			}
			seen := map[string]bool{}
			for _, row := range rows {
				if row.DomainID != "" || row.TenantID != scope.TenantID || row.EnrollmentID == "" {
					return fmt.Errorf("lost canonical identity: %+v", row)
				}
				seen[row.EnrollmentID] = true
			}
			if len(seen) != 3 {
				return fmt.Errorf("deduplicated same timestamp across enrollments: %+v", rows)
			}
		}
		if _, err := scoped.(*Store).ListRecentSessionNarratives(context.Background(), scope, "L1", d1.ID, 3); !errors.Is(err, memory.ErrNarrativeTenantScope) {
			return fmt.Errorf("scoped store accepted missing database context: %v", err)
		}
		called := false
		fresh, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := scoped.WithTenantTx(fresh, scope, func(context.Context, storeport.Store) error { called = true; return nil })
		cancel()
		if err == nil || called || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("fresh context opened or waited on nested transaction: callback=%v err=%v", called, err)
		}
		wrong := scope
		wrong.TenantID = "tenant_wrong"
		if _, err := runtime.ListRecentSessionNarratives(ctx, wrong, "L1", d1.ID, 3); err == nil {
			return fmt.Errorf("cross-scope query accepted")
		}
		view, err := memory.LoadContextForDomainContext(ctx, "L1", d1.ID, "", nil, nil)
		if err != nil || len(view.RecentSessions) != 3 {
			return fmt.Errorf("loaded history=%+v err=%w", view, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.reads != 3 || backend.selects != 1 {
		t.Fatalf("session IO reads=%d selections=%d", backend.reads, backend.selects)
	}
	if _, err := runtime.ListRecentSessionNarratives(saved, scope, "L1", d1.ID, 3); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("committed context silently reopened transaction: %v", err)
	}
	if _, err := memory.LoadContext("L1", "", nil, nil); !errors.Is(err, memory.ErrNarrativeTenantScope) {
		t.Fatalf("unscoped SQL loader fell back: %v", err)
	}
}

func TestSessionNarrativeIndexBackfillResumesAndPreservesEnvelopes(t *testing.T) {
	s := narrativeTestStore(t)
	scope := models.LegacyPrincipal("L1").TenantScope()
	var objects []*memory.NarrativeObject
	for i := 0; i < 4; i++ {
		objects = append(objects, seedIndexedSession(t, s, memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeSession, Key: fmt.Sprintf("2026-10-10T00-00-0%dZ", i)}, "old", "session"))
	}
	// Exercise retained AADv1 as well as current AADv2 without rekeying.
	v1, err := s.secretKeyring.encryptWithAAD(objects[0].Content, narrativeObjectAAD(objects[0].NarrativeKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(t.Context(), `UPDATE narrative_objects SET ciphertext=?,aad_version=1 WHERE object_key=? AND scope='session'`, v1, objects[0].Key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(t.Context(), `UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	snapshots := make(map[string]string)
	for _, object := range objects {
		snapshots[object.Key] = sessionEnvelopeSnapshot(t, s, object.Key)
	}
	runtime := sessionIndexRuntimeStore(t, s)
	if _, err := runtime.ListRecentSessionNarratives(t.Context(), scope, "L1", "old", 3); !errors.Is(err, memory.ErrSessionIndexIncomplete) {
		t.Fatalf("unknown index=%v", err)
	}
	first, next, err := runtime.BackfillSessionNarrativeIndex(t.Context(), scope, "", 1)
	if err != nil || first.Indexed != 1 || first.Remaining != -1 || first.RemainingKnown || next == "" {
		t.Fatalf("first page=%+v next=%q err=%v", first, next, err)
	}
	// Simulate interruption. A restart deliberately starts at the beginning;
	// indexed versions, rather than an in-memory cursor, retain progress.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := runtime.BackfillSessionNarrativeIndex(ctx, scope, next, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	for {
		page, next, err := runtime.BackfillSessionNarrativeIndex(t.Context(), scope, "", 2)
		if err != nil {
			t.Fatal(err)
		}
		if page.Remaining == 0 {
			break
		}
		if next == "" {
			t.Fatalf("unfinished page=%+v", page)
		}
	}
	for _, object := range objects {
		if got := sessionEnvelopeSnapshot(t, s, object.Key); got != snapshots[object.Key] {
			t.Fatalf("backfill altered encrypted object %s", object.Key)
		}
	}
	rows, err := runtime.ListRecentSessionNarratives(t.Context(), scope, "L1", "old", 3)
	if err != nil || len(rows) != 3 {
		t.Fatalf("backfilled=%+v,%v", rows, err)
	}
	page, _, err := runtime.BackfillSessionNarrativeIndex(t.Context(), scope, "", 100)
	if err != nil || page.Scanned != 0 || page.Remaining != 0 {
		t.Fatalf("replay=%+v,%v", page, err)
	}
}

func sessionEnvelopeSnapshot(t *testing.T, s *Store, key string) string {
	t.Helper()
	var ciphertext, keyID, checksum string
	var aad int
	var version, size int64
	var updated flexTime
	if err := s.queryRow(t.Context(), `SELECT ciphertext,key_id,aad_version,version,checksum,size_bytes,updated_at FROM narrative_objects WHERE scope='session' AND object_key=?`, key).Scan(&ciphertext, &keyID, &aad, &version, &checksum, &size, &updated); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(ciphertext, keyID, aad, version, checksum, size, updated.Time)
}

func TestSessionNarrativeIndexRepairFencesWritesRotationAndMissingKey(t *testing.T) {
	s := narrativeTestStore(t)
	scope := models.LegacyPrincipal("L1").TenantScope()
	first := seedIndexedSession(t, s, memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeSession, Key: "2026-10-10T00-00-00Z"}, "old", "body")
	if _, err := s.exec(t.Context(), `UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	envelope, err := s.readSessionIndexEnvelope(t.Context(), scope, first.NarrativeKey, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := s.CompareAndSwapNarrative(t.Context(), first.NarrativeKey, 1, "---\ndomain_id: new\n---\nnew", "", "", narrativeTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := s.writeSessionIndexEnvelope(t.Context(), scope, first.NarrativeKey, envelope, "old"); err != nil || changed {
		t.Fatalf("old repair overwrote writer: %v,%v", changed, err)
	}
	if rows, err := s.ListRecentSessionNarratives(t.Context(), scope, "L1", "new", 3); err != nil || len(rows) != 1 || rows[0].Version != updated.Version {
		t.Fatalf("writer metadata=%+v,%v", rows, err)
	}
	if _, err := s.exec(t.Context(), `UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	envelope, err = s.readSessionIndexEnvelope(t.Context(), scope, first.NarrativeKey, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	oldKeys := s.secretKeyring
	s.SetIntegrationSecretKeyring(testSecretKeyring(t, "narrative:"+encodedSecretKey(21)+",next:"+encodedSecretKey(22), "next"))
	if _, err := s.RotateNarrativeSecrets(t.Context()); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.writeSessionIndexEnvelope(t.Context(), scope, first.NarrativeKey, envelope, "new"); err != nil || changed {
		t.Fatalf("old envelope repair survived rotation: %v,%v", changed, err)
	}
	s.SetIntegrationSecretKeyring(oldKeys)
	page, _, err := s.BackfillSessionNarrativeIndex(t.Context(), scope, "", 100)
	if err != nil || page.Unresolved != 1 || page.Remaining != 1 {
		t.Fatalf("missing retained key=%+v,%v", page, err)
	}
	s.SetIntegrationSecretKeyring(testSecretKeyring(t, "narrative:"+encodedSecretKey(21)+",next:"+encodedSecretKey(22), "next"))
	if page, _, err = s.BackfillSessionNarrativeIndex(t.Context(), scope, "", 100); err != nil || page.Indexed != 1 || page.Remaining != 0 {
		t.Fatalf("key repair=%+v,%v", page, err)
	}
	// An older writer increments version but leaves the projection untouched.
	if _, err := s.exec(t.Context(), `UPDATE narrative_objects SET version=version+1 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListRecentSessionNarratives(t.Context(), scope, "L1", "missing", 3); !errors.Is(err, memory.ErrSessionIndexIncomplete) {
		t.Fatalf("stale index looked empty: %v", err)
	}
	if _, _, err := s.BackfillSessionNarrativeIndex(t.Context(), scope, "", 100); err != nil {
		t.Fatal(err)
	}
	// Tampering the candidate's projection cannot pass final plaintext checks.
	if _, err := s.exec(t.Context(), `UPDATE narrative_objects SET session_domain_digest=? WHERE scope='session'`, memory.SessionDomainDigest("forged")); err != nil {
		t.Fatal(err)
	}
	memory.ConfigureNarrativeStore(s)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
	err = s.WithTenantTx(t.Context(), scope, func(ctx context.Context, _ storeport.Store) error {
		_, err := memory.LoadContextForDomainContext(ctx, "L1", "forged", "", nil, nil)
		return err
	})
	if !errors.Is(err, memory.ErrSessionIndexIncomplete) {
		t.Fatalf("forged metadata=%v", err)
	}
}

func TestSessionNarrativeIndexRepairContinuesPastCorruptObjects(t *testing.T) {
	s := narrativeTestStore(t)
	scope := models.LegacyPrincipal("L1").TenantScope()
	for i := 0; i < 3; i++ {
		seedIndexedSession(t, s, memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeSession, Key: fmt.Sprintf("2026-10-10T00-00-0%dZ", i)}, strings.Repeat("long", 16<<10), "body")
	}
	if _, err := s.exec(t.Context(), `UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(t.Context(), `UPDATE narrative_objects SET ciphertext='corrupt' WHERE object_key='2026-10-10T00-00-00Z' AND scope='session'`); err != nil {
		t.Fatal(err)
	}
	runtime := sessionIndexRuntimeStore(t, s)
	after := ""
	indexed := int64(0)
	unresolved := int64(0)
	for {
		page, next, err := runtime.BackfillSessionNarrativeIndex(t.Context(), scope, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		indexed += page.Indexed
		unresolved += page.Unresolved
		if next == "" {
			if page.Remaining != 1 {
				t.Fatalf("remaining=%+v", page)
			}
			break
		}
		after = next
	}
	if indexed != 2 || unresolved != 1 {
		t.Fatalf("poison prefix starved healthy rows: indexed=%d unresolved=%d", indexed, unresolved)
	}
}

func TestSessionNarrativeIndexTenantIsolation(t *testing.T) {
	s := narrativeTestStore(t)
	a := models.LegacyPrincipal("L1").TenantScope()
	seedIndexedSession(t, s, memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeSession, Key: "2026-10-10T00-00-00Z"}, "shared-domain", "tenant A")
	owner, _ := verifiedOwner(t, s, seedInstitutionOwner(t, s, "index-b", "owner@index-b.test"))
	_, token, err := s.CreateTenantInvitation(t.Context(), owner, "learner@index-b.test", []string{models.RoleLearner}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	membership, err := s.AcceptTenantInvitationWithNewUser(t.Context(), models.InvitationCredential{Token: token}, "hash")
	if err != nil {
		t.Fatal(err)
	}
	b := models.TenantScope{TenantID: membership.TenantID, UserID: membership.UserID, MembershipID: membership.ID, LearnerID: membership.LearnerID}
	if err := s.WithTenantTx(t.Context(), b, func(ctx context.Context, _ storeport.Store) error {
		return s.EnsureRecoveryEnrollment(ctx, b.LearnerID)
	}); err != nil {
		t.Fatal(err)
	}
	seedIndexedSession(t, s, memory.NarrativeKey{LearnerID: b.LearnerID, Scope: memory.ScopeSession, Key: "2026-10-11T00-00-00Z"}, "shared-domain", "tenant B")
	if _, err := s.exec(t.Context(), `UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	runtime := sessionIndexRuntimeStore(t, s)
	page, _, err := runtime.BackfillSessionNarrativeIndex(t.Context(), a, "", 100)
	if err != nil || page.Indexed != 1 || page.Remaining != 0 {
		t.Fatalf("A repair=%+v,%v", page, err)
	}
	var foreignVersion int
	if err := s.queryRow(t.Context(), `SELECT session_indexed_version FROM narrative_objects WHERE tenant_id=? AND scope='session'`, b.TenantID).Scan(&foreignVersion); err != nil || foreignVersion != 0 {
		t.Fatalf("A repaired B: %d,%v", foreignVersion, err)
	}
	maintenanceA := a
	maintenanceA.LearnerID = ""
	if _, err := runtime.ListRecentSessionNarratives(t.Context(), maintenanceA, b.LearnerID, "shared-domain", 3); !errors.Is(err, memory.ErrNarrativeTenantScope) {
		t.Fatalf("A queried B learner: %v", err)
	}
	if page, _, err = runtime.BackfillSessionNarrativeIndex(t.Context(), b, "", 100); err != nil || page.Indexed != 1 {
		t.Fatalf("B repair=%+v,%v", page, err)
	}
	for _, scope := range []models.TenantScope{a, b} {
		rows, err := runtime.ListRecentSessionNarratives(t.Context(), scope, scope.LearnerID, "shared-domain", 3)
		if err != nil || len(rows) != 1 || rows[0].TenantID != scope.TenantID || rows[0].LearnerID != scope.LearnerID {
			t.Fatalf("tenant selection=%+v,%v", rows, err)
		}
	}
}

func TestSessionNarrativeIndexOrdersMixedKeysWithoutRekeying(t *testing.T) {
	s := narrativeTestStore(t)
	scope := models.LegacyPrincipal("L1").TenantScope()
	keys := []string{"2026-10-15T13-59-59Z", "2026-10-15T13-59-58Z", "2026-10-15T13-59-57Z", "2026-10-15T13:00:00Z", "2026-10-15T14:59:59.5+01:00"}
	snapshots := make(map[string]string)
	for _, key := range keys {
		seedIndexedSession(t, s, memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeSession, Key: key}, "course", key)
		snapshots[key] = sessionEnvelopeSnapshot(t, s, key)
	}
	if _, err := s.exec(t.Context(), `UPDATE narrative_objects SET session_indexed_version=0,session_sort_key='' WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	runtime := sessionIndexRuntimeStore(t, s)
	if report, _, err := runtime.BackfillSessionNarrativeIndex(t.Context(), scope, "", 100); err != nil || report.Indexed != 5 {
		t.Fatalf("backfill mixed keys=%+v,%v", report, err)
	}
	for _, key := range keys {
		if got := sessionEnvelopeSnapshot(t, s, key); got != snapshots[key] {
			t.Fatalf("key/AAD envelope changed: %q", key)
		}
	}
	for _, domain := range []string{"course", ""} {
		rows, err := runtime.ListRecentSessionNarratives(t.Context(), scope, "L1", domain, 3)
		if err != nil || len(rows) != 3 {
			t.Fatalf("mixed key query=%+v,%v", rows, err)
		}
		for i, want := range []string{keys[4], keys[0], keys[1]} {
			if rows[i].Key != want {
				t.Fatalf("mixed key rank%d=%q want%q", i, rows[i].Key, want)
			}
		}
	}
}

func TestSessionNarrativeIndexTracksFinalPlaintextAcrossWriteOperations(t *testing.T) {
	s := narrativeTestStore(t)
	scope := models.LegacyPrincipal("L1").TenantScope()
	memory.ConfigureNarrativeStore(s)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
	at := time.Date(2026, 10, 15, 7, 0, 0, 0, time.UTC)
	operations := []memory.WriteRequest{
		{Operation: memory.OpReplaceFile, Content: "---\ndomain_id: actual\n---\nbody"},
		{Operation: memory.OpAppend, Content: "body mentions domain_id: wrong"},
		{Operation: memory.OpReplaceSection, SectionKey: "Notes", Content: "section domain_id: wrong"},
		{Operation: memory.OpReplaceFile, Content: "---\ndomain_id: replacement\n---\nnew body"},
	}
	for i, request := range operations {
		request.LearnerID = "L1"
		request.DomainID = "ignored-request-domain"
		request.Scope = memory.ScopeSession
		request.Timestamp = at
		err := s.WithTenantTx(t.Context(), scope, func(ctx context.Context, _ storeport.Store) error { return memory.WriteContext(ctx, request) })
		if err != nil {
			t.Fatal(err)
		}
		wanted := "actual"
		if i == 3 {
			wanted = "replacement"
		}
		rows, err := s.ListRecentSessionNarratives(t.Context(), scope, "L1", wanted, 3)
		if err != nil || len(rows) != 1 || rows[0].Version != int64(i+1) || rows[0].DomainID != "" {
			t.Fatalf("operation%d indexed request instead of final plaintext: %+v,%v", i, rows, err)
		}
		if rows, err := s.ListRecentSessionNarratives(t.Context(), scope, "L1", "ignored-request-domain", 3); err != nil || len(rows) != 0 {
			t.Fatalf("request domain leaked into index: %+v,%v", rows, err)
		}
	}
}

func TestSessionNarrativeIndexPendingCursorUsesBoundedIndexSeek(t *testing.T) {
	s := setupTestDB(t)
	if s.dialect != DialectSQLite {
		t.Skip("EXPLAIN QUERY PLAN regression targets SQLite's partial-index planner")
	}
	for _, learner := range []string{"", "L1"} {
		scope := models.LegacyPrincipal("L1").TenantScope()
		scope.LearnerID = learner
		query, args := sessionIndexPageQuery(scope, [3]string{"legacy_recovery_enrollment_L1", "", "2026-10-01T00-00-00Z"}, 100)
		rows, err := s.query(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plans []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plans = append(plans, detail)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		plan := strings.Join(plans, "\n")
		index := "idx_narrative_sessions_pending_tenant"
		if learner != "" {
			index = "idx_narrative_sessions_pending"
		}
		if !strings.Contains(plan, "SEARCH narrative_objects USING INDEX "+index) || !strings.Contains(plan, ">") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatalf("unbounded pending page for learner=%q:\n%s", learner, plan)
		}
	}
}
