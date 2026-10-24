// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"time"

	"tutor-mcp/models"
)

var (
	ErrInvalidNativeIdentity  = errors.New("invalid native identity")
	ErrInvalidNativeChallenge = errors.New("invalid native challenge")
	ErrInvalidNativeSession   = errors.New("invalid native session")
	ErrNativeKeyConflict      = errors.New("native public key already registered")
)

// NativeIdentityStore persists public keys and bounded grants only. Signature
// verification belongs to authentication; all state transitions recheck the
// current identity and membership to fence revocation and key rotation.
type NativeIdentityStore interface {
	GetNativeIdentity(context.Context, string, string, string) (*models.NativeIdentity, error)
	CreateNativeChallenge(context.Context, *models.NativeChallenge) error
	ConsumeNativeChallenge(context.Context, string, time.Time) (*models.NativeChallenge, error)
	CreateNativeSession(context.Context, *models.NativeSession) (models.Principal, error)
	RotateNativeIdentity(context.Context, string, int64, string, string, time.Time) (*models.NativeIdentity, error)
	RevokeNativeSession(context.Context, string, string, time.Time) error
	ValidateNativeSession(context.Context, string, models.Principal, time.Time) error
	BootstrapNativeOwner(context.Context, models.ControlPlanePrincipal, models.NativeOwnerProvision) (*models.NativeIdentity, error)
	ProvisionNativeMember(context.Context, models.ControlPlanePrincipal, models.NativeOwnerProvision, []string) (*models.NativeIdentity, error)
	BindNativeIdentity(context.Context, models.ControlPlanePrincipal, string, models.NativeOwnerProvision) (*models.NativeIdentity, error)
}
