package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func sqliteReaderTestStore(t *testing.T) *Store {
	t.Helper()
	template, err := sqliteTestDBTemplateBytes()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runtime.db")
	if err := os.WriteFile(path, template, 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSQLiteStore(writer, path)
	if err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTenantReadSQLiteConcurrencyAndStableSnapshot(t *testing.T) {
	s := sqliteReaderTestStore(t)
	if s.root.Stats().MaxOpenConnections != 1 || s.reader.Stats().MaxOpenConnections != 4 {
		t.Fatal("runtime pool limits changed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	scope := models.LegacyPrincipal("L1").TenantScope()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	for range 2 {
		go func() {
			done <- s.WithTenantReadTx(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
				bound := txCtx.Value(tenantTransactionContextKey{}).(*tenantTransactionContext)
				if bound.root != s.root || s.executor(txCtx) != bound.tx || scoped.(*Store).db != bound.tx {
					return errors.New("reader transaction lost writer identity")
				}
				var before, after string
				if err := s.queryRow(txCtx, `SELECT objective FROM learners WHERE id='L1'`).Scan(&before); err != nil {
					return err
				}
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				if err := s.queryRow(txCtx, `SELECT objective FROM learners WHERE id='L1'`).Scan(&after); err != nil {
					return err
				}
				if before != after {
					return errors.New("reader snapshot changed")
				}
				return nil
			})
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("two readers did not enter concurrently", ctx.Err())
		}
	}
	if s.reader.Stats().InUse != 2 || s.root.Stats().InUse != 0 {
		t.Fatal("readers consumed writer capacity")
	}
	err := s.WithTenantTx(ctx, scope, func(txCtx context.Context, _ storeport.Store) error {
		_, err := s.exec(txCtx, `UPDATE learners SET objective='new snapshot' WHERE id='L1'`)
		return err
	})
	close(release)
	if err != nil {
		t.Fatalf("writer could not progress during two WAL readers: %v", err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var current string
	if err := s.queryRow(ctx, `SELECT objective FROM learners WHERE id='L1'`).Scan(&current); err != nil || current != "new snapshot" {
		t.Fatalf("writer commit missing: %q %v", current, err)
	}
}

func TestTenantReadSQLiteEnforcesWritesAndNestedScope(t *testing.T) {
	s := sqliteReaderTestStore(t)
	assertReadEnforcement(t, s)
	foreign := sqliteReaderTestStore(t)
	scope := models.LegacyPrincipal("L1").TenantScope()
	var saved context.Context
	err := s.WithTenantReadTx(t.Context(), scope, func(ctx context.Context, scoped storeport.Store) error {
		saved = ctx
		for _, target := range []*Store{s, scoped.(*Store)} {
			for _, read := range []bool{false, true} {
				fn := target.WithTenantTx
				if read {
					fn = target.WithTenantReadTx
				}
				if err := fn(ctx, scope, func(nested context.Context, _ storeport.Store) error {
					if s.executor(nested) != s.executor(ctx) {
						return errors.New("nested reader opened another transaction")
					}
					_, err := s.exec(nested, `UPDATE learners SET objective='promoted' WHERE id='L1'`)
					if err == nil {
						return errors.New("nested reader promoted to writer")
					}
					return nil
				}); err != nil {
					return err
				}
			}
			if err := target.WithTx(ctx, func(nested storeport.Store) error {
				_, err := nested.(*Store).exec(ctx, `DELETE FROM learners WHERE id='L1'`)
				if err == nil {
					return errors.New("legacy WithTx promoted reader")
				}
				return nil
			}); err != nil {
				return err
			}
		}
		for _, tc := range []struct {
			target *Store
			ctx    context.Context
			scope  models.TenantScope
		}{
			{s, ctx, models.LegacyPrincipal("other").TenantScope()},
			{foreign, ctx, scope}, {scoped.(*Store), t.Context(), scope},
		} {
			called := false
			err := tc.target.WithTenantReadTx(tc.ctx, tc.scope, func(context.Context, storeport.Store) error { called = true; return nil })
			if err == nil || called {
				return errors.New("invalid read scope/context reached callback")
			}
		}
		if bridge, ok := memory.NarrativeTenantScope(ctx); !ok || bridge != scope {
			return errors.New("narrative bridge missing")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reusing a committed context may not silently open a fresh transaction.
	if err := s.WithTenantReadTx(saved, scope, func(ctx context.Context, _ storeport.Store) error {
		var count int
		return s.queryRow(ctx, `SELECT COUNT(*) FROM learners`).Scan(&count)
	}); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("committed context reopened: %v", err)
	}
	if err := s.WithTenantTx(t.Context(), scope, func(ctx context.Context, _ storeport.Store) error {
		return s.WithTenantReadTx(ctx, scope, func(nested context.Context, _ storeport.Store) error {
			if s.executor(ctx) != s.executor(nested) {
				return errors.New("read inside writer opened another pool")
			}
			_, err := s.exec(nested, `UPDATE learners SET objective='writer inherited' WHERE id='L1'`)
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func assertReadEnforcement(t *testing.T, s *Store) {
	t.Helper()
	scope := models.LegacyPrincipal("L1").TenantScope()
	for _, query := range []string{
		`UPDATE learners SET objective='bad' WHERE id='L1'`,
		`INSERT INTO tenants(id,slug,name,status,region,policy_json,created_at,updated_at) VALUES ('bad','bad','bad','active','default','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP) RETURNING id`,
	} {
		if err := s.WithTenantReadTx(t.Context(), scope, func(ctx context.Context, scoped storeport.Store) error {
			var err error
			if query[0] == 'U' {
				_, err = s.exec(ctx, query)
			} else {
				var id string
				err = scoped.(*Store).queryRow(ctx, query).Scan(&id)
			}
			if err == nil {
				return errors.New("read transaction accepted DML")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTenantReadSQLiteRawFallbackRestoresConnection(t *testing.T) {
	s := sqliteReaderTestStore(t)
	raw := NewStore(s.root)
	assertReadEnforcement(t, raw)
	scope := models.LegacyPrincipal("L1").TenantScope()
	for _, mode := range []string{"success", "error", "panic", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			func() {
				defer func() {
					if p := recover(); p != nil && mode != "panic" {
						t.Fatal(p)
					}
				}()
				_ = raw.WithTenantReadTx(ctx, scope, func(context.Context, storeport.Store) error {
					switch mode {
					case "error":
						return errors.New("rollback")
					case "panic":
						panic("test")
					case "cancel":
						cancel()
						return ctx.Err()
					}
					return nil
				})
			}()
			if _, err := raw.exec(t.Context(), `UPDATE learners SET objective=? WHERE id='L1'`, mode); err != nil {
				t.Fatalf("fallback polluted writer connection: %v", err)
			}
		})
	}
}

func TestTenantReadSQLiteConstructorAndClose(t *testing.T) {
	s := sqliteReaderTestStore(t)
	if _, err := NewSQLiteStore(s.root, filepath.Join(t.TempDir(), "absent.db")); err == nil {
		t.Fatal("accepted missing/different file")
	}
	if err := s.root.Ping(); err != nil {
		t.Fatal("failed constructor took writer ownership")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.reader.Ping(); err == nil {
		t.Fatal("Close leaked reader pool")
	}
	if err := s.root.Ping(); err == nil {
		t.Fatal("Close leaked writer pool")
	}
}

func TestTenantReadNarrativeGlobalBackendUsesReaderTransaction(t *testing.T) {
	s := sqliteReaderTestStore(t)
	keyring, err := NewIntegrationSecretKeyring("test:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrationSecretKeyring(keyring)
	memory.ConfigureNarrativeStore(s)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
	domain, err := s.CreateDomain(t.Context(), "L1", "reader memory", "", models.KnowledgeSpace{Concepts: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	object := seedIndexedSession(t, s, memory.NarrativeKey{LearnerID: "L1", Scope: memory.ScopeSession, Key: "2026-10-14T00-00-00Z"}, domain.ID, "reader context")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// Occupy the sole writer connection. A root backend call that loses the
	// read transaction identity would deadlock instead of completing here.
	writer, err := s.root.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	scope := models.LegacyPrincipal("L1").TenantScope()
	items, err := s.ListRecentSessionNarratives(ctx, scope, "L1", domain.ID, 3)
	if err != nil || len(items) != 1 {
		t.Fatalf("standalone index reader used writer: %v,%v", items, err)
	}
	if err := s.WithTenantReadTx(ctx, scope, func(ctx context.Context, scoped storeport.Store) error {
		view, err := memory.LoadContextForDomainContext(ctx, "L1", domain.ID, "", nil, nil)
		if err != nil {
			return err
		}
		if len(view.RecentSessions) != 1 {
			return errors.New("global narrative backend lost session")
		}
		// The same bridge/identity must also work on the scoped Store.
		_, err = scoped.(*Store).ListRecentSessionNarratives(ctx, scope, "L1", domain.ID, 3)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if _, err := s.root.Exec(`UPDATE narrative_objects SET session_indexed_version=0 WHERE scope='session'`); err != nil {
		t.Fatal(err)
	}
	writer, err = s.root.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	envelope, err := s.readSessionIndexEnvelope(ctx, scope, object.NarrativeKey, 1<<20)
	if err != nil || envelope.version != object.Version {
		t.Fatalf("standalone envelope used writer: %+v,%v", envelope, err)
	}
}
