// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import (
	"errors"
	"time"
)

const (
	MemoryMaintenanceMaxSessions    = 8
	MemoryMaintenanceMaxConcepts    = 16
	MemoryMaintenanceMaxObjectBytes = 32 * 1024
	MemoryMaintenanceMaxBatchBytes  = 128 * 1024
	MemoryMaintenanceLease          = 5 * time.Minute
)

var (
	ErrMemoryMaintenanceLease    = errors.New("memory maintenance lease expired or replaced")
	ErrMemoryMaintenanceConflict = errors.New("memory changed during maintenance; claim a fresh batch")
	ErrMemoryMaintenanceBudget   = errors.New("memory maintenance batch exceeds its safe bounds")
)

// MemoryMaintenanceItem is plaintext only in the authenticated tool response.
// The durable job stores metadata and an encrypted intermediate summary.
type MemoryMaintenanceItem struct {
	Scope    string `json:"scope"`
	Key      string `json:"key"`
	DomainID string `json:"domain_id,omitempty"`
	Content  string `json:"content"`
}

type MemoryMaintenanceBatch struct {
	Status         string                  `json:"status"`
	JobID          int64                   `json:"job_id,omitempty"`
	ClaimToken     string                  `json:"claim_token,omitempty"`
	LeaseExpiresAt *time.Time              `json:"lease_expires_at,omitempty"`
	PeriodType     string                  `json:"period_type,omitempty"`
	PeriodKey      string                  `json:"period_key,omitempty"`
	HasMore        bool                    `json:"has_more"`
	Sessions       []MemoryMaintenanceItem `json:"sessions,omitempty"`
	Snapshots      []MemoryMaintenanceItem `json:"snapshots,omitempty"`
	ArchiveSummary string                  `json:"archive_summary"`
}

type MemoryMaintenanceResult struct {
	OK        bool `json:"ok"`
	Completed bool `json:"completed"`
	Replayed  bool `json:"replayed"`
}

type PendingConsolidation struct {
	ID          int64      `json:"id"`
	LearnerID   string     `json:"learner_id"`
	PeriodType  string     `json:"period_type"`
	PeriodKey   string     `json:"period_key"`
	Status      string     `json:"status"`
	DetectedAt  time.Time  `json:"detected_at"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}
