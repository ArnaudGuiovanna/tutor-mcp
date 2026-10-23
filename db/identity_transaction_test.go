package db

import (
	"context"
	"errors"
	"os"
	"testing"

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
