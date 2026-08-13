// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	NotificationDeliveryStateReserved        = "reserved"
	NotificationDeliveryStateDelivered       = "delivered"
	NotificationDeliveryStateDeliveryUnknown = "delivery_unknown"
)

// IsIntrusiveWebhookKind classifies proactive suggestions. Unknown kinds fail
// closed as intrusive; daily recap is the sole informational exception.
func IsIntrusiveWebhookKind(kind string) bool {
	return strings.TrimSpace(kind) != WebhookKindDailyRecap
}

const WebhookBriefVersion = 1

// WebhookBrief is the structured content contract for learner-facing pushes.
// It keeps Discord copy short while preserving the pedagogical reason behind
// the nudge: why this matters now, what learning gain is expected, and what
// should happen when the learner opens the tutor again.
type WebhookBrief struct {
	Version           int      `json:"version,omitempty"`
	Kind              string   `json:"kind,omitempty"`
	DomainID          string   `json:"domain_id,omitempty"`
	DomainName        string   `json:"domain_name,omitempty"`
	Concept           string   `json:"concept,omitempty"`
	Trigger           string   `json:"trigger,omitempty"`
	PedagogicalIntent string   `json:"pedagogical_intent,omitempty"`
	LearningGain      string   `json:"learning_gain,omitempty"`
	WhyNow            string   `json:"why_now,omitempty"`
	Evidence          []string `json:"evidence,omitempty"`
	GoalLink          string   `json:"goal_link,omitempty"`
	OpenLoop          string   `json:"open_loop,omitempty"`
	NextAction        string   `json:"next_action,omitempty"`
	EstimatedMinutes  int      `json:"estimated_minutes,omitempty"`
	Language          string   `json:"language,omitempty"`
	Tone              string   `json:"tone,omitempty"`
}

func (b *WebhookBrief) Normalize(defaultKind string) {
	if b == nil {
		return
	}
	if b.Version == 0 {
		b.Version = WebhookBriefVersion
	}
	if b.Kind == "" {
		b.Kind = defaultKind
	}
	b.Kind = strings.TrimSpace(b.Kind)
	b.DomainID = strings.TrimSpace(b.DomainID)
	b.DomainName = strings.TrimSpace(b.DomainName)
	b.Concept = strings.TrimSpace(b.Concept)
	b.Trigger = strings.TrimSpace(b.Trigger)
	b.PedagogicalIntent = strings.TrimSpace(b.PedagogicalIntent)
	b.LearningGain = strings.TrimSpace(b.LearningGain)
	b.WhyNow = strings.TrimSpace(b.WhyNow)
	b.GoalLink = strings.TrimSpace(b.GoalLink)
	b.OpenLoop = strings.TrimSpace(b.OpenLoop)
	b.NextAction = strings.TrimSpace(b.NextAction)
	b.Language = strings.TrimSpace(b.Language)
	b.Tone = strings.TrimSpace(b.Tone)
	if b.EstimatedMinutes < 0 {
		b.EstimatedMinutes = 0
	}
	filtered := b.Evidence[:0]
	for _, e := range b.Evidence {
		if trimmed := strings.TrimSpace(e); trimmed != "" {
			filtered = append(filtered, trimmed)
		}
	}
	b.Evidence = filtered
}

func (b WebhookBrief) IsStructured() bool {
	return b.WhyNow != "" || b.LearningGain != "" || b.OpenLoop != "" || b.NextAction != ""
}

func EncodeWebhookBrief(b WebhookBrief) (string, error) {
	b.Normalize(b.Kind)
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func DecodeWebhookBrief(raw, defaultKind string) (*WebhookBrief, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "{") {
		return nil, false
	}
	var b WebhookBrief
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return nil, false
	}
	b.Normalize(defaultKind)
	if !b.IsStructured() {
		return nil, false
	}
	return &b, true
}

// WebhookPushLog records learner-facing pushes after Discord accepts them.
// It is intentionally about learning value, not transport: queue_id is zero
// for Go fallback pushes that were not authored through webhook_message_queue.
type WebhookPushLog struct {
	ID                int64      `json:"id"`
	LearnerID         string     `json:"learner_id"`
	QueueID           int64      `json:"queue_id,omitempty"`
	Kind              string     `json:"kind"`
	DomainID          string     `json:"domain_id,omitempty"`
	DomainName        string     `json:"domain_name,omitempty"`
	Concept           string     `json:"concept,omitempty"`
	Trigger           string     `json:"trigger,omitempty"`
	PedagogicalIntent string     `json:"pedagogical_intent,omitempty"`
	LearningGain      string     `json:"learning_gain,omitempty"`
	OpenLoop          string     `json:"open_loop,omitempty"`
	NextAction        string     `json:"next_action,omitempty"`
	PushedAt          time.Time  `json:"pushed_at"`
	OpenedSessionAt   *time.Time `json:"opened_session_at,omitempty"`
	ConceptAddressed  bool       `json:"concept_addressed"`
	CreatedAt         time.Time  `json:"created_at"`
}

// WebhookDeliveryTransition is an operator-facing, payload-free state change.
// EventID remains stable across every retry of one queue message; AttemptCount
// distinguishes individual delivery attempts without exposing the webhook URL
// or learner-authored content.
type WebhookDeliveryTransition struct {
	ID           int64     `json:"id"`
	QueueID      int64     `json:"queue_id"`
	EventID      string    `json:"event_id"`
	LearnerID    string    `json:"learner_id"`
	AttemptCount int       `json:"attempt_count"`
	FromStatus   string    `json:"from_status,omitempty"`
	ToStatus     string    `json:"to_status"`
	Reason       string    `json:"reason"`
	OccurredAt   time.Time `json:"occurred_at"`
}
