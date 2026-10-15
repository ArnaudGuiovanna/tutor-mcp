package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"testing"

	"tutor-mcp/db"
	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestSchedulerNarrativeScopeKeepsExplicitTenantAndStrictLegacyAdapter(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	persistence, raw := newOLMTestStore(t)
	raw.SetMaxOpenConns(1)
	keyring, err := db.NewIntegrationSecretKeyring("test:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "test")
	if err != nil {
		t.Fatal(err)
	}
	persistence.SetIntegrationSecretKeyring(keyring)
	learner, err := persistence.CreateLearner(t.Context(), "narrative@example.test", "hash", "objective", "")
	if err != nil {
		t.Fatal(err)
	}
	limits, err := memory.LimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := persistence.CompareAndSwapNarrative(t.Context(), memory.NarrativeKey{LearnerID: learner.ID, Scope: memory.ScopeSession, Key: "2026-10-15T00-00-00Z"}, 0, "---\ndomain_id: course\n---\nlocal history", "", "", limits); err != nil {
		t.Fatal(err)
	}
	memory.ConfigureNarrativeStore(persistence)
	t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	legacy := NewScheduler(persistence, logger)
	if view, err := legacy.loadOLMNarrativeContext(t.Context(), learner.ID, "course", ""); err != nil || len(view.RecentSessions) != 1 {
		t.Fatalf("legacy adapter=%+v,%v", view, err)
	}
	institution := NewTenantDistributedScheduler(persistence, logger, "narrative-test")
	child := *institution
	child.tenantStore = nil
	child.currentTenantScope = nil
	if _, err := child.loadOLMNarrativeContext(t.Context(), learner.ID, "course", ""); !errors.Is(err, memory.ErrNarrativeTenantScope) {
		t.Fatalf("institution child fabricated legacy scope: %v", err)
	}
	scope := models.LegacyPrincipal(learner.ID).TenantScope()
	// A real context takes precedence even on a legacy constructor. Worker
	// scopes differ from the learner principal but own this tenant's reads.
	scope.UserID = "worker_narrative"
	scope.MembershipID = "worker_process"
	scope.LearnerID = ""
	err = persistence.WithTenantTx(t.Context(), scope, func(ctx context.Context, scoped storeport.Store) error {
		for _, scheduler := range []*Scheduler{legacy, &child} {
			copy := *scheduler
			copy.store = scoped
			view, err := copy.loadOLMNarrativeContext(ctx, learner.ID, "course", "")
			if err != nil || len(view.RecentSessions) != 1 {
				return errors.New("scheduler replaced or lost explicit transaction scope")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
