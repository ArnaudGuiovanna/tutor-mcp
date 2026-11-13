// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"time"

	"tutor-mcp/models"
)

// A credential_tenant_routes row only maps the digest of a presented secret to
// its tenant. It is needed exactly as long as presenting that secret can still
// have an effect:
//
//   - authorization codes, email-verification and password-reset tokens and
//     login challenges are read only while expires_at > now. The route carries
//     the same expiry (a consumed login challenge extends it to trusted_until),
//     so a route whose expires_at has passed can never resolve to a usable
//     credential. Login challenge creation already purges such routes inline.
//   - a refresh token route is also the entry point of replay detection: a
//     rotated (used) ancestor presented again must still be found to revoke its
//     family, and CleanupExpiredRefreshTokens deliberately keeps expired
//     ancestors while any member of the family is live. The route is therefore
//     purged only when it has expired AND its refresh_tokens row is gone, i.e.
//     after the family-aware token cleanup has released it. Without the row a
//     presented token is rejected as unknown whatever the route says.
//
// refresh_tokens.token stores refreshTokenHashPrefix + credential_key, so the
// join below needs no secret beyond what both tables already hold.
//
// On PostgreSQL refresh_tokens is under FORCE ROW LEVEL SECURITY and the
// worker role may not read its token column (it must never see credential
// digests). The purge is therefore a SECURITY DEFINER function owned by the
// migrator, executable by tutor_worker only. It acts solely on the tenant bound
// to the calling transaction (app.current_tenant), uses the database clock, and
// takes no argument, so the caller cannot widen it to unexpired routes or to
// another tenant than the one it is already allowed to scope to.
const postgresCredentialRoutePurgeMigration = `CREATE OR REPLACE FUNCTION tutor_purge_expired_credential_routes()
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
AS $fn$
DECLARE
    bound_tenant text := NULLIF(current_setting('app.current_tenant', true), '');
    purged bigint;
BEGIN
    IF bound_tenant IS NULL THEN
        RETURN 0;
    END IF;
    DELETE FROM credential_tenant_routes route
    WHERE route.tenant_id = bound_tenant
      AND route.expires_at <= now()
      AND (
          route.kind <> 'refresh_token'
          OR NOT EXISTS (
              SELECT 1 FROM refresh_tokens token
              WHERE token.tenant_id = route.tenant_id
                AND token.token = 'sha256:' || route.credential_key
          )
      );
    GET DIAGNOSTICS purged = ROW_COUNT;
    RETURN purged;
END
$fn$;
DO $$
BEGIN
    -- Pin name resolution to the schema the migration runs in: a SECURITY
    -- DEFINER body must never resolve tables through the caller's search_path.
    EXECUTE format('ALTER FUNCTION tutor_purge_expired_credential_routes() SET search_path = %I, pg_temp', current_schema());
END $$;
REVOKE ALL ON FUNCTION tutor_purge_expired_credential_routes() FROM PUBLIC`

// CleanupExpiredCredentialRoutes removes tenant routes whose credential can no
// longer be used and is no longer needed for refresh-token replay detection
// (see postgresCredentialRoutePurgeMigration for the exact rule). On
// PostgreSQL it acts on the tenant bound to the current transaction and is a
// no-op outside a tenant transaction; the scheduler runs it in every tenant
// scope after CleanupExpiredRefreshTokens. SQLite has no row-level security,
// so one call purges every tenant. The worker principal makes this a reviewed
// worker-only entry point of the Store surface (tenant_scope_guard_test.go).
func (s *Store) CleanupExpiredCredentialRoutes(ctx context.Context, worker models.WorkerPrincipal) (int64, error) {
	if !worker.Validate() {
		return 0, fmt.Errorf("cleanup expired credential routes: invalid worker")
	}
	if s.dialect == DialectPostgres {
		var purged int64
		if err := s.queryRow(ctx, `SELECT tutor_purge_expired_credential_routes()`).Scan(&purged); err != nil {
			return 0, fmt.Errorf("cleanup expired credential routes: %w", err)
		}
		return purged, nil
	}
	result, err := s.exec(ctx,
		`DELETE FROM credential_tenant_routes
		 WHERE expires_at <= ?
		   AND (
		       kind <> ?
		       OR NOT EXISTS (
		           SELECT 1 FROM refresh_tokens
		           WHERE refresh_tokens.tenant_id = credential_tenant_routes.tenant_id
		             AND refresh_tokens.token = ? || credential_tenant_routes.credential_key
		       )
		   )`,
		time.Now().UTC(), credentialKindRefreshToken, refreshTokenHashPrefix,
	)
	if err != nil {
		return 0, fmt.Errorf("cleanup expired credential routes: %w", err)
	}
	return result.RowsAffected()
}
