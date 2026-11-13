// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"testing"
)

func TestLearnerModelView_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callLearnerView(t, deps, "", "model", nil)
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
}

func TestLearnerModelView_NoActiveDomain(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callLearnerView(t, deps, "L_owner", "model", nil)
	if res.IsError || decodeResult(t, res)["needs_domain_setup"] != true {
		t.Fatalf("expected needs_domain_setup for learner with no domain, got %q", resultText(res))
	}
}

func TestLearnerModelView_HappyPath(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callLearnerView(t, deps, "L_owner", "model", map[string]any{
		"domain_id": d.ID,
	})
	if res.IsError {
		t.Fatalf("got error: %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["domain_id"] != d.ID {
		t.Errorf("domain_id=%v, want %s", out["domain_id"], d.ID)
	}
	if _, ok := out["has_actionable"]; !ok {
		t.Errorf("has_actionable key missing in response: %+v", out)
	}
	if _, ok := out["solid"]; !ok {
		t.Errorf("solid key missing in response: %+v", out)
	}
}

func TestLearnerModelView_GlobalScope(t *testing.T) {
	store, deps := setupToolsTest(t)
	makeOwnerDomain(t, store, "L_owner", "math")
	makeOwnerDomain(t, store, "L_owner", "anglais")

	res := callLearnerView(t, deps, "L_owner", "model_all_domains", nil)
	if res.IsError {
		t.Fatalf("error: %q", resultText(res))
	}
	out := decodeResult(t, res)
	domains, ok := out["domains"].([]any)
	if !ok {
		t.Fatalf("structuredContent.domains missing, got %+v", out)
	}
	if len(domains) != 2 {
		t.Errorf("Domains=%d, want 2", len(domains))
	}
}
