// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"tutor-mcp/webhookurl"
)

type webhookHTTPOutcome int

const (
	webhookHTTPDelivered webhookHTTPOutcome = iota
	webhookHTTPRejected
	webhookHTTPUnknown
)

type webhookHTTPResult struct {
	outcome    webhookHTTPOutcome
	statusCode int
	err        error
}

// safeWebhookURL guards payload preparation and redirects against SSRF.
// Tests replace the Discord-only allowlist to exercise loopback HTTP servers.
var safeWebhookURL = webhookurl.IsSafeWebhookURL

func marshalDiscordRequest(url string, payload discordPayload) ([]byte, error) {
	if !safeWebhookURL(url) {
		return nil, fmt.Errorf("unsafe webhook url")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal webhook payload: %w", err)
	}
	return body, nil
}

// doOnceClassified performs exactly one outbound attempt after the caller has
// durably entered the HTTP boundary. A response is known only when the client
// returns it without a transport/redirect error. Any such non-2xx response is
// safe to retry through the durable backoff policy; every transport error is
// ambiguous and must be quarantined rather than replayed automatically.
func (s *Scheduler) doOnceClassified(url string, body []byte) webhookHTTPResult {
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		// net/url errors can contain the credential-bearing webhook path.
		s.logger.Warn("webhook network error", "error_type", fmt.Sprintf("%T", err))
		return webhookHTTPResult{outcome: webhookHTTPUnknown, err: fmt.Errorf("webhook transport outcome unknown")}
	}
	resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return webhookHTTPResult{
			outcome: webhookHTTPRejected, statusCode: resp.StatusCode,
			err: fmt.Errorf("webhook returned %d", resp.StatusCode),
		}
	}
	return webhookHTTPResult{outcome: webhookHTTPDelivered, statusCode: resp.StatusCode}
}

func (s *Scheduler) releaseWebhookClaims(ctx context.Context, learnerID string, queueIDs []int64) {
	for _, queueID := range queueIDs {
		if err := s.store.ReleaseWebhookClaim(ctx, queueID, learnerID); err != nil && s.logger != nil {
			s.logger.Error("scheduler: release pre-http webhook claim",
				"error_type", fmt.Sprintf("%T", err), "learner", learnerID, "queue_id", queueID,
			)
		}
	}
}

// dispatchDurableWebhook owns the complete queue/reservation/HTTP lifecycle.
// No caller may issue the HTTP request itself once a durable queue row has been
// claimed: this helper is the single point that classifies every crash boundary.
func (s *Scheduler) dispatchDurableWebhook(
	ctx context.Context,
	learnerID, alertType, domainID string,
	intrusive bool,
	queueIDs []int64,
	payload discordPayload,
	now time.Time,
) error {
	if s.tenantDBPhase {
		return fmt.Errorf("webhook dispatch cannot run inside a tenant database phase")
	}
	var prepared *preparedWebhookRequest
	var preparationErr error
	if err := s.withTenantStore(ctx, func(txCtx context.Context, child *Scheduler) error {
		prepared, preparationErr = child.prepareDurableWebhook(txCtx, learnerID, alertType, domainID, intrusive, queueIDs, payload, now)
		// A rejected preparation can release a claim or reservation. Persist
		// that cleanup even though no HTTP request will be made.
		return nil
	}); err != nil {
		return fmt.Errorf("commit webhook HTTP boundary: %w", err)
	}
	if preparationErr != nil {
		return preparationErr
	}
	// No tenant transaction or SQL connection survives into the HTTP call.
	// A crash from here leaves committed dispatching rows for quarantine.
	result := s.doOnceClassified(prepared.url, prepared.body)
	if err := s.withTenantStore(ctx, func(txCtx context.Context, child *Scheduler) error {
		switch result.outcome {
		case webhookHTTPDelivered:
			if err := child.store.CompleteWebhookDelivery(txCtx, learnerID, queueIDs, prepared.reservationID, time.Now().UTC()); err != nil {
				// The persisted dispatching state is intentional: reconciliation will
				// quarantine it because Discord accepted the request but the local
				// completion transaction did not commit.
				return fmt.Errorf("complete durable webhook delivery after 2xx: %w", err)
			}
			return nil
		case webhookHTTPRejected:
			reason := fmt.Sprintf("http_%d", result.statusCode)
			if _, err := child.store.RecordKnownWebhookDeliveryFailure(
				txCtx, learnerID, queueIDs, prepared.reservationID, reason, time.Now().UTC(),
			); err != nil {
				return fmt.Errorf("record known webhook delivery failure: %w", err)
			}
			return nil
		case webhookHTTPUnknown:
			if err := child.store.MarkWebhookDeliveryUnknown(
				txCtx, learnerID, queueIDs, prepared.reservationID, "transport_outcome_unknown", time.Now().UTC(),
			); err != nil {
				return fmt.Errorf("quarantine unknown webhook delivery: %w", err)
			}
			return nil
		default:
			return fmt.Errorf("unknown webhook delivery outcome")
		}
	}); err != nil {
		return err
	}
	return result.err
}

type preparedWebhookRequest struct {
	url           string
	body          []byte
	reservationID int64
}

func (s *Scheduler) prepareDurableWebhook(
	ctx context.Context,
	learnerID, alertType, domainID string,
	intrusive bool,
	queueIDs []int64,
	payload discordPayload,
	now time.Time,
) (*preparedWebhookRequest, error) {
	// Resolve plaintext only after all caller-side computation, policy checks,
	// queue claims, and claim revalidation have completed. This is the single
	// runtime boundary where an integration credential enters memory.
	webhookURL, err := s.store.GetWebhookDispatchURL(ctx, learnerID)
	if err != nil {
		s.releaseWebhookClaims(ctx, learnerID, queueIDs)
		return nil, fmt.Errorf("resolve webhook dispatch credential: %w", err)
	}
	body, err := marshalDiscordRequest(webhookURL, payload)
	if err != nil {
		s.releaseWebhookClaims(ctx, learnerID, queueIDs)
		return nil, err
	}

	reservationID, prepared, err := s.store.PrepareWebhookDelivery(
		ctx, learnerID, alertType, domainID, intrusive, queueIDs, now,
	)
	if err != nil {
		s.releaseWebhookClaims(ctx, learnerID, queueIDs)
		return nil, fmt.Errorf("prepare durable webhook delivery: %w", err)
	}
	if !prepared {
		s.releaseWebhookClaims(ctx, learnerID, queueIDs)
		return nil, fmt.Errorf("webhook delivery blocked by notification policy")
	}

	if err := s.store.BeginWebhookDelivery(ctx, learnerID, queueIDs, reservationID, now); err != nil {
		if releaseErr := s.store.ReleasePreparedWebhookDelivery(
			ctx, learnerID, queueIDs, reservationID, "begin_delivery_failed", now,
		); releaseErr != nil && s.logger != nil {
			s.logger.Error("scheduler: release delivery after begin failure",
				"error_type", fmt.Sprintf("%T", releaseErr), "learner", learnerID,
			)
		}
		return nil, fmt.Errorf("begin durable webhook delivery: %w", err)
	}

	return &preparedWebhookRequest{url: webhookURL, body: body, reservationID: reservationID}, nil
}
