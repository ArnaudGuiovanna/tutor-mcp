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
		{name: "invite owner", opts: merge(base, options{action: "invite-owner", tenantID: "tenant_x", email: "head@school.test", baseURL: "https://tutor.example"}), ok: true},
		{name: "invite owner without url", opts: merge(base, options{action: "invite-owner", tenantID: "tenant_x", email: "head@school.test"})},
		{name: "invite owner relative url", opts: merge(base, options{action: "invite-owner", tenantID: "tenant_x", email: "head@school.test", baseURL: "tutor.example"})},
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

func merge(base, overlay options) options {
	overlay.reason, overlay.requestID = base.reason, base.requestID
	return overlay
}
