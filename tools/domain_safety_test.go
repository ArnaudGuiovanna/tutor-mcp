// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"

	"tutor-mcp/models"
)

func TestMarkDomainHighStakesIsOwnedAndIdempotent(t *testing.T) {
	store, deps := setupToolsTest(t)
	domain, err := store.CreateDomain(context.Background(), "L_owner", "Clinical", "", models.KnowledgeSpace{
		Concepts: []string{"triage"}, Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"domain_id": domain.ID}
	for i := 0; i < 2; i++ {
		res := callTool(t, deps, registerMarkDomainHighStakes, "L_owner", "mark_domain_high_stakes", args)
		if res.IsError {
			t.Fatalf("idempotent mark %d failed: %s", i, resultText(res))
		}
		out := decodeResult(t, res)
		if out["high_stakes"] != true || out["demonstrated_claims_allowed"] != false {
			t.Fatalf("unexpected high-stakes policy: %v", out)
		}
	}
	foreign := callTool(t, deps, registerMarkDomainHighStakes, "L_attacker", "mark_domain_high_stakes", args)
	if !foreign.IsError {
		t.Fatal("foreign learner marked domain high stakes")
	}
	stored, err := store.GetDomainByID(context.Background(), domain.ID)
	if err != nil || !stored.HighStakes {
		t.Fatalf("stored classification=%+v err=%v", stored, err)
	}
}

func TestInitDomainCanApplyHighStakesPolicyAtomically(t *testing.T) {
	store, deps := setupToolsTest(t)
	res := callTool(t, deps, registerInitDomain, "L_owner", "init_domain", map[string]any{
		"name":          "regulated",
		"concepts":      []string{"decision"},
		"prerequisites": map[string][]string{},
		"high_stakes":   true,
	})
	if res.IsError {
		t.Fatalf("init high-stakes domain: %s", resultText(res))
	}
	out := decodeResult(t, res)
	if out["high_stakes"] != true || out["high_stakes_policy"] == nil {
		t.Fatalf("missing high-stakes response: %v", out)
	}
	domain, err := store.GetDomainByID(context.Background(), out["domain_id"].(string))
	if err != nil || !domain.HighStakes {
		t.Fatalf("domain classification=%+v err=%v", domain, err)
	}
}
