// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"tutor-mcp/models"
)

type InstitutionActionsStore interface {
	InstitutionAction(context.Context, models.Principal, string, models.InstitutionRequest) (any, error)
	ConsumeNativeActionProof(context.Context, models.Principal, models.NativeActionProof) error
}
