// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"time"

	"tutor-mcp/models"
)

const sqliteOAuthCSRFMigration = `CREATE TABLE oauth_csrf_replays (
    token_hash TEXT PRIMARY KEY,
    expires_at DATETIME NOT NULL,
    consumed_at DATETIME NOT NULL
);
CREATE INDEX idx_oauth_csrf_replays_expiry ON oauth_csrf_replays(expires_at);`

const postgresOAuthCSRFMigration = `CREATE TABLE oauth_csrf_replays (
    token_hash TEXT PRIMARY KEY,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_oauth_csrf_replays_expiry ON oauth_csrf_replays(expires_at);`

// ConsumeOAuthCSRF atomically admits one fleet-wide use. The token does not
// carry tenant authority and its raw value is never persisted.
func (s *Store) ConsumeOAuthCSRF(ctx context.Context, credential models.OAuthCSRFCredential) (bool, error) {
	now := time.Now().UTC()
	if credential.Token == "" || !credential.ExpiresAt.After(now) || credential.ExpiresAt.After(now.Add(2*time.Hour)) {
		return false, fmt.Errorf("consume OAuth CSRF: invalid credential")
	}
	consumed := false
	err := s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx, `DELETE FROM oauth_csrf_replays WHERE expires_at <= ?`, now); err != nil {
			return err
		}
		result, err := txs.exec(ctx, `INSERT INTO oauth_csrf_replays
			(token_hash, expires_at, consumed_at) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, opaqueTokenHash(credential.Token), credential.ExpiresAt.UTC(), now)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		consumed = count == 1
		return err
	})
	return consumed, err
}
