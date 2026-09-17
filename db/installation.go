package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"tutor-mcp/models"
)

// EnsureInstallation prevents implicit profile conversion. Only an empty
// database may acquire a local/hobby identity. Legacy HTTP remains compatible.
func (s *Store) EnsureInstallation(ctx context.Context, profile string) error {
	if (profile == "local" || profile == "hobby") && s.dialect != DialectSQLite {
		return fmt.Errorf("%s requires SQLite", profile)
	}
	return s.inTx(ctx, nil, func(tx *Store) error {
		var actual string
		err := tx.queryRow(ctx, `SELECT profile FROM installation WHERE singleton = 1`).Scan(&actual)
		if err == nil {
			if actual != profile {
				return fmt.Errorf("database belongs to profile %s, requested %s; use a separate data directory", actual, profile)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if profile == "local" || profile == "hobby" {
			var count int
			if err := tx.queryRow(ctx, `SELECT COUNT(*) FROM learners`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("existing server database cannot be opened as %s", profile)
			}
		}
		_, err = tx.exec(ctx, `INSERT INTO installation (singleton, profile) VALUES (1, ?)`, profile)
		return err
	})
}

// EnsureLocalIdentity runs under SQLite's immediate transaction; concurrent
// stdio processes see the same committed identity and learner-only membership.
func (s *Store) EnsureLocalIdentity(ctx context.Context) (string, error) {
	if s.dialect != DialectSQLite {
		return "", fmt.Errorf("local requires SQLite")
	}
	var id string
	err := s.inTx(ctx, nil, func(tx *Store) error {
		var existing sql.NullString
		if err := tx.queryRow(ctx, `SELECT local_learner_id FROM installation WHERE singleton = 1 AND profile = 'local'`).Scan(&existing); err != nil {
			return err
		}
		if existing.Valid {
			id = existing.String
			return nil
		}
		var err error
		id, err = tx.createOfflineIdentity(ctx, "local", "local", "!no-password")
		if err != nil {
			return err
		}
		_, err = tx.exec(ctx, `UPDATE installation SET local_learner_id = ? WHERE singleton = 1`, id)
		return err
	})
	return id, err
}

func (s *Store) createOfflineIdentity(ctx context.Context, mode, name, hash string) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", err
	}
	if _, err := s.createLearnerWithID(ctx, id, "identity:"+id, hash, "", "", nil); err != nil {
		return "", err
	}
	if _, err := s.exec(ctx, `UPDATE learners SET identity_mode = ? WHERE id = ?`, mode, id); err != nil {
		return "", err
	}
	if _, err := s.exec(ctx, `UPDATE users SET identity_mode = ?, login_name = ?, status = 'active', updated_at = ? WHERE id = ?`, mode, name, time.Now().UTC(), id); err != nil {
		return "", err
	}
	_, err = s.exec(ctx, `UPDATE tenant_memberships SET status = 'active' WHERE user_id = ?`, id)
	return id, err
}

func (s *Store) GetUserByLoginName(ctx context.Context, name string) (*models.User, error) {
	if err := s.requireHobby(ctx); err != nil {
		return nil, err
	}
	name, err := models.NormalizeLoginName(name)
	if err != nil {
		return nil, sql.ErrNoRows
	}
	user := &models.User{}
	err = s.queryRow(ctx, `SELECT id, login_name, identity_mode, password_hash, status, token_version, created_at, updated_at FROM users WHERE login_name = ? AND identity_mode = 'username'`, name).Scan(
		&user.ID, &user.LoginName, &user.IdentityMode, &user.PasswordHash, &user.Status, &user.TokenVersion, &user.CreatedAt, &user.UpdatedAt)
	return user, err
}
