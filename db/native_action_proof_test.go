// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"
	"tutor-mcp/models"
)

func TestNativeActionProofIsIdentityBoundDurableAndSingleUse(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "native-proof")
	p := trainingPrincipal(t, s, tenant, 177, true)
	other := trainingPrincipal(t, s, tenant, 179, false)
	n, err := s.GetNativeIdentity(t.Context(), tenant, nativeTestKey(177), "session")
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "action-proof-session"
	if _, err = s.CreateNativeSession(t.Context(), nativeTestSession(n, sessionID, p.Scopes)); err != nil {
		t.Fatal(err)
	}
	id := sha256.Sum256([]byte("proof-event"))
	payload := sha256.Sum256([]byte("signed-request"))
	created := time.Now().UTC().Truncate(time.Second)
	proof := models.NativeActionProof{EventID: hex.EncodeToString(id[:]), PublicKey: nativeTestKey(177), PayloadHash: hex.EncodeToString(payload[:]), TenantID: tenant, SessionID: sessionID, CreatedAt: created, ExpiresAt: created.Add(time.Minute)}
	if err := s.ConsumeNativeActionProof(t.Context(), other, proof); err == nil {
		t.Fatal("another account consumed the host proof")
	}
	foreign := proof
	foreign.TenantID = "other-institution"
	if err := s.ConsumeNativeActionProof(t.Context(), p, foreign); err == nil {
		t.Fatal("foreign institution binding accepted")
	}
	foreign = proof
	foreign.SessionID = "unknown-native-session"
	if err := s.ConsumeNativeActionProof(t.Context(), p, foreign); err == nil {
		t.Fatal("unknown native session binding accepted")
	}
	var wait sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() { defer wait.Done(); results <- s.ConsumeNativeActionProof(t.Context(), p, proof) }()
	}
	wait.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("proof accepted %d times", accepted)
	}
	if err := s.ConsumeNativeActionProof(t.Context(), p, proof); err == nil {
		t.Fatal("proof replay accepted")
	}
	proof.CreatedAt = created.Add(-2 * time.Minute)
	proof.ExpiresAt = proof.CreatedAt.Add(time.Minute)
	if err := s.ConsumeNativeActionProof(t.Context(), p, proof); err == nil {
		t.Fatal("expired action proof accepted")
	}
}
