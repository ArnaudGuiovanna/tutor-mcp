// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Before the fix a missing identifier answered "domain not found: <id>" while
// another learner's identifier answered "domain not found", which let a
// learner confirm that someone else's domain exists.
func TestDomainToolsAnswerForeignAndMissingIdentically(t *testing.T) {
	store, deps := setupToolsTest(t)
	foreign := makeOwnerDomain(t, store, "L_owner", "math")
	rank := 1
	tools := []struct {
		name     string
		register func(*mcp.Server, *Deps)
		extra    map[string]any
	}{
		{"archive_domain", registerArchiveDomain, nil},
		{"unarchive_domain", registerUnarchiveDomain, nil},
		{"delete_domain", registerDeleteDomain, map[string]any{"confirm": true}},
		{"set_domain_priority", registerSetDomainPriority, map[string]any{"rank": rank}},
	}
	for _, tool := range tools {
		t.Run(tool.name, func(t *testing.T) {
			call := func(domainID string) string {
				args := map[string]any{"domain_id": domainID}
				for key, value := range tool.extra {
					args[key] = value
				}
				res := callTool(t, deps, tool.register, "L_attacker", tool.name, args)
				if !res.IsError {
					t.Fatalf("%s(%s) succeeded for a non-owner", tool.name, domainID)
				}
				return resultText(res)
			}
			foreignText, missingText := call(foreign.ID), call("missing-domain-id")
			if foreignText != missingText {
				t.Fatalf("foreign=%q missing=%q, want identical responses", foreignText, missingText)
			}
		})
	}
}
