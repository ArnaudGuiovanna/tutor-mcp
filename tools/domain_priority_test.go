package tools

import (
	"context"
	"strings"
	"testing"
)

func TestSetDomainPriority_NoAuth(t *testing.T) {
	_, deps := setupToolsTest(t)
	res := callTool(t, deps, registerSetDomainPriority, "", "set_domain_priority", map[string]any{
		"domain_id": "d_x",
		"rank":      1,
	})
	if !res.IsError {
		t.Fatalf("expected auth error")
	}
	if !strings.Contains(resultText(res), "authentication") {
		t.Fatalf("expected authentication required, got %q", resultText(res))
	}
}

func TestSetDomainPriority_Validation(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "empty domain",
			args: map[string]any{"domain_id": "", "rank": 1},
			want: "domain_id is required",
		},
		{
			name: "zero rank",
			args: map[string]any{"domain_id": d.ID, "rank": 0},
			want: "rank must be >= 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, deps, registerSetDomainPriority, "L_owner", "set_domain_priority", tc.args)
			if !res.IsError {
				t.Fatalf("expected validation error")
			}
			if !strings.Contains(resultText(res), tc.want) {
				t.Fatalf("expected %q, got %q", tc.want, resultText(res))
			}
		})
	}
}

func TestSetDomainPriority_HappyPath(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerSetDomainPriority, "L_owner", "set_domain_priority", map[string]any{
		"domain_id": d.ID,
		"rank":      1,
	})
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(res))
	}
	out := decodeResult(t, res)
	if out["domain_id"] != d.ID {
		t.Fatalf("domain_id mismatch: %+v", out)
	}
	if out["priority_rank"] != float64(1) {
		t.Fatalf("priority_rank mismatch: %+v", out)
	}

	got, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDomainByID: %v", err)
	}
	if got.PriorityRank == nil || *got.PriorityRank != 1 {
		t.Fatalf("expected stored priority_rank=1, got %+v", got.PriorityRank)
	}
}

func TestSetDomainPriority_ForeignDomainRejected(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")

	res := callTool(t, deps, registerSetDomainPriority, "L_attacker", "set_domain_priority", map[string]any{
		"domain_id": d.ID,
		"rank":      1,
	})
	if !res.IsError {
		t.Fatalf("expected foreign domain to be rejected")
	}
	if !strings.Contains(resultText(res), "not found") {
		t.Fatalf("expected not found, got %q", resultText(res))
	}

	got, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDomainByID: %v", err)
	}
	if got.PriorityRank != nil {
		t.Fatalf("foreign update should not set priority_rank, got %d", *got.PriorityRank)
	}
}

func TestSetDomainPriority_ArchivedDomainRejected(t *testing.T) {
	store, deps := setupToolsTest(t)
	d := makeOwnerDomain(t, store, "L_owner", "math")
	if err := store.ArchiveDomain(context.Background(), d.ID, "L_owner"); err != nil {
		t.Fatalf("archive domain: %v", err)
	}

	res := callTool(t, deps, registerSetDomainPriority, "L_owner", "set_domain_priority", map[string]any{
		"domain_id": d.ID,
		"rank":      1,
	})
	if !res.IsError {
		t.Fatalf("expected archived domain to be rejected")
	}
	if !strings.Contains(resultText(res), "archived") {
		t.Fatalf("expected archived-domain error, got %q", resultText(res))
	}

	got, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDomainByID: %v", err)
	}
	if got.PriorityRank != nil {
		t.Fatalf("archived update should not set priority_rank, got %d", *got.PriorityRank)
	}
}
