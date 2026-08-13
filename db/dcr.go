// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const dcrRegistrationLockID int64 = 0x7475746f72444352

var dcrTokenIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func dcrRegistrationSecretAAD(clientID, fingerprint string) []byte {
	return []byte(fmt.Sprintf(
		"tutor-mcp\x00oauth_clients\x00registration_secret\x00%d:%s\x00%d:%s",
		len(clientID), clientID, len(fingerprint), fingerprint,
	))
}

func validateDCRToken(token models.DCRInitialAccessToken) error {
	if !dcrTokenIDPattern.MatchString(token.TokenID) {
		return fmt.Errorf("DCR token ID must use 1..64 safe characters")
	}
	if len(token.TokenHash) != 64 || strings.Trim(token.TokenHash, "0123456789abcdef") != "" {
		return fmt.Errorf("DCR token hash must be lowercase SHA-256")
	}
	if strings.TrimSpace(token.Label) == "" || len(token.Label) > 120 {
		return fmt.Errorf("DCR token label must use 1..120 bytes")
	}
	if token.MaxRegistrations < 1 || token.MaxRegistrations > 100000 {
		return fmt.Errorf("DCR token max registrations must be between 1 and 100000")
	}
	if token.CreatedAt.IsZero() {
		return fmt.Errorf("DCR token creation time is required")
	}
	if token.ExpiresAt != nil && !token.ExpiresAt.After(token.CreatedAt) {
		return fmt.Errorf("DCR token expiry must follow creation")
	}
	if strings.TrimSpace(token.CreatedBy) == "" || len(token.CreatedBy) > 120 {
		return fmt.Errorf("DCR token actor must use 1..120 bytes")
	}
	return nil
}

func (s *Store) lockDCRRegistration(ctx context.Context) error {
	if s.dialect != DialectPostgres {
		return nil
	}
	if _, err := s.exec(ctx, `SELECT pg_advisory_xact_lock(?)`, dcrRegistrationLockID); err != nil {
		return fmt.Errorf("lock DCR registry: %w", err)
	}
	return nil
}

