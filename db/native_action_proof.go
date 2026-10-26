// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// ConsumeNativeActionProof is a durable one-use ledger, separate from business
// idempotency. Retrying an operation requires a fresh signed nonce while keeping
// its original operation key. No private key, request body or bearer is stored.
func (s *Store) ConsumeNativeActionProof(ctx context.Context, p models.Principal, proof models.NativeActionProof) error {
	now := time.Now().UTC()
	if proof.TenantID != p.TenantID || proof.SessionID == "" || len(proof.SessionID) > 128 || !validNativeDigest(proof.EventID) || !validNativeDigest(proof.PayloadHash) || !validNativePublicKey(proof.PublicKey) || proof.CreatedAt.Before(now.Add(-time.Minute)) || proof.CreatedAt.After(now.Add(time.Minute)) || !proof.ExpiresAt.Equal(proof.CreatedAt.Add(time.Minute)) || !models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeFormationContribute) {
		return storeport.ErrInvalidNativeChallenge
	}
	return s.WithTenantTx(ctx, p.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		tx := scoped.(*Store)
		if err := tx.ValidatePrincipal(txCtx, p); err != nil {
			return err
		}
		var matches int
		if err := tx.queryRow(txCtx, `SELECT COUNT(*) FROM native_identities n JOIN tenant_memberships m ON m.user_id=n.user_id JOIN native_sessions ns ON ns.identity_id=n.id AND ns.user_id=n.user_id AND ns.tenant_id=m.tenant_id AND ns.membership_id=m.id AND ns.key_version=n.version WHERE n.user_id=? AND n.public_key=? AND n.status='active' AND m.tenant_id=? AND m.id=? AND m.status='active' AND ns.id=? AND ns.revoked_at IS NULL AND ns.created_at<=? AND ns.expires_at>?`, p.UserID, proof.PublicKey, p.TenantID, p.MembershipID, proof.SessionID, now, now).Scan(&matches); err != nil {
			return err
		}
		if matches != 1 {
			return storeport.ErrInvalidNativeIdentity
		}
		if _, err := tx.exec(txCtx, `DELETE FROM training_receipts WHERE tenant_id=? AND membership_id=? AND operation='native_action_proof' AND created_at<?`, p.TenantID, p.MembershipID, now.Add(-2*time.Minute)); err != nil {
			return err
		}
		result, err := tx.exec(txCtx, `INSERT INTO training_receipts(tenant_id,membership_id,learner_id,idempotency_key,operation,request_hash,response_json,actor_user_id,created_at) VALUES(?,?,?,?,'native_action_proof',?,'{}',?,?) ON CONFLICT(tenant_id,idempotency_key) DO NOTHING`, p.TenantID, p.MembershipID, nullableTrainingLearner(p.LearnerID), "proof:"+proof.EventID, proof.PayloadHash, p.UserID, now)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("action proof already consumed: %w", storeport.ErrInvalidNativeChallenge)
		}
		return nil
	})
}
