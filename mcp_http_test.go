package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
	"tutor-mcp/tools"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpHTTPTestIssuer = "https://mcp-http.test"

type mcpHTTPFixture struct {
	admin, runtime *db.Store
	postgres       bool
	role           string
}

// The migrator/seed store is deliberately separate from the PostgreSQL HTTP
// runtime. SQLite owns a real WAL file, one writer and four read connections.
func newMCPHTTPFixture(t *testing.T, backend string) *mcpHTTPFixture {
	t.Helper()
	f := &mcpHTTPFixture{postgres: backend == "postgres"}
	if !f.postgres {
		path := filepath.Join(t.TempDir(), "mcp-http.db")
		pool, err := db.OpenDB(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(pool); err != nil {
			_ = pool.Close()
			t.Fatal(err)
		}
		f.runtime, err = db.NewSQLiteStore(pool, path)
		if err != nil {
			_ = pool.Close()
			t.Fatal(err)
		}
		f.admin = f.runtime
		t.Cleanup(func() { _ = f.runtime.Close() })
		if pool.Stats().MaxOpenConnections != 1 {
			t.Fatal("SQLite fixture lost its single writer")
		}
	} else {
		configureMCPHTTPPostgres(t, f)
	}
	material := make([]byte, 32)
	if _, err := rand.Read(material); err != nil {
		t.Fatal(err)
	}
	keys, err := db.NewIntegrationSecretKeyring("http:"+base64.StdEncoding.EncodeToString(material), "http")
	if err != nil {
		t.Fatal(err)
	}
	f.admin.SetIntegrationSecretKeyring(keys)
	f.runtime.SetIntegrationSecretKeyring(keys)
	if err := f.admin.EnsureInstallation(t.Context(), "institution"); err != nil {
		t.Fatal(err)
	}
	return f
}

func configureMCPHTTPPostgres(t *testing.T, f *mcpHTTPFixture) {
	t.Helper()
	dsn := os.Getenv("TUTOR_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set TUTOR_TEST_PG_DSN for the PostgreSQL MCP HTTP identity regression")
	}
	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminPool := stdlib.OpenDB(*base)
	adminPool.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = adminPool.Close() })
	suffix := fmt.Sprint(time.Now().UnixNano())
	schema := "mcp_http_" + suffix
	f.role = "mcp_http_role_" + suffix
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	quotedRole := pgx.Identifier{f.role}.Sanitize()
	if _, err := adminPool.ExecContext(t.Context(), `CREATE SCHEMA `+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := adminPool.ExecContext(context.Background(), `DROP SCHEMA `+quotedSchema+` CASCADE`); err != nil {
			t.Errorf("drop MCP HTTP test schema: %v", err)
		}
	})
	seedConfig := base.Copy()
	seedConfig.RuntimeParams["search_path"] = schema
	seedPool := stdlib.OpenDB(*seedConfig)
	seedPool.SetMaxOpenConns(2)
	f.admin = db.NewStoreWithDialect(seedPool, db.DialectPostgres)
	t.Cleanup(func() { _ = f.admin.Close() })
	if err := db.MigratePostgres(t.Context(), seedPool); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.ExecContext(t.Context(), `CREATE ROLE `+quotedRole+` NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := adminPool.ExecContext(context.Background(), `DROP OWNED BY `+quotedRole+`; DROP ROLE `+quotedRole); err != nil {
			t.Errorf("drop MCP HTTP runtime role: %v", err)
		}
	})
	// Match the API role's DML privileges, with neither schema CREATE nor
	// ownership/BYPASSRLS. The administrative pool alone runs migrations.
	if _, err := adminPool.ExecContext(t.Context(), `GRANT USAGE ON SCHEMA `+quotedSchema+` TO `+quotedRole+`;
		GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA `+quotedSchema+` TO `+quotedRole+`;
		GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA `+quotedSchema+` TO `+quotedRole); err != nil {
		t.Fatal(err)
	}
	runtimeConfig := seedConfig.Copy()
	runtimePool := stdlib.OpenDB(*runtimeConfig, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE `+quotedRole)
		return err
	}))
	runtimePool.SetMaxOpenConns(10)
	runtimePool.SetMaxIdleConns(10)
	f.runtime = db.NewStoreWithDialect(runtimePool, db.DialectPostgres)
	t.Cleanup(func() { _ = f.runtime.Close() })
	if err := db.VerifyPostgresRuntimeRole(t.Context(), runtimePool); err != nil {
		t.Fatal(err)
	}
	// Pin all ten simultaneously to force ten physical connections. Release
	// them before returning, so the HTTP harness has its entire pool available.
	var conns []*sql.Conn
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()
	for range 10 {
		conn, err := runtimePool.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		var role string
		var superuser, bypass, ownsTable, creates bool
		if err := conn.QueryRowContext(t.Context(), `SELECT current_user,rolsuper,rolbypassrls,
			EXISTS(SELECT 1 FROM pg_class c WHERE c.relnamespace=current_schema()::regnamespace AND c.relowner=r.oid),
			has_schema_privilege(current_user,current_schema(),'CREATE') FROM pg_roles r WHERE rolname=current_user`).Scan(&role, &superuser, &bypass, &ownsTable, &creates); err != nil {
			t.Fatal(err)
		}
		if role != f.role || superuser || bypass || ownsTable || creates {
			t.Fatalf("runtime connection lost restricted role: role=%s superuser=%v bypass=%v owner=%v create=%v", role, superuser, bypass, ownsTable, creates)
		}
	}
}