func (s *Store) appendDCRAudit(ctx context.Context, action, actor, tokenID, clientID string, detail any, at time.Time) error {
	detailJSON := []byte(`{}`)
	if detail != nil {
		encoded, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("encode DCR audit detail: %w", err)
		}
		detailJSON = encoded
	}
	_, err := s.exec(ctx,
		`INSERT INTO oauth_dcr_audit
		    (action, actor, token_id, client_id, detail_json, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		action, actor, tokenID, clientID, string(detailJSON), at.UTC(),
	)
	if err != nil {
		return fmt.Errorf("append DCR audit: %w", err)
	}
	return nil
}

// EnsureDCRInitialAccessToken installs a bootstrapping configuration token
// once. Changing the configured token adds a second active capability so a
// rolling deployment has an overlap window. A revoked token is never silently
// resurrected by restart.
func (s *Store) EnsureDCRInitialAccessToken(ctx context.Context, tokenID, tokenHash, label string, maxRegistrations int, now time.Time) error {
	token := models.DCRInitialAccessToken{
		TokenID: tokenID, TokenHash: tokenHash, Label: label,
		MaxRegistrations: maxRegistrations, CreatedAt: now.UTC(), CreatedBy: "startup-config",
	}
	if err := validateDCRToken(token); err != nil {
		return err
	}
	return s.inTx(ctx, nil, func(txs *Store) error {
		if err := txs.lockDCRRegistration(ctx); err != nil {
			return err
		}
		var existingID string
		var expiresAt, revokedAt sql.NullTime
		err := txs.queryRow(ctx,
			`SELECT token_id, expires_at, revoked_at
			 FROM oauth_dcr_initial_access_tokens WHERE token_hash = ?`, tokenHash,
		).Scan(&existingID, &expiresAt, &revokedAt)
		switch {
		case err == nil:
			if revokedAt.Valid || expiresAt.Valid && !expiresAt.Time.After(now) {
				return storeport.ErrDCRInvalidInitialAccessToken
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("load configured DCR token: %w", err)
		}
		if _, err := txs.exec(ctx,
			`INSERT INTO oauth_dcr_initial_access_tokens
			    (token_id, token_hash, label, max_registrations,
			     used_registrations, created_at, expires_at, revoked_at, created_by)
			 VALUES (?, ?, ?, ?, 0, ?, NULL, NULL, ?)`,
			token.TokenID, token.TokenHash, token.Label, token.MaxRegistrations,
			token.CreatedAt, token.CreatedBy,
		); err != nil {
			return fmt.Errorf("create configured DCR token: %w", err)
		}
		return txs.appendDCRAudit(ctx, "token_created", token.CreatedBy, token.TokenID, "", map[string]any{
			"source": "configuration", "max_registrations": token.MaxRegistrations,
		}, token.CreatedAt)
	})
}

func (s *Store) GetActiveDCRInitialAccessToken(ctx context.Context, tokenHash string, now time.Time) (*models.DCRInitialAccessToken, error) {
	if len(tokenHash) != 64 || strings.Trim(tokenHash, "0123456789abcdef") != "" {
		return nil, storeport.ErrDCRInvalidInitialAccessToken
	}
	token := &models.DCRInitialAccessToken{}
	var createdAt flexTime
	var expiresAt, revokedAt flexTime
	err := s.queryRow(ctx,
		`SELECT token_id, token_hash, label, max_registrations,
		        used_registrations, created_at, expires_at, revoked_at, created_by
		 FROM oauth_dcr_initial_access_tokens
		 WHERE token_hash = ? AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > ?)`,
		tokenHash, now.UTC(),
	).Scan(
		&token.TokenID, &token.TokenHash, &token.Label, &token.MaxRegistrations,
		&token.UsedRegistrations, &createdAt, &expiresAt, &revokedAt, &token.CreatedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storeport.ErrDCRInvalidInitialAccessToken
	}
	if err != nil {
		return nil, fmt.Errorf("lookup DCR token: %w", err)
	}
	token.CreatedAt = createdAt.Time.UTC()
	if expiresAt.Valid {
		expires := expiresAt.Time.UTC()
		token.ExpiresAt = &expires
	}
	if revokedAt.Valid {
		revoked := revokedAt.Time.UTC()
		token.RevokedAt = &revoked
	}
	return token, nil
}

// CreateDCRInitialAccessToken is the administration boundary used for manual
// issue and overlap rotation. previousTokenID is audit context only: the old
// capability intentionally remains valid until a separate, explicit revoke.
func (s *Store) CreateDCRInitialAccessToken(ctx context.Context, token models.DCRInitialAccessToken, previousTokenID string) error {
	if err := validateDCRToken(token); err != nil {
		return err
	}
	if previousTokenID != "" && !dcrTokenIDPattern.MatchString(previousTokenID) {
		return fmt.Errorf("previous DCR token ID is invalid")
	}
	return s.inTx(ctx, nil, func(txs *Store) error {
		if err := txs.lockDCRRegistration(ctx); err != nil {
			return err
		}
		if previousTokenID != "" {
			var one int
			if err := txs.queryRow(ctx,
				`SELECT 1 FROM oauth_dcr_initial_access_tokens
				 WHERE token_id = ? AND revoked_at IS NULL
				   AND (expires_at IS NULL OR expires_at > ?)`,
				previousTokenID, token.CreatedAt,
			).Scan(&one); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return storeport.ErrDCRInvalidInitialAccessToken
				}
				return fmt.Errorf("load previous DCR token: %w", err)
			}
		}
		if _, err := txs.exec(ctx,
			`INSERT INTO oauth_dcr_initial_access_tokens
			    (token_id, token_hash, label, max_registrations,
			     used_registrations, created_at, expires_at, revoked_at, created_by)
			 VALUES (?, ?, ?, ?, 0, ?, ?, NULL, ?)`,
			token.TokenID, token.TokenHash, token.Label, token.MaxRegistrations,
			token.CreatedAt.UTC(), token.ExpiresAt, token.CreatedBy,
		); err != nil {
			return fmt.Errorf("create DCR token: %w", err)
		}
		action := "token_created"
		detail := map[string]any{"max_registrations": token.MaxRegistrations}
		if previousTokenID != "" {
			action = "rotation_started"
			detail["previous_token_id"] = previousTokenID
		}
		if token.ExpiresAt != nil {
			detail["expires_at"] = token.ExpiresAt.UTC().Format(time.RFC3339)
		}
		return txs.appendDCRAudit(ctx, action, token.CreatedBy, token.TokenID, "", detail, token.CreatedAt)
	})
}

func (s *Store) RevokeDCRInitialAccessToken(ctx context.Context, tokenID, actor, reason string, now time.Time) (bool, error) {
	if !dcrTokenIDPattern.MatchString(tokenID) {
		return false, fmt.Errorf("DCR token ID is invalid")
	}
	if strings.TrimSpace(actor) == "" || len(actor) > 120 {
		return false, fmt.Errorf("DCR revoke actor must use 1..120 bytes")
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return false, fmt.Errorf("DCR revoke reason must use 1..500 bytes")
	}
	applied := false
	err := s.inTx(ctx, nil, func(txs *Store) error {
		if err := txs.lockDCRRegistration(ctx); err != nil {
			return err
		}
		result, err := txs.exec(ctx,
			`UPDATE oauth_dcr_initial_access_tokens SET revoked_at = ?
			 WHERE token_id = ? AND revoked_at IS NULL`, now.UTC(), tokenID,
		)
		if err != nil {
			return fmt.Errorf("revoke DCR token: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("revoke DCR token rows: %w", err)
		}
		if rows == 0 {
			var one int
			if err := txs.queryRow(ctx, `SELECT 1 FROM oauth_dcr_initial_access_tokens WHERE token_id = ?`, tokenID).Scan(&one); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return storeport.ErrDCRInvalidInitialAccessToken
				}
				return err
			}
			return nil
		}
		applied = true
		return txs.appendDCRAudit(ctx, "token_revoked", actor, tokenID, "", map[string]any{"reason": reason}, now)
	})
	return applied, err
}

func (s *Store) RegisterDynamicOAuthClient(ctx context.Context, registration models.DynamicClientRegistration) (*models.DynamicClientRegistrationResult, error) {
	if len(registration.Fingerprint) != 64 || strings.Trim(registration.Fingerprint, "0123456789abcdef") != "" {
		return nil, fmt.Errorf("DCR registration fingerprint must be lowercase SHA-256")
	}
	if strings.TrimSpace(registration.CandidateClientID) == "" || registration.ExpiresAt.IsZero() || registration.Now.IsZero() {
		return nil, fmt.Errorf("DCR registration identity and timestamps are required")
	}
	if registration.MaxClients < 1 {
		return nil, fmt.Errorf("DCR global client cap must be positive")
	}
	if registration.CandidateClientSecret == "" != (registration.CandidateClientSecretHash == "") {
		return nil, fmt.Errorf("DCR client secret and hash must be supplied together")
	}

	secretCiphertext := ""
	if registration.CandidateClientSecret != "" && s.secretKeyring != nil {
		var err error
		secretCiphertext, err = s.secretKeyring.encryptWithAAD(
			registration.CandidateClientSecret,
			dcrRegistrationSecretAAD(registration.CandidateClientID, registration.Fingerprint),
		)
		if err != nil {
			return nil, fmt.Errorf("encrypt DCR replay secret: %w", err)
		}
	}

	var result *models.DynamicClientRegistrationResult
	err := s.inTx(ctx, nil, func(txs *Store) error {
		if err := txs.lockDCRRegistration(ctx); err != nil {
			return err
		}
		tokenAtQuota := false
		if registration.TokenID != "" {
			var maxRegistrations, usedRegistrations int
			var expiresAt, revokedAt sql.NullTime
			lockSuffix := ""
			if txs.dialect == DialectPostgres {
				lockSuffix = " FOR UPDATE"
			}
			err := txs.queryRow(ctx,
				`SELECT max_registrations, used_registrations, expires_at, revoked_at
				 FROM oauth_dcr_initial_access_tokens WHERE token_id = ?`+lockSuffix,
				registration.TokenID,
			).Scan(&maxRegistrations, &usedRegistrations, &expiresAt, &revokedAt)
			if errors.Is(err, sql.ErrNoRows) || revokedAt.Valid || expiresAt.Valid && !expiresAt.Time.After(registration.Now) {
				return storeport.ErrDCRInvalidInitialAccessToken
			}
			if err != nil {
				return fmt.Errorf("revalidate DCR token: %w", err)
			}
			tokenAtQuota = usedRegistrations >= maxRegistrations
		}

		var clientID, secretHash, replayCiphertext string
		var issuedAt, expiresAt flexTime
		err := txs.queryRow(ctx,
			`SELECT client_id, client_secret_hash, registration_secret_ciphertext, created_at, expires_at
			 FROM oauth_clients WHERE registration_fingerprint = ?`,
			registration.Fingerprint,
		).Scan(&clientID, &secretHash, &replayCiphertext, &issuedAt, &expiresAt)
		switch {
		case err == nil:
			if !expiresAt.Valid || !expiresAt.Time.After(registration.Now) {
				return storeport.ErrDCRReplaySecretUnavailable
			}
			secret := ""
			if secretHash != "" {
				if replayCiphertext == "" || txs.secretKeyring == nil {
					return storeport.ErrDCRReplaySecretUnavailable
				}
				secret, err = txs.secretKeyring.decryptWithAAD(
					replayCiphertext, dcrRegistrationSecretAAD(clientID, registration.Fingerprint),
				)
				if err != nil {
					return fmt.Errorf("authenticate DCR replay secret: %w", err)
				}
			}
			result = &models.DynamicClientRegistrationResult{
				ClientID: clientID, ClientSecret: secret,
				IssuedAt: issuedAt.Time.UTC(), ExpiresAt: expiresAt.Time.UTC(), Replayed: true,
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("load equivalent DCR client: %w", err)
		}
		if tokenAtQuota {
			return storeport.ErrDCRInitialAccessTokenQuota
		}

		var clientCount int
		if err := txs.queryRow(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&clientCount); err != nil {
			return fmt.Errorf("count DCR clients: %w", err)
		}
		if clientCount >= registration.MaxClients {
			return storeport.ErrOAuthClientLimitReached
		}
		if _, err := txs.exec(ctx,
			`INSERT INTO oauth_clients
			    (client_id, client_name, redirect_uris, client_secret_hash, expires_at,
			     registration_fingerprint, registration_token_id, registration_secret_ciphertext)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			registration.CandidateClientID, registration.ClientName, registration.RedirectURIsJSON,
			registration.CandidateClientSecretHash, registration.ExpiresAt.UTC(),
			registration.Fingerprint, registration.TokenID, secretCiphertext,
		); err != nil {
			return fmt.Errorf("create dynamic OAuth client: %w", err)
		}
		if registration.TokenID != "" {
			updated, err := txs.exec(ctx,
				`UPDATE oauth_dcr_initial_access_tokens
				 SET used_registrations = used_registrations + 1
				 WHERE token_id = ? AND revoked_at IS NULL
				   AND (expires_at IS NULL OR expires_at > ?)
				   AND used_registrations < max_registrations`,
				registration.TokenID, registration.Now.UTC(),
			)
			if err != nil {
				return fmt.Errorf("consume DCR token quota: %w", err)
			}
			if rows, _ := updated.RowsAffected(); rows != 1 {
				return storeport.ErrDCRInitialAccessTokenQuota
			}
		}
		if err := txs.appendDCRAudit(ctx, "client_registered", "oauth-register", registration.TokenID, registration.CandidateClientID, map[string]any{
			"confidential": registration.CandidateClientSecret != "",
		}, registration.Now); err != nil {
			return err
		}
		result = &models.DynamicClientRegistrationResult{
			ClientID:     registration.CandidateClientID,
			ClientSecret: registration.CandidateClientSecret,
			IssuedAt:     registration.Now.UTC(),
			ExpiresAt:    registration.ExpiresAt.UTC(),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ListDCRInitialAccessTokens(ctx context.Context, limit int) ([]models.DCRInitialAccessToken, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("DCR token list limit must be between 1 and 1000")
	}
	rows, err := s.query(ctx,
		`SELECT token_id, label, max_registrations, used_registrations,
		        created_at, expires_at, revoked_at, created_by
		 FROM oauth_dcr_initial_access_tokens
		 ORDER BY created_at DESC, token_id LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list DCR tokens: %w", err)
	}
	defer rows.Close()
	var tokens []models.DCRInitialAccessToken
	for rows.Next() {
		var token models.DCRInitialAccessToken
		var createdAt, expiresAt, revokedAt flexTime
		if err := rows.Scan(
			&token.TokenID, &token.Label, &token.MaxRegistrations, &token.UsedRegistrations,
			&createdAt, &expiresAt, &revokedAt, &token.CreatedBy,
		); err != nil {
			return nil, fmt.Errorf("scan DCR token: %w", err)
		}
		token.CreatedAt = createdAt.Time.UTC()
		if expiresAt.Valid {
			value := expiresAt.Time.UTC()
			token.ExpiresAt = &value
		}
		if revokedAt.Valid {
			value := revokedAt.Time.UTC()
			token.RevokedAt = &value
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Store) ListDCRAudit(ctx context.Context, limit int) ([]models.DCRAuditEvent, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("DCR audit list limit must be between 1 and 1000")
	}
	rows, err := s.query(ctx,
		`SELECT id, action, actor, token_id, client_id, detail_json, occurred_at
		 FROM oauth_dcr_audit ORDER BY occurred_at DESC, id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list DCR audit: %w", err)
	}
	defer rows.Close()
	var events []models.DCRAuditEvent
	for rows.Next() {
		var event models.DCRAuditEvent
		var occurredAt flexTime
		if err := rows.Scan(
			&event.ID, &event.Action, &event.Actor, &event.TokenID,
			&event.ClientID, &event.DetailJSON, &occurredAt,
		); err != nil {
			return nil, fmt.Errorf("scan DCR audit: %w", err)
		}
		event.OccurredAt = occurredAt.Time.UTC()
		events = append(events, event)
	}
	return events, rows.Err()
}
