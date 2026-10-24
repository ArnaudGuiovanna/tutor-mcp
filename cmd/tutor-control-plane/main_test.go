// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package main

import "testing"

func TestOptionsValidationRequiresAuditedBoundedActions(t *testing.T) {
	base := options{reason: "approved", requestID: "change-1"}
	tests := []struct {
		name string
		opts options
		ok   bool
	}{
		{name: "provision", opts: merge(base, options{action: "provision", slug: "acme", name: "Acme", region: "eu", planID: "plan"}), ok: true},
		{name: "plan", opts: merge(base, options{action: "plan-upsert", planID: "plan", name: "Plan", status: "active", entitlements: `{"mcp_calls_month":10}`}), ok: true},
		{name: "legacy invite owner retired", opts: merge(base, options{action: "invite-owner", tenantID: "tenant_x"})},
		{name: "native owner", opts: merge(base, options{action: "bootstrap-owner", tenantID: "tenant_x", ownerNpub: testNpub, recoveryNpub: testRecovery}), ok: true},
		{name: "same recovery", opts: merge(base, options{action: "bootstrap-owner", tenantID: "tenant_x", ownerNpub: testNpub, recoveryNpub: testNpub})},
		{name: "native learner", opts: merge(base, options{action: "add-key-member", tenantID: "tenant_x", memberNpub: testNpub, recoveryNpub: testRecovery, roles: "learner"}), ok: true},
		{name: "member cannot make owner", opts: merge(base, options{action: "add-key-member", tenantID: "tenant_x", memberNpub: testNpub, recoveryNpub: testRecovery, roles: "owner"})},
		{name: "staff without learner", opts: merge(base, options{action: "add-key-member", tenantID: "tenant_x", memberNpub: testNpub, recoveryNpub: testRecovery, roles: "trainer"}), ok: true},
		{name: "binding exact user", opts: merge(base, options{action: "bind-key-member", tenantID: "tenant_x", userID: "existing-user", ownerNpub: testNpub, recoveryNpub: testRecovery}), ok: true},
		{name: "binding requires user", opts: merge(base, options{action: "bind-key-member", tenantID: "tenant_x", ownerNpub: testNpub, recoveryNpub: testRecovery})},
		{name: "missing audit", opts: options{action: "status", tenantID: "tenant", status: "active"}},
		{name: "unknown", opts: merge(base, options{action: "delete-everything"})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.opts.validate() == nil; got != test.ok {
				t.Fatalf("valid=%v want=%v", got, test.ok)
			}
		})
	}
}

const testNpub = "npub180cvv07tjdrrgpa0j7j7tmnyl2yr6yr7l8j4s3evf6u64th6gkwsyjh6w6"
const testRecovery = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

func merge(base, overlay options) options {
	overlay.reason, overlay.requestID = base.reason, base.requestID
	return overlay
}
