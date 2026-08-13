// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package models

import (
	"encoding/json"
	"strings"
	"time"
)

const RolePlatformAdmin = "platform_admin"

type WorkerPrincipal struct {
	ActorID string
}

func (p WorkerPrincipal) Validate() bool {
	return p.ActorID != "" && p.ActorID == strings.TrimSpace(p.ActorID) && len(p.ActorID) <= 128
}

type ControlPlanePrincipal struct {
	ActorID   string
	Roles     []string
	Reason    string
	RequestID string
}

type BillingWebhookCredential struct {
	Provider string
	Secret   string
}

// ServiceAccountCredential is the narrow global authentication capability
// used before a tenant can be resolved from a credential hash. It carries no
// caller-supplied tenant selector.
type ServiceAccountCredential struct {
	ClientID string
	Secret   string
}

// SupportAccessCredential is a one-time-delivered, short-lived break-glass
// capability. The tenant is resolved from its digest and is never supplied by
// the support caller.
type SupportAccessCredential struct {
	Token string
}

// OAuthCSRFCredential is an opaque one-time browser capability. Persistence
// stores only its digest and expiry.
type OAuthCSRFCredential struct {
	Token     string
	ExpiresAt time.Time
}

type SupportAccessGrant struct {
	ID        string
	TenantID  string
	ActorID   string
	Status    string
	ExpiresAt time.Time
	CreatedAt time.Time
}

func (p ControlPlanePrincipal) Validate() bool {
	if p.ActorID == "" || p.ActorID != strings.TrimSpace(p.ActorID) || len(p.ActorID) > 128 ||
		p.Reason == "" || p.Reason != strings.TrimSpace(p.Reason) || len(p.Reason) > 1024 ||
		p.RequestID == "" || p.RequestID != strings.TrimSpace(p.RequestID) || len(p.RequestID) > 255 {
		return false
	}
	for _, role := range p.Roles {
		if role == RolePlatformAdmin {
			return true
		}
	}
	return false
}

type OutboxEvent struct {
	ID             string
	TenantID       string
	AggregateType  string
	AggregateID    string
	EventType      string
	IdempotencyKey string
	PayloadJSON    string
	Status         string
	AttemptCount   int
	AvailableAt    time.Time
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	LastError      string
	CreatedAt      time.Time
	DeliveredAt    *time.Time
}

type AsyncJob struct {
	ID             string
	TenantID       string
	Kind           string
	IdempotencyKey string
	PayloadJSON    string
	Status         string
	AttemptCount   int
	MaxAttempts    int
	AvailableAt    time.Time
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	HeartbeatAt    *time.Time
	LastError      string
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

type UsageEvent struct {
	ID             int64
	TenantID       string
	EventKey       string
	Metric         string
	Quantity       int64
	SourceType     string
	SourceID       string
	CorrectionOf   string
	DimensionsJSON string
	OccurredAt     time.Time
	RecordedAt     time.Time
}

type Entitlement struct {
	TenantID      string
	Key           string
	HardLimit     int64
	UsedValue     int64
	ReservedValue int64
	PeriodStart   time.Time
	PeriodEnd     time.Time
	Version       int64
	UpdatedAt     time.Time
}

type Plan struct {
	ID               string
	Name             string
	Status           string
	EntitlementsJSON string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type TenantSubscription struct {
	TenantID           string
	PlanID             string
	Status             string
	Provider           string
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	GraceUntil         *time.Time
	Version            int64
	UpdatedAt          time.Time
}

type EntitlementReservation struct {
	ID             string
	TenantID       string
	EntitlementKey string
	Quantity       int64
	Status         string
	UsageEventKey  string
	SourceType     string
	SourceID       string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

type UsageCorrection struct {
	ID               int64
	TenantID         string
	CorrectionKey    string
	OriginalEventKey string
	Metric           string
	QuantityDelta    int64
	Reason           string
	OccurredAt       time.Time
	RecordedAt       time.Time
}

type TenantRetentionPolicy struct {
	TenantID      string
	DataClass     string
	RetentionDays int
	LegalHold     bool
	Version       int64
	UpdatedAt     time.Time
}

type TenantRestoreManifest struct {
	ID                  string
	TenantID            string
	BackupID            string
	Status              string
	TableChecksumsJSON  string
	ObjectChecksumsJSON string
	RequestedBy         string
	RequestedAt         time.Time
	VerifiedAt          *time.Time
}

type AuditEventFilter struct {
	ActionPrefix string
	TargetType   string
	OccurredFrom time.Time
	OccurredTo   time.Time
	AfterID      int64
	Limit        int
}

type AuditEventPage struct {
	Items       []AuditEvent
	NextAfterID int64
}

type TenantDSARRequest struct {
	ID          string
	TenantID    string
	LearnerID   string
	Kind        string
	Status      string
	RequestedBy string
	Reason      string
	ResultJSON  string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

type TenantDSARJob struct {
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
}

type TenantIntegration struct {
	ID             string
	TenantID       string
	Kind           string
	EndpointURL    string
	EventTypesJSON string
	SecretVersion  int64
	Status         string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type TenantIntegrationSecretVersion struct {
	TenantID      string
	IntegrationID string
	Version       int64
	KeyID         string
	ActiveFrom    time.Time
	ValidUntil    *time.Time
	CreatedBy     string
}

type SignedWebhook struct {
	IntegrationID string
	EventID       string
	EndpointURL   string
	Timestamp     int64
	Signature     string
	SecretVersion int64
	Payload       []byte
}

type TenantWebhookJob struct {
	IntegrationID string          `json:"integration_id"`
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	Payload       json.RawMessage `json:"payload"`
}

type IntegrationDelivery struct {
	ID            string
	TenantID      string
	IntegrationID string
	EventID       string
	Attempt       int
	Status        string
	ResponseCode  *int
	ResponseHash  string
	LastError     string
	CreatedAt     time.Time
	DeliveredAt   *time.Time
}
