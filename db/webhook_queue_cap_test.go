// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	storeport "tutor-mcp/store"
)

// The bound lives in the shared insert helper, so the client tool, the
// scheduler and the once-per-day fallbacks all hit it; other learners are
// unaffected.
func TestEnqueueWebhookMessage_CapsPendingMessagesPerLearner(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	other, err := s.CreateLearner(ctx, "other@example.test", "hash", "", "")
	if err != nil {
		t.Fatalf("create second learner: %v", err)
	}
	scheduled := time.Now().UTC().Add(time.Hour)
	for i := 0; i < MaxPendingWebhookMessagesPerLearner; i++ {
		if _, err := s.EnqueueWebhookMessage(ctx, "L1", "reminder", fmt.Sprintf("m%d", i), scheduled, scheduled.Add(time.Hour), 0); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if _, err := s.EnqueueWebhookMessage(ctx, "L1", "reminder", "overflow", scheduled, scheduled.Add(time.Hour), 0); !errors.Is(err, storeport.ErrWebhookQueueFull) {
		t.Fatalf("overflow enqueue err=%v, want ErrWebhookQueueFull", err)
	}
	if _, err := s.EnqueueWebhookMessage(ctx, other.ID, "reminder", "fine", scheduled, scheduled.Add(time.Hour), 0); err != nil {
		t.Fatalf("other learner blocked: %v", err)
	}
}
