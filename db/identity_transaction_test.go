package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestPrincipalValidationWorksInReadOnlyTransaction(t *testing.T) {
	s := setupTestDB(t)
	p, err := s.GetPrincipalForLearner(t.Context(), "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WithTenantReadTx(t.Context(), p.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		return scoped.ValidatePrincipal(ctx, p)
	}); err != nil {
		t.Fatalf("read validation attempted a write or row lock: %v", err)
	}
}

func TestPrincipalWriteTransactionFencesConcurrentRevocation(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("PostgreSQL row-lock regression requires TUTOR_TEST_PG_DSN")
	}
	s := setupTestDB(t)
	s.root.SetMaxOpenConns(3)
	p, err := s.GetPrincipalForLearner(t.Context(), "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- s.WithTenantTx(t.Context(), p.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
			if err := scoped.ValidatePrincipal(ctx, p); err != nil {
				close(locked)
				return err
			}
			close(locked)
			<-release
			return scoped.UpdateLearnerProfile(ctx, p.LearnerID, "{}", nil)
		})
	}()
	<-locked
	// PostgreSQL itself proves the conflicting UPDATE waits for the writer;
	// the assertion does not rely on goroutine timing or a sleep.
	err = s.WithTenantTx(t.Context(), p.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if _, err := txs.exec(ctx, `SET LOCAL lock_timeout = '100ms'`); err != nil {
			return err
		}
		_, err := txs.exec(ctx, `UPDATE tenant_memberships SET status='revoked',version=version+1 WHERE tenant_id=? AND id=?`, p.TenantID, p.MembershipID)
		return err
	})
	close(release)
	if writerErr := <-writerDone; writerErr != nil {
		t.Fatalf("authorized writer: %v", writerErr)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("revocation was not fenced by active writer: %v", err)
	}
	if _, err := s.SetMembershipAuthorization(t.Context(), p.TenantScope(), models.MembershipStatusRevoked, p.Roles); err != nil {
		t.Fatal(err)
	}
	err = s.WithTenantTx(t.Context(), p.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		return scoped.ValidatePrincipal(ctx, p)
	})
	if !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("old authorization after revocation: %v", err)
	}
}

// The HTTP bearer middleware validates on the root store before any tool
// transaction. That pre-check must not wait for (or occupy) the single SQLite
// writer connection; the tool transaction revalidates under its own lock.
func TestRootPrincipalValidationUsesReadTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tutor.db")
	writer, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(writer); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	s, err := NewSQLiteStore(writer, path)
	if err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.EnsureInstallation(t.Context(), "local"); err != nil {
		t.Fatal(err)
	}
	id, err := s.EnsureLocalIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPrincipalForLearner(t.Context(), id, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	busy, err := writer.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := s.ValidatePrincipal(ctx, p); err != nil {
		t.Fatalf("root validation waited for the writer: %v", err)
	}
}
