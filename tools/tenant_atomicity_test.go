// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	"tutor-mcp/db"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Fail in Go after the first durable write. A PostgreSQL SQL error would
// independently abort the transaction and conceal a broken MCP rollback.
type failDomainInitializationStore struct {
	storeport.Store
	fail *atomic.Bool
}

func (s *failDomainInitializationStore) WithTx(ctx context.Context, fn func(storeport.Store) error) error {
	return s.Store.WithTx(ctx, func(tx storeport.Store) error {
		return fn(&failDomainInitializationStore{Store: tx, fail: s.fail})
	})
}

func (s *failDomainInitializationStore) InsertConceptStateIfNotExists(ctx context.Context, state *models.ConceptState) error {
	if state.Concept == "b" && s.fail.CompareAndSwap(true, false) {
		return errors.New("injected initialization failure")
	}
	return s.Store.InsertConceptStateIfNotExists(ctx, state)
}

func TestInitDomainApplicationFailureRollsBackThroughMiddleware(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			for _, keyed := range []bool{false, true} {
				name := "without_idempotency_key"
				if keyed {
					name = "with_idempotency_key"
				}
				t.Run(name, func(t *testing.T) {
					var s *db.Store
					var deps *Deps
					learnerID := "L_owner"
					if driver == "postgres" {
						dsn := os.Getenv("TUTOR_TEST_PG_DSN")
						if dsn == "" {
							t.Skip("set TUTOR_TEST_PG_DSN to test PostgreSQL rollback")
						}
						t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
						s = postgresToolsTestStore(t, dsn)
						learner, err := s.CreateLearner(context.Background(), "atomicity@example.invalid", "hash", "learn", "")
						if err != nil {
							t.Fatal(err)
						}
						learnerID = learner.ID
						deps = &Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
					} else {
						s, deps = setupToolsTest(t)
					}
					var fail atomic.Bool
					fail.Store(true)
					deps.Store = &failDomainInitializationStore{Store: s, fail: &fail}
					args := map[string]any{
						"name": "atomic-domain", "concepts": []string{"a", "b"},
						"prerequisites": map[string][]string{"b": {"a"}},
					}
					if keyed {
						args["idempotency_key"] = "atomic-domain-key"
					}
					countState := func(wantDomains, wantConcepts int) {
						t.Helper()
						var domains, concepts int
						if err := s.RawDB().QueryRow(`SELECT COUNT(*) FROM domains WHERE name = 'atomic-domain'`).Scan(&domains); err != nil {
							t.Fatal(err)
						}
						if err := s.RawDB().QueryRow(`SELECT COUNT(*) FROM concept_states WHERE domain_id IN (SELECT id FROM domains WHERE name = 'atomic-domain')`).Scan(&concepts); err != nil {
							t.Fatal(err)
						}
						if domains != wantDomains || concepts != wantConcepts {
							t.Fatalf("persisted domains/concepts = %d/%d, want %d/%d", domains, concepts, wantDomains, wantConcepts)
						}
					}
					failed := callTool(t, deps, RegisterTools, learnerID, "init_domain", args)
					if !failed.IsError || resultText(failed) != "failed to create domain" {
						t.Fatalf("original MCP error was lost: %+v", failed)
					}
					countState(0, 0)
					retried := callTool(t, deps, RegisterTools, learnerID, "init_domain", args)
					if retried.IsError {
						t.Fatalf("retry after rollback failed: %s", resultText(retried))
					}
					countState(1, 2)
					if keyed {
						replayed := callTool(t, deps, RegisterTools, learnerID, "init_domain", args)
						if replayed.IsError || decodeResult(t, replayed)["domain_id"] != decodeResult(t, retried)["domain_id"] {
							t.Fatalf("successful retry was not replayed: %s", resultText(replayed))
						}
						countState(1, 2)
					}
				})
			}
		})
	}
}
