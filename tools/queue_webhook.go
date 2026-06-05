// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type QueueWebhookMessageParams struct {
	Kind         string               `json:"kind" jsonschema:"nudge type: daily_motivation | daily_recap | reactivation | reminder | mirror_message | olm:<domain_id> (per-domain OLM snapshot)"`
	ScheduledFor string               `json:"scheduled_for" jsonschema:"ISO 8601 UTC timestamp for the delivery window (e.g. 2026-04-13T08:00:00Z)"`
	ExpiresAt    string               `json:"expires_at,omitempty" jsonschema:"ISO 8601 UTC timestamp after which the message must not be sent"`
	Content      string               `json:"content,omitempty" jsonschema:"legacy markdown content ready to post to the Discord webhook; prefer brief for pedagogical nudges"`
	Brief        *models.WebhookBrief `json:"brief,omitempty" jsonschema:"structured learner-facing nudge: why_now, learning_gain, open_loop, next_action; concise, user-friendly, no internal tool names"`
	Priority     int                  `json:"priority,omitempty" jsonschema:"priority (higher = more important, default 0)"`
}

const maxWebhookContentLen = 1500

func registerQueueWebhookMessage(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "queue_webhook_message",
		Description: "Queue a nudge message that the scheduler will post to the learner's Discord webhook at the desired window. The LLM composes the text (warm, no raw KPIs); the scheduler dispatches.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params QueueWebhookMessageParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "queue_webhook_message", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		if !validWebhookKind(params.Kind) {
			r, _ := errorResult(fmt.Sprintf("invalid kind %q (expected daily_motivation | daily_recap | reactivation | reminder | mirror_message | olm:<domain_id>)", params.Kind))
			return r, nil, nil
		}
		if params.ScheduledFor == "" {
			r, _ := errorResult("scheduled_for is required")
			return r, nil, nil
		}
		content := strings.TrimSpace(params.Content)
		if params.Brief != nil {
			brief := *params.Brief
			brief.Normalize(params.Kind)
			if err := validateWebhookBrief(brief); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
			encoded, err := models.EncodeWebhookBrief(brief)
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "invalid brief", err)
				return r, nil, nil
			}
			content = encoded
		}
		if content == "" {
			r, _ := errorResult("content or brief is required")
			return r, nil, nil
		}
		if len(content) > maxWebhookContentLen {
			r, _ := errorResult(fmt.Sprintf("content too long (%d chars, max %d)", len(content), maxWebhookContentLen))
			return r, nil, nil
		}

		scheduledFor, err := time.Parse(time.RFC3339, params.ScheduledFor)
		if err != nil {
			r, _ := errorResult(fmt.Sprintf("scheduled_for must be RFC3339 (got %q)", params.ScheduledFor))
			return r, nil, nil
		}

		var expiresAt time.Time
		if params.ExpiresAt != "" {
			parsed, perr := time.Parse(time.RFC3339, params.ExpiresAt)
			if perr != nil {
				r, _ := errorResult(fmt.Sprintf("expires_at must be RFC3339 (got %q)", params.ExpiresAt))
				return r, nil, nil
			}
			expiresAt = parsed
		}

		id, err := deps.Store.EnqueueWebhookMessage(learnerID, params.Kind, content, scheduledFor, expiresAt, params.Priority)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to enqueue", err)
			return r, nil, nil
		}

		r, _ := jsonResult(map[string]any{
			"queue_id":      id,
			"kind":          params.Kind,
			"scheduled_for": scheduledFor.UTC().Format(time.RFC3339),
		})
		return r, nil, nil
	})
}

func validWebhookKind(k string) bool {
	switch k {
	case models.WebhookKindDailyMotivation,
		models.WebhookKindDailyRecap,
		models.WebhookKindReactivation,
		models.WebhookKindReminder,
		models.WebhookKindMirror:
		return true
	}
	// olm:<domain_id> — used by the OLM dispatch to allow one queued
	// message per domain, since a learner can have multiple active domains
	// each with its own state snapshot.
	if strings.HasPrefix(k, "olm:") && len(k) > len("olm:") {
		return true
	}
	return false
}

func validateWebhookBrief(brief models.WebhookBrief) error {
	required := []struct {
		name  string
		value string
		max   int
	}{
		{"brief.why_now", brief.WhyNow, 320},
		{"brief.learning_gain", brief.LearningGain, 260},
		{"brief.open_loop", brief.OpenLoop, 260},
		{"brief.next_action", brief.NextAction, 220},
	}
	for _, f := range required {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("%s is required", f.name)
		}
		if err := validateString(f.name, f.value, f.max); err != nil {
			return err
		}
		if containsInternalToolName(f.value) {
			return fmt.Errorf("%s must be learner-facing and must not mention internal tool names", f.name)
		}
	}
	optional := []struct {
		name  string
		value string
		max   int
	}{
		{"brief.domain_id", brief.DomainID, maxShortLabelLen},
		{"brief.domain_name", brief.DomainName, maxShortLabelLen},
		{"brief.concept", brief.Concept, maxShortLabelLen},
		{"brief.trigger", brief.Trigger, 120},
		{"brief.pedagogical_intent", brief.PedagogicalIntent, 240},
		{"brief.goal_link", brief.GoalLink, 280},
		{"brief.language", brief.Language, maxShortLabelLen},
		{"brief.tone", brief.Tone, 120},
	}
	for _, f := range optional {
		if err := validateString(f.name, f.value, f.max); err != nil {
			return err
		}
		if containsInternalToolName(f.value) {
			return fmt.Errorf("%s must be learner-facing and must not mention internal tool names", f.name)
		}
	}
	if len(brief.Evidence) > 3 {
		return fmt.Errorf("brief.evidence must contain at most 3 concise items")
	}
	for i, e := range brief.Evidence {
		name := fmt.Sprintf("brief.evidence[%d]", i)
		if err := validateString(name, e, 180); err != nil {
			return err
		}
		if containsInternalToolName(e) {
			return fmt.Errorf("%s must be learner-facing and must not mention internal tool names", name)
		}
	}
	if brief.EstimatedMinutes < 0 || brief.EstimatedMinutes > 90 {
		return fmt.Errorf("brief.estimated_minutes must be between 0 and 90")
	}
	return nil
}

func containsInternalToolName(s string) bool {
	s = strings.ToLower(s)
	for _, marker := range []string{
		"get_",
		"record_",
		"queue_webhook_message",
		"calibration_check",
		"feynman_challenge",
		"transfer_challenge",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
