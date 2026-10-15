package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type routedToolStore struct {
	*db.Store
	reads, writes int
}

func (s *routedToolStore) WithTenantReadTx(ctx context.Context, scope models.TenantScope, fn func(context.Context, storeport.Store) error) error {
	s.reads++
	return s.Store.WithTenantReadTx(ctx, scope, func(ctx context.Context, scoped storeport.Store) error {
		// Exercise a real SQL write through the captured root Store. SQLite
		// rejects it without aborting the read transaction used by the handler.
		if err := s.Store.UpdateLearnerProfile(ctx, "L_owner", "{}", nil); err == nil {
			return context.Canceled // fail closed if routing ever loses the guard
		}
		return fn(ctx, scoped)
	})
}

func (s *routedToolStore) WithTenantTx(ctx context.Context, scope models.TenantScope, fn func(context.Context, storeport.Store) error) error {
	s.writes++
	return s.Store.WithTenantTx(ctx, scope, fn)
}

func TestTenantToolReadRoutesAreExplicitAndEnforced(t *testing.T) {
	// This reviewed list is deliberately independent of readOnlyTools, so a
	// new getter cannot inherit read-only routing through a name heuristic.
	readers := []string{
		"get_my_badges", "get_learner_badges", "list_trainer_cohorts", "get_cohort_insights", "get_cohort_statistics", "get_learner_progress",
		"list_available_formations", "get_my_formations", "get_learner_formation", "get_formation_version",
		"get_pending_alerts", "check_mastery", "get_learner_context", "get_availability_model", "get_olm_snapshot", "get_pedagogical_snapshots",
		"get_decision_replay_summary", "validate_domain_graph", "get_autonomy_metrics", "feynman_challenge", "transfer_challenge", "read_raw_session",
		"get_misconceptions", "list_implementation_intentions", "get_dashboard_state", "get_goal_relevance",
	}
	writers := []string{"get_curriculum_snapshot", "get_memory_state", "get_next_activity", "get_metacognitive_mirror"}
	if len(readOnlyTools) != len(readers) {
		t.Fatal("review the explicit read-only tool inventory")
	}
	t.Setenv("REGULATION_GOAL", "on")
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_state", true: "legacy_domain"}[legacy], func(t *testing.T) {
			persistence, deps := setupToolsTest(t)
			deps.Institution = true
			wrapped := &routedToolStore{Store: persistence}
			deps.Store = wrapped
			domainID := "missing"
			if legacy {
				domainID = makeOwnerDomain(t, persistence, "L_owner", "legacy-reader").ID
			}
			for _, names := range [][]string{readers, writers} {
				for _, name := range names {
					t.Run(name, func(t *testing.T) {
						beforeR, beforeW := wrapped.reads, wrapped.writes
						p := models.LegacyPrincipal("L_owner")
						grant := models.OAuthScopeLearner
						// Discovering institutional records is authorized at middleware;
						// absent records/legacy enrollment may still return not_found.
						if progressTool(name) || formationTool(name) {
							p.Roles = []string{models.RoleOwner, models.RoleLearner}
							if progressTool(name) {
								grant = models.OAuthScopeProgressRead
							} else {
								grant = models.OAuthScopeFormationRead + " " + models.OAuthScopeFormationWrite
							}
							p.Scopes = strings.Fields(grant)
						}
						args := map[string]any{}
						switch name {
						case "feynman_challenge", "transfer_challenge":
							args["concept"] = "a"
						case "read_raw_session":
							args["timestamp"] = "2026-10-14T00:00:00Z"
						case "get_curriculum_snapshot":
							args["domain_id"] = domainID
						case "get_my_badges", "get_learner_badges", "get_learner_progress":
							args["enrollment_id"] = "missing"
						case "get_learner_formation", "get_cohort_insights", "get_cohort_statistics":
							args["cohort_id"] = "missing"
						case "get_formation_version":
							args["version_id"] = "missing"
						}
						result := callTool(t, deps, func(server *mcp.Server, deps *Deps) {
							RegisterTools(server, deps)
							server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
								return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
									ctx, err := auth.WithPrincipal(ctx, p)
									if err != nil {
										return nil, err
									}
									return next(auth.WithOAuthScope(ctx, grant), method, req)
								}
							})
						}, "", name, args)
						wantRead := readOnlyTools[name]
						if (wrapped.reads-beforeR == 1) != wantRead || (wrapped.writes-beforeW == 1) == wantRead {
							t.Fatalf("route reads=%d writes=%d; expected read=%v; result=%s", wrapped.reads-beforeR, wrapped.writes-beforeW, wantRead, resultText(result))
						}
						if strings.Contains(resultText(result), "tenant-scoped persistence failed") {
							t.Fatalf("handler tried to write through reader: %s", resultText(result))
						}
					})
				}
			}
		})
	}
}