func mcpHTTPMembershipScope(m *models.TenantMembership) models.TenantScope {
	return models.TenantScope{TenantID: m.TenantID, UserID: m.UserID, MembershipID: m.ID, LearnerID: m.LearnerID}
}

func (f *mcpHTTPFixture) seedTenant(t *testing.T, slug string, count int) []models.Principal {
	t.Helper()
	ctx := t.Context()
	operator := models.ControlPlanePrincipal{ActorID: "mcp-http-operator", Roles: []string{models.RolePlatformAdmin}, Reason: "MCP HTTP identity regression", RequestID: "mcp-http-" + slug}
	tenant, err := f.admin.ProvisionTenant(ctx, operator, slug, "Institution "+slug, "test", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	ownerToken, err := f.admin.CreateOwnerInvitation(ctx, operator, tenant.ID, "owner@"+slug+".test", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	membership, err := f.admin.AcceptTenantInvitationWithNewUser(ctx, models.InvitationCredential{Token: ownerToken}, "test-password-hash")
	if err != nil {
		t.Fatal(err)
	}
	ownerScope := mcpHTTPMembershipScope(membership)
	if _, err := f.admin.RecordMembershipMFAVerification(ctx, ownerScope, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	owner, err := f.admin.GetPrincipal(ctx, ownerScope, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.admin.SetInstitutionLearningPolicy(ctx, owner, true); err != nil {
		t.Fatal(err)
	}
	learners := make([]models.Principal, 0, count)
	for i := range count {
		_, token, err := f.admin.CreateTenantInvitation(ctx, owner, fmt.Sprintf("learner%d@%s.test", i, slug), []string{models.RoleLearner}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		member, err := f.admin.AcceptTenantInvitationWithNewUser(ctx, models.InvitationCredential{Token: token}, "test-password-hash")
		if err != nil {
			t.Fatal(err)
		}
		principal, err := f.admin.GetPrincipal(ctx, mcpHTTPMembershipScope(member), []string{models.OAuthScopeLearner})
		if err != nil {
			t.Fatal(err)
		}
		if principal.LearnerID == "" || principal.TenantID != tenant.ID {
			t.Fatal("institution invitation did not bind the learner membership")
		}
		learners = append(learners, principal)
	}
	return learners
}

func (f *mcpHTTPFixture) assertPostgresRLS(t *testing.T, actors []models.Principal) {
	t.Helper()
	if !f.postgres {
		return
	}
	pool := f.runtime.RawDB()
	var visible int
	if err := pool.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM learners`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("populated unscoped runtime exposed %d learners: %v", visible, err)
	}
	for _, actor := range actors {
		func() {
			tx, err := pool.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for setting, value := range map[string]string{"app.current_tenant": actor.TenantID, "app.current_user": actor.UserID, "app.current_membership": actor.MembershipID} {
				if _, err := tx.ExecContext(t.Context(), `SELECT set_config($1,$2,true)`, setting, value); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := tx.QueryContext(t.Context(), `SELECT tenant_id,id FROM learners`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			found := false
			for rows.Next() {
				var tenant, learner string
				if err := rows.Scan(&tenant, &learner); err != nil {
					t.Fatal(err)
				}
				if tenant != actor.TenantID {
					t.Fatal("unfiltered scoped SQL exposed a foreign tenant")
				}
				found = found || learner == actor.LearnerID
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("positive scoped RLS probe did not see its own learner")
			}
		}()
	}
}

func startMCPHTTPFixture(t *testing.T, f *mcpHTTPFixture) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Explicit fixture policy accommodates loopback initialization and calls;
	// the production limiter middleware and concurrency budgets remain active.
	ip := auth.NewRateLimiterWithNamespace("mcp_ip", 1000, 2000)
	learner := auth.NewRateLimiterWithNamespace("mcp_learner", 1000, 2000)
	t.Cleanup(ip.Stop)
	t.Cleanup(learner.Stop)
	concurrency, err := auth.NewPrincipalConcurrencyLimiter(128, 8)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", newMCPHTTPHandler(&tools.Deps{Store: f.runtime, Logger: logger, BaseURL: mcpHTTPTestIssuer, Institution: true},
		defaultMCPMaxRequestBodyBytes, defaultMCPToolCallTimeout, mcpHTTPGuards{IP: ip, Learner: learner, Concurrency: concurrency}))
	server := newIPv4HTTPTestServer(t, wrapHTTPHandler(logger, mcpHTTPTestIssuer, mux))
	t.Cleanup(server.Close)
	return server
}

func connectMCPHTTPActor(t *testing.T, ctx context.Context, server *httptest.Server, principal models.Principal) *mcp.ClientSession {
	t.Helper()
	token, err := auth.GenerateJWTForPrincipalAndScope(mcpHTTPTestIssuer, auth.MCPResource(mcpHTTPTestIssuer), "mcp-http-test-client", principal, models.OAuthScopeLearner)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	// A different immutable bearer, HTTP transport and MCP session per learner.
	httpClient := &http.Client{Transport: bearerRoundTripper{base: transport, token: token}, Timeout: 40 * time.Second}
	client := mcp.NewClient(&mcp.Implementation{Name: "identity-" + principal.LearnerID, Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callMCPHTTPTool(ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, map[string]any, error) {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, nil, fmt.Errorf("%s transport: %w", name, err)
	}
	if result == nil || result.IsError {
		return result, nil, fmt.Errorf("%s returned a tool error: %+v", name, result)
	}
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			var payload map[string]any
			if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
				return result, nil, fmt.Errorf("%s JSON: %w", name, err)
			}
			return result, payload, nil
		}
	}
	return result, nil, fmt.Errorf("%s returned no JSON text", name)
}

func checkMCPHTTPContext(payload map[string]any, learner, domain, objective string) error {
	if payload["learner_id"] != learner || payload["objective"] != objective {
		return fmt.Errorf("learner context identity mismatch: learner=%v objective=%v", payload["learner_id"], payload["objective"])
	}
	domains, ok := payload["domains"].([]any)
	if !ok || len(domains) != 1 {
		return fmt.Errorf("learner context domain count=%v", payload["domains"])
	}
	entry, ok := domains[0].(map[string]any)
	if !ok || entry["domain_id"] != domain {
		return fmt.Errorf("learner context foreign domain: %v", domains[0])
	}
	return nil
}

func configureMCPHTTPTestJWT(t *testing.T) {
	t.Helper()
	// Test processes configure signing once before any HTTP goroutines. Restore
	// the environment and reload any pre-existing configured keys on cleanup.
	previousKeys := os.Getenv("JWT_ED25519_KEYS") != "" || os.Getenv("JWT_SECRET") != ""
	t.Cleanup(func() {
		if previousKeys {
			if err := auth.LoadJWTSecret(); err != nil {
				t.Errorf("restore JWT configuration: %v", err)
			}
		}
	})
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JWT_ED25519_KEYS", "")
	t.Setenv("JWT_SECRET", base64.StdEncoding.EncodeToString(secret))
	if err := auth.LoadJWTSecret(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPHTTPConcurrentIdentityAndReplay(t *testing.T) {
	configureMCPHTTPTestJWT(t)
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			f := newMCPHTTPFixture(t, backend)
			t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
			t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
			// Exactly the runtime root passed to tools: A06 checks pool identity.
			memory.ConfigureNarrativeStore(f.runtime)
			t.Cleanup(func() { memory.ConfigureNarrativeStore(nil) })
			actors := append(f.seedTenant(t, "http-alpha", 2), f.seedTenant(t, "http-beta", 2)...)
			f.assertPostgresRLS(t, actors)
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			server := startMCPHTTPFixture(t, f)
			response, err := server.Client().Post(server.URL+"/mcp", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized || response.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("shared HTTP auth/security wrapper missing: status=%d headers=%v", response.StatusCode, response.Header)
			}
			sessions := make([]*mcp.ClientSession, len(actors))
			domains := make([]*models.Domain, len(actors))
			for i, actor := range actors {
				if err := f.runtime.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
					var err error
					domains[i], err = scoped.CreateDomain(txCtx, actor.LearnerID, "private-"+actor.LearnerID, "identity proof", models.KnowledgeSpace{Concepts: []string{"private-concept"}})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				sessions[i] = connectMCPHTTPActor(t, ctx, server, actor)
			}
			start := make(chan struct{})
			errors := make(chan error, len(actors))
			var group sync.WaitGroup
			for i, actor := range actors {
				group.Add(1)
				go func() {
					defer group.Done()
					<-start
					key := "identity-" + actor.LearnerID
					firstObjective := "first objective for " + actor.LearnerID
					finalObjective := "final objective for " + actor.LearnerID
					firstArgs := map[string]any{"objective": firstObjective, "idempotency_key": key + ":first"}
					first, payload, err := callMCPHTTPTool(ctx, sessions[i], "update_learner_profile", firstArgs)
					if err != nil || payload["updated"] != true {
						errors <- fmt.Errorf("first profile write %s: %v, %v", actor.LearnerID, payload, err)
						return
					}
					_, payload, err = callMCPHTTPTool(ctx, sessions[i], "update_learner_profile", map[string]any{"objective": finalObjective, "idempotency_key": key + ":final"})
					if err != nil || payload["updated"] != true {
						errors <- fmt.Errorf("second profile write %s: %v, %v", actor.LearnerID, payload, err)
						return
					}
					replay, _, err := callMCPHTTPTool(ctx, sessions[i], "update_learner_profile", firstArgs)
					if err != nil || !reflect.DeepEqual(first.Content, replay.Content) {
						errors <- fmt.Errorf("replay %s did not return its original response: %v", actor.LearnerID, err)
						return
					}
					_, payload, err = callMCPHTTPTool(ctx, sessions[i], "get_learner_context", map[string]any{"domain_id": domains[i].ID})
					if err == nil {
						err = checkMCPHTTPContext(payload, actor.LearnerID, domains[i].ID, finalObjective)
					}
					if err != nil {
						errors <- err
						return
					}
					// Independent SQL read after the HTTP transaction commits. If
					// replay executed again, it would restore the first objective.
					err = f.runtime.WithTenantReadTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
						learner, err := scoped.GetLearnerByID(txCtx, actor.LearnerID)
						if err != nil {
							return err
						}
						if learner.Objective != finalObjective {
							return fmt.Errorf("durable objective for %s=%q", actor.LearnerID, learner.Objective)
						}
						return nil
					})
					if err != nil {
						errors <- err
					}
				}()
			}
			close(start)
			group.Wait() // join before any client, HTTP server, or pool cleanup
			close(errors)
			for err := range errors {
				t.Error(err)
			}
			if t.Failed() {
				return
			}
			// A's bearer cannot read or modify B's domain. Accompany each
			// rejection with a positive owner read, including durable contents.
			for _, tool := range []string{"get_learner_context", "add_concepts"} {
				args := map[string]any{"domain_id": domains[2].ID}
				if tool == "add_concepts" {
					args["concepts"] = []string{"foreign-intrusion"}
					args["expected_version"] = domains[2].GraphVersion
					args["prerequisites"] = map[string][]string{}
					args["idempotency_key"] = "foreign-mutation"
				}
				result, err := sessions[0].CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
				if err != nil || result == nil || !result.IsError {
					t.Fatalf("foreign domain accepted by %s: result=%+v err=%v", tool, result, err)
				}
				encoded, err := json.Marshal(result.Content)
				if err != nil || !strings.Contains(string(encoded), "domain not found") {
					t.Fatalf("%s failed before the domain authorization boundary: %s, %v", tool, encoded, err)
				}
			}
			_, ownerPayload, err := callMCPHTTPTool(ctx, sessions[2], "get_learner_context", map[string]any{"domain_id": domains[2].ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := checkMCPHTTPContext(ownerPayload, actors[2].LearnerID, domains[2].ID, "final objective for "+actors[2].LearnerID); err != nil {
				t.Fatal(err)
			}
			if err := f.runtime.WithTenantReadTx(ctx, actors[2].TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
				domain, err := scoped.GetDomainByID(txCtx, domains[2].ID)
				if err != nil {
					return err
				}
				if domain.LearnerID != actors[2].LearnerID || !reflect.DeepEqual(domain.Graph, domains[2].Graph) {
					return fmt.Errorf("foreign mutation changed the owner's domain")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
