// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "testing"

func TestPrincipalValidateRequiresUnambiguousTenantIdentity(t *testing.T) {
	valid := LegacyPrincipal("learner-1")
	for name, mutate := range map[string]func(*Principal){
		"user":          func(p *Principal) { p.UserID = "" },
		"tenant":        func(p *Principal) { p.TenantID = "" },
		"membership":    func(p *Principal) { p.MembershipID = "" },
		"role":          func(p *Principal) { p.Roles = nil },
		"scope":         func(p *Principal) { p.Scopes = nil },
		"token version": func(p *Principal) { p.TokenVersion = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("expected invalid principal")
			}
		})
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid legacy principal: %v", err)
	}
}

func TestPrincipalSessionBindingSeparatesMemberships(t *testing.T) {
	a := LegacyPrincipal("global-user")
	b := a
	b.TenantID = "tenant-b"
	b.MembershipID = "membership-b"
	if a.SessionBindingID() == b.SessionBindingID() {
		t.Fatal("same global user in two tenants must have distinct session bindings")
	}
}

func TestTenantScopeRejectsSilentlyNormalizedID(t *testing.T) {
	scope := LegacyPrincipal("learner-1").TenantScope()
	scope.TenantID = " tenant_legacy"
	if err := scope.Validate(); err == nil {
		t.Fatal("scope with ambiguous tenant ID must be rejected")
	}
}
