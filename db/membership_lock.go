// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import "context"

// Workers cannot update the tenants root. A transaction advisory lock gives
// owner changes and the first erasure phase the same serialization boundary
// without granting workers any tenant-administration privilege.
func (s *Store) lockMembershipChanges(ctx context.Context, tenantID string) error {
	if s.dialect != DialectPostgres {
		return nil
	}
	_, err := s.exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(?))`, "tutor-memberships:"+tenantID)
	return err
}
