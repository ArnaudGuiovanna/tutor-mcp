// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	storeport "tutor-mcp/store"
)

func TestIdempotencyKeyClaimCompleteReplayAndConflict(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	cached, execute, err := s.ClaimIdempotencyKey(ctx, "L1", "record_interaction", "request-1", "hash-a", now)
	if err != nil || !execute || cached != "" {
		t.Fatalf("first claim: cached=%q execute=%v err=%v", cached, execute, err)
	}
	if _, execute, err := s.ClaimIdempotencyKey(ctx, "L1", "record_interaction", "request-1", "hash-a", now); !errors.Is(err, storeport.ErrIdempotencyInProgress) || execute {
		t.Fatalf("duplicate live claim: execute=%v err=%v", execute, err)
	}
	if err := s.CompleteIdempotencyKey(ctx, "L1", "record_interaction", "request-1", "hash-a", `{"updated":true}`, now); err != nil {
		t.Fatal(err)
	}
	cached, execute, err = s.ClaimIdempotencyKey(ctx, "L1", "record_interaction", "request-1", "hash-a", now)
	if err != nil || execute || cached != `{"updated":true}` {
		t.Fatalf("completed replay: cached=%q execute=%v err=%v", cached, execute, err)
	}
	if _, _, err := s.ClaimIdempotencyKey(ctx, "L1", "record_interaction", "request-1", "hash-b", now); !errors.Is(err, storeport.ErrIdempotencyKeyConflict) {
		t.Fatalf("changed payload reused key: %v", err)
	}
	// Scope includes both learner and tool.
	if _, execute, err := s.ClaimIdempotencyKey(ctx, "L1", "record_affect", "request-1", "hash-b", now); err != nil || !execute {
		t.Fatalf("tool-scoped key: execute=%v err=%v", execute, err)
	}
	if _, err := s.exec(ctx, `INSERT INTO learners (id, email, password_hash, objective, created_at)
		VALUES (?, ?, 'hash', 'test', ?)`, "L2", "l2-idempotency@test.invalid", now); err != nil {
		t.Fatal(err)
	}
	if _, execute, err := s.ClaimIdempotencyKey(ctx, "L2", "record_interaction", "request-1", "hash-b", now); err != nil || !execute {
		t.Fatalf("learner-scoped key: execute=%v err=%v", execute, err)
	}
}

func TestIdempotencyKeyConcurrentClaimHasOneOwner(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	var owners atomic.Int32
	var unexpected atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, execute, err := s.ClaimIdempotencyKey(ctx, "L1", "record_interaction", "concurrent", "same-hash", time.Now().UTC())
			switch {
			case err == nil && execute:
				owners.Add(1)
			case errors.Is(err, storeport.ErrIdempotencyInProgress):
			default:
				unexpected.Add(1)
			}
		}()
	}
	wg.Wait()
	if owners.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("owners=%d unexpected=%d", owners.Load(), unexpected.Load())
	}
}

func TestAbortIdempotencyKeyAllowsSafeRetry(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	if _, execute, err := s.ClaimIdempotencyKey(ctx, "L1", "record_affect", "retry", "hash", time.Now()); err != nil || !execute {
		t.Fatalf("claim: execute=%v err=%v", execute, err)
	}
	if err := s.AbortIdempotencyKey(ctx, "L1", "record_affect", "retry", "hash"); err != nil {
		t.Fatal(err)
	}
	if _, execute, err := s.ClaimIdempotencyKey(ctx, "L1", "record_affect", "retry", "hash", time.Now()); err != nil || !execute {
		t.Fatalf("retry claim: execute=%v err=%v", execute, err)
	}
}
