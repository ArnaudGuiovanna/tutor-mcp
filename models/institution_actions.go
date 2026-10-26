// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

type NativeActionProof struct {
	EventID     string
	PublicKey   string
	PayloadHash string
	TenantID    string
	SessionID   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// InstitutionRequest is a native account-management request. Its service
// validates the live principal and the exact institution on every operation.
type InstitutionRequest struct {
	Query        string   `json:"query,omitempty"`
	After        string   `json:"after,omitempty"`
	Limit        int      `json:"limit,omitempty"`
	Key          string   `json:"key,omitempty"`
	MemberID     string   `json:"member_id,omitempty"`
	Roles        []string `json:"roles,omitempty"`
	Npub         string   `json:"npub,omitempty"`
	RecoveryNpub string   `json:"recovery_npub,omitempty"`
	Name         string   `json:"name,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	Confirm      bool     `json:"confirm,omitempty"`
	RequestID    string   `json:"request_id,omitempty"`
}

type InstitutionMember struct {
	MembershipID string   `json:"membership_id"`
	UserID       string   `json:"user_id"`
	Name         string   `json:"name"`
	PublicKey    string   `json:"public_key"`
	Roles        []string `json:"roles"`
	Status       string   `json:"status"`
	IsSelf       bool     `json:"is_self"`
}

func InstitutionReadAction(action string) bool {
	return action == "overview" || action == "list_members" || action == "get_erasure_status"
}
func ValidInstitutionAction(action string) bool {
	if InstitutionReadAction(action) {
		return true
	}
	switch action {
	case "invite_member", "set_member_roles", "remove_member", "transfer_ownership", "request_erasure":
		return true
	}
	return false
}
