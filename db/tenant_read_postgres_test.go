package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestTenantReadPostgresReadOnlyRLSAndPoolReset(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("TUTOR_TEST_PG_DSN not configured")
	}
	admin := narrativeTestStore(t)
	owner, _ := verifiedOwner(t, admin, seedInstitutionOwner(t, admin, "read-b", "owner@read-b.test"))
	_, token, err := admin.CreateTenantInvitation(t.Context(), owner, "learner@read-b.test", []string{models.RoleLearner}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	membership, err := admin.AcceptTenantInvitationWithNewUser(t.Context(), models.InvitationCredential{Token: token}, "hash")
	if err != nil {
		t.Fatal(err)
	}
	b := models.TenantScope{TenantID: membership.TenantID, UserID: membership.UserID, MembershipID: membership.ID, LearnerID: membership.LearnerID}
	runtime := sessionIndexRuntimeStore(t, admin)
	var role string
	if err := runtime.root.QueryRow(`SELECT current_user`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	quotedRole := pgx.Identifier{role}.Sanitize()
	// Grant the mutations explicitly. SQLSTATE 25006 must prove transaction
	// read-only enforcement, rather than an unrelated missing grant.
	if _, err := admin.root.Exec(`GRANT UPDATE(objective),INSERT ON learners TO ` + quotedRole); err != nil {
		t.Fatal(err)
	}
	a := models.LegacyPrincipal("L1").TenantScope()
	for _, scope := range []models.TenantScope{a, b} {
		if err := runtime.WithTenantReadTx(t.Context(), scope, func(ctx context.Context, scoped storeport.Store) error {
			var readOnly, currentTenant, currentUser string
			if err := runtime.queryRow(ctx, `SELECT current_setting('transaction_read_only'),current_setting('app.current_tenant'),current_user`).Scan(&readOnly, &currentTenant, &currentUser); err != nil {
				return err
			}
			if readOnly != "on" || currentTenant != scope.TenantID || currentUser != role {
				return errors.New("read transaction lost role/scope/read-only setting")
			}
			var total, foreign int
			if err := runtime.queryRow(ctx, `SELECT COUNT(*) FROM learners`).Scan(&total); err != nil {
				return err
			}
			if total < 1 {
				return errors.New("owner learner invisible")
			}
			if err := runtime.queryRow(ctx, `SELECT COUNT(*) FROM learners WHERE tenant_id<>?`, scope.TenantID).Scan(&foreign); err != nil {
				return err
			}
			if foreign != 0 {
				return errors.New("read transaction bypassed tenant RLS")
			}
			for _, target := range []*Store{runtime, scoped.(*Store)} {
				if err := target.WithTenantTx(ctx, scope, func(nested context.Context, _ storeport.Store) error {
					if runtime.executor(nested) != runtime.executor(ctx) {
						return errors.New("nested call opened a writer")
					}
					var mode string
					if err := runtime.queryRow(nested, `SHOW transaction_read_only`).Scan(&mode); err != nil {
						return err
					}
					if mode != "on" {
						return errors.New("nested call promoted reader")
					}
					return nil
				}); err != nil {
					return err
				}
			}
			other := a
			if scope == a {
				other = b
			}
			called := false
			if err := runtime.WithTenantReadTx(ctx, other, func(context.Context, storeport.Store) error { called = true; return nil }); err == nil || called {
				return errors.New("nested scope mismatch accepted")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, returning := range []bool{false, true} {
		err := runtime.WithTenantReadTx(t.Context(), a, func(ctx context.Context, scoped storeport.Store) error {
			if returning {
				var id string
				return scoped.(*Store).queryRow(ctx, `INSERT INTO learners(id,email,password_hash,objective,created_at,tenant_id) VALUES ('read-blocked','read-blocked@test','hash','',CURRENT_TIMESTAMP,?) RETURNING id`, a.TenantID).Scan(&id)
			}
			_, err := runtime.exec(ctx, `UPDATE learners SET objective='forbidden' WHERE id='L1'`)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
			t.Fatalf("read-only mutation returning=%v got %v; want SQLSTATE25006", returning, err)
		}
	}
	// One-slot PG pool is shared; failed read transactions must reset both
	// read-only and SET LOCAL settings before the next writer or tenant.
	if runtime.reader != nil {
		t.Fatal("PostgreSQL allocated a duplicate pool")
	}
	if err := runtime.WithTenantTx(t.Context(), a, func(ctx context.Context, _ storeport.Store) error {
		return runtime.WithTenantReadTx(ctx, a, func(ctx context.Context, _ storeport.Store) error {
			_, err := runtime.exec(ctx, `UPDATE learners SET objective='writer resumed' WHERE id='L1'`)
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := runtime.root.QueryRow(`SELECT COUNT(*) FROM learners`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("read scope leaked through shared PG pool: %d %v", count, err)
	}
}
