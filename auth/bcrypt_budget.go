// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const (
	defaultBcryptMaxConcurrent = 4
	maxBcryptMaxConcurrent     = 128
	bcryptBusyRetryAfter       = "1"
)

// ErrBcryptBusy means the process-wide CPU budget is already fully occupied.
// Admission is deliberately non-blocking: HTTP handlers must shed excess work
// instead of accumulating an unbounded goroutine queue behind bcrypt.
var ErrBcryptBusy = errors.New("bcrypt concurrency budget exhausted")

type bcryptBudget struct {
	slots    chan struct{}
	generate func([]byte, int) ([]byte, error)
	compare  func([]byte, []byte) error
}

func newBcryptBudget(maxConcurrent int) (*bcryptBudget, error) {
	if maxConcurrent <= 0 || maxConcurrent > maxBcryptMaxConcurrent {
		return nil, fmt.Errorf("bcrypt max concurrency must be between 1 and %d", maxBcryptMaxConcurrent)
	}
	return &bcryptBudget{
		slots:    make(chan struct{}, maxConcurrent),
		generate: bcrypt.GenerateFromPassword,
		compare:  bcrypt.CompareHashAndPassword,
	}, nil
}

func mustBcryptBudget(maxConcurrent int) *bcryptBudget {
	budget, err := newBcryptBudget(maxConcurrent)
	if err != nil {
		panic(err)
	}
	return budget
}

func (b *bcryptBudget) run(ctx context.Context, operation func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrBcryptBusy
	}
	return operation()
}

func (b *bcryptBudget) Generate(ctx context.Context, password []byte, cost int) ([]byte, error) {
	var hash []byte
	err := b.run(ctx, func() error {
		var err error
		hash, err = b.generate(password, cost)
		return err
	})
	return hash, err
}

func (b *bcryptBudget) Compare(ctx context.Context, hash, password []byte) error {
	return b.run(ctx, func() error { return b.compare(hash, password) })
}

// CompareCredential performs the real credential comparison and one padding
// comparison under a single admission slot. Current-cost/unknown credentials
// pay current+minimum work; legacy low-cost credentials pay their stored work
// plus current work. This keeps absent, inactive, wrong-password, and legacy
// account paths close enough that a remote caller cannot cheaply classify the
// account from bcrypt duration while still allowing old valid hashes to log in.
func (b *bcryptBudget) CompareCredential(
	ctx context.Context,
	hash, password, currentCostDummy, minimumCostDummy []byte,
	targetCost int,
) error {
	var credentialErr error
	returnErr := b.run(ctx, func() error {
		credentialErr = b.compare(hash, password)
		paddingHash := minimumCostDummy
		storedCost, err := bcrypt.Cost(hash)
		if err != nil || storedCost < targetCost {
			paddingHash = currentCostDummy
		}
		// Padding always uses a known-valid digest. Its mismatch is expected;
		// only the result of the real comparison determines authentication.
		_ = b.compare(paddingHash, password)
		return nil
	})
	if returnErr != nil {
		return returnErr
	}
	return credentialErr
}

var processBcrypt = struct {
	sync.RWMutex
	budget *bcryptBudget
}{budget: mustBcryptBudget(defaultBcryptMaxConcurrent)}

// ConfigureBcryptMaxConcurrent replaces the process-wide bcrypt executor.
// Call it once during startup, before constructing an OAuthServer.
func ConfigureBcryptMaxConcurrent(maxConcurrent int) error {
	budget, err := newBcryptBudget(maxConcurrent)
	if err != nil {
		return err
	}
	processBcrypt.Lock()
	processBcrypt.budget = budget
	processBcrypt.Unlock()
	return nil
}

func currentBcryptBudget() *bcryptBudget {
	processBcrypt.RLock()
	defer processBcrypt.RUnlock()
	return processBcrypt.budget
}
