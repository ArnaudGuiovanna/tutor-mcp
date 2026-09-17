package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
)

func (s *Store) requireHobby(ctx context.Context) error {
	var profile string
	if s.dialect != DialectSQLite {
		return fmt.Errorf("hobby administration requires SQLite")
	}
	if err := s.queryRow(ctx, `SELECT profile FROM installation WHERE singleton = 1`).Scan(&profile); err != nil {
		return err
	}
	if profile != "hobby" {
		return fmt.Errorf("hobby administration requires a hobby installation")
	}
	return nil
}

func hobbyLinkHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CreateHobbyLink returns the one-time secret only to the SSH operator. Reset
// issuance revokes existing grants immediately; consumption revokes again so
// credentials issued between those two events cannot survive the reset.
func (s *Store) CreateHobbyLink(ctx context.Context, purpose, name string) (string, error) {
	if err := s.requireHobby(ctx); err != nil {
		return "", err
	}
	if purpose != "invite" && purpose != "reset" {
		return "", fmt.Errorf("invalid link purpose")
	}
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		return "", err
	}
	raw := base64.RawURLEncoding.EncodeToString(rawBytes)
	err := s.inTx(ctx, nil, func(tx *Store) error {
		var learnerID any
		ttl := 24 * time.Hour
		if purpose == "reset" {
			user, err := tx.GetUserByLoginName(ctx, name)
			if err != nil {
				return err
			}
			if user.Status != models.UserStatusActive {
				return fmt.Errorf("account is disabled")
			}
			learnerID = user.ID
			ttl = 15 * time.Minute
			if err = tx.revokeHobbyCredentials(ctx, user.ID); err != nil {
				return err
			}
		}
		if _, err := tx.exec(ctx, `DELETE FROM hobby_account_links WHERE expires_at <= ? OR consumed_at IS NOT NULL`, time.Now().UTC()); err != nil {
			return err
		}
		_, err := tx.exec(ctx, `INSERT INTO hobby_account_links (token_hash, purpose, learner_id, expires_at) VALUES (?, ?, ?, ?)`, hobbyLinkHash(raw), purpose, learnerID, time.Now().UTC().Add(ttl))
		return err
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

func (s *Store) HobbyLinkValid(ctx context.Context, raw, purpose string) bool {
	if len(raw) != 43 || s.requireHobby(ctx) != nil {
		return false
	}
	var count int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM hobby_account_links WHERE token_hash = ? AND purpose = ? AND consumed_at IS NULL AND expires_at > ?`, hobbyLinkHash(raw), purpose, time.Now().UTC()).Scan(&count)
	return err == nil && count == 1
}

func (s *Store) AcceptHobbyInvite(ctx context.Context, raw, name, passwordHash string) (string, error) {
	if err := s.requireHobby(ctx); err != nil {
		return "", err
	}
	name, err := models.NormalizeLoginName(name)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(passwordHash, "$2") {
		return "", fmt.Errorf("password hash required")
	}
	var id string
	err = s.inTx(ctx, nil, func(tx *Store) error {
		if err := tx.consumeHobbyLink(ctx, raw, "invite"); err != nil {
			return err
		}
		var err error
		id, err = tx.createOfflineIdentity(ctx, "username", name, passwordHash)
		return err
	})
	return id, err
}

func (s *Store) consumeHobbyLink(ctx context.Context, raw, purpose string) error {
	if len(raw) != 43 {
		return fmt.Errorf("invalid or expired link")
	}
	result, err := s.exec(ctx, `UPDATE hobby_account_links SET consumed_at = ? WHERE token_hash = ? AND purpose = ? AND consumed_at IS NULL AND expires_at > ?`, time.Now().UTC(), hobbyLinkHash(raw), purpose, time.Now().UTC())
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("invalid or expired link")
	}
	return nil
}

func (s *Store) ResetHobbyPassword(ctx context.Context, raw, passwordHash string) error {
	if err := s.requireHobby(ctx); err != nil {
		return err
	}
	if !strings.HasPrefix(passwordHash, "$2") {
		return fmt.Errorf("password hash required")
	}
	return s.inTx(ctx, nil, func(tx *Store) error {
		if err := tx.consumeHobbyLink(ctx, raw, "reset"); err != nil {
			return err
		}
		var id string
		if err := tx.queryRow(ctx, `SELECT h.learner_id FROM hobby_account_links h JOIN users u ON u.id = h.learner_id WHERE h.token_hash = ? AND u.identity_mode = 'username' AND u.status = 'active'`, hobbyLinkHash(raw)).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `UPDATE learners SET password_hash = ? WHERE id = ? AND identity_mode = 'username'`, passwordHash, id); err != nil {
			return err
		}
		return tx.revokeHobbyCredentials(ctx, id)
	})
}

func (s *Store) revokeHobbyCredentials(ctx context.Context, id string) error {
	// Membership versions are validated on every authenticated HTTP request.
	if _, err := s.exec(ctx, `UPDATE tenant_memberships SET version = version + 1, updated_at = ? WHERE user_id = ?`, time.Now().UTC(), id); err != nil {
		return err
	}
	if _, err := s.exec(ctx, `UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE learner_id = ?`, time.Now().UTC(), id); err != nil {
		return err
	}
	for _, table := range []string{"oauth_codes", "learner_approved_clients", "login_challenges", "account_tokens", "credential_tenant_routes"} {
		// Static table names only; no operator or HTTP input enters the SQL.
		if _, err := s.exec(ctx, `DELETE FROM `+table+` WHERE learner_id = ?`, id); err != nil {
			return err
		}
	}
	_, err := s.exec(ctx, `UPDATE hobby_account_links SET consumed_at = ? WHERE learner_id = ? AND consumed_at IS NULL`, time.Now().UTC(), id)
	return err
}

func (s *Store) DisableHobbyUser(ctx context.Context, name string) error {
	if err := s.requireHobby(ctx); err != nil {
		return err
	}
	return s.inTx(ctx, nil, func(tx *Store) error {
		user, err := tx.GetUserByLoginName(ctx, name)
		if err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `UPDATE users SET status = 'suspended', token_version = token_version + 1, updated_at = ? WHERE id = ?`, time.Now().UTC(), user.ID); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `UPDATE tenant_memberships SET status = 'suspended' WHERE user_id = ?`, user.ID); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `UPDATE learners SET webhook_url = '' WHERE id = ?`, user.ID); err != nil {
			return err
		}
		return tx.revokeHobbyCredentials(ctx, user.ID)
	})
}

func (s *Store) ListHobbyUsers(ctx context.Context) ([]models.User, error) {
	if err := s.requireHobby(ctx); err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, `SELECT login_name, status FROM users WHERE identity_mode = 'username' ORDER BY login_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []models.User
	for rows.Next() {
		var user models.User
		if err := rows.Scan(&user.LoginName, &user.Status); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}
