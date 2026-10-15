package store

import (
	"context"
	"errors"
	"time"

	"tutor-mcp/models"
)

// ErrWebhookPermanent marks an invalid/revoked integration or subscription.
// Storage failures and unavailable retained keys must remain retryable.
var ErrWebhookPermanent = errors.New("webhook configuration is unavailable or invalid")

type AsyncJobAttemptStore interface {
	// FinalizeAsyncJobAttempt owns an independent tenant transaction. Callers
	// must not invoke it from a tenant transaction or a transaction-scoped Store.
	// The returned outcome reflects retry exhaustion as a dead-letter result.
	FinalizeAsyncJobAttempt(context.Context, models.TenantScope, string, string, int, models.AsyncJobAttemptResult, *models.AsyncJobDeliveryResult, time.Time) (models.AsyncJobOutcome, error)
}
