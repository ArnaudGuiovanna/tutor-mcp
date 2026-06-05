package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestMigrate_AddsNullableDomainPriorityRank(t *testing.T) {
	store := setupTestDB(t)
	rows, err := store.db.Query(`PRAGMA table_info(domains)`)
	if err != nil {
		t.Fatalf("table_info domains: %v", err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == "priority_rank" {
			found = true
			if typ != "INTEGER" {
				t.Fatalf("priority_rank type = %q, want INTEGER", typ)
			}
			if notNull != 0 {
				t.Fatalf("priority_rank should be nullable, notnull=%d", notNull)
			}
			if defaultValue.Valid {
				t.Fatalf("priority_rank should have no default, got %q", defaultValue.String)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	if !found {
		t.Fatalf("domains.priority_rank column not found")
	}
}

func createPriorityTestDomain(t *testing.T, store *Store, learnerID, name string, createdAt time.Time) *models.Domain {
	t.Helper()
	d, err := store.CreateDomain(context.Background(), learnerID, name, "", models.KnowledgeSpace{
		Concepts:      []string{name + "_concept"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create domain %q: %v", name, err)
	}
	if _, err := store.db.Exec(`UPDATE domains SET created_at = ? WHERE id = ?`, createdAt, d.ID); err != nil {
		t.Fatalf("set created_at for %q: %v", name, err)
	}
	return d
}

func TestGetDomainByLearner_PreservesCreatedAtFallbackWhenPriorityNull(t *testing.T) {
	store := setupTestDB(t)
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	old := createPriorityTestDomain(t, store, "L1", "old", base)
	newer := createPriorityTestDomain(t, store, "L1", "newer", base.Add(time.Hour))

	got, err := store.GetDomainByLearner(context.Background(), "L1")
	if err != nil {
		t.Fatalf("GetDomainByLearner: %v", err)
	}
	if got.ID != newer.ID {
		t.Fatalf("without ranks, expected newest domain %s, got %s (old %s)", newer.ID, got.ID, old.ID)
	}
	if got.PriorityRank != nil {
		t.Fatalf("new domains should default to NULL priority_rank, got %d", *got.PriorityRank)
	}
}

func TestGetDomainByLearner_ExplicitPriorityBeatsNewerUnrankedDomain(t *testing.T) {
	store := setupTestDB(t)
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	preferred := createPriorityTestDomain(t, store, "L1", "preferred", base)
	newer := createPriorityTestDomain(t, store, "L1", "newer", base.Add(time.Hour))

	if err := store.SetDomainPriority(context.Background(), preferred.ID, "L1", 1); err != nil {
		t.Fatalf("SetDomainPriority: %v", err)
	}

	got, err := store.GetDomainByLearner(context.Background(), "L1")
	if err != nil {
		t.Fatalf("GetDomainByLearner: %v", err)
	}
	if got.ID != preferred.ID {
		t.Fatalf("explicit priority should beat newer unranked domain: got %s, want %s (newer %s)", got.ID, preferred.ID, newer.ID)
	}
	if got.PriorityRank == nil || *got.PriorityRank != 1 {
		t.Fatalf("expected priority_rank=1, got %+v", got.PriorityRank)
	}
}

func TestGetDomainByLearner_LowerRankWinsThenCreatedAtTieBreak(t *testing.T) {
	store := setupTestDB(t)
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	lowPriority := createPriorityTestDomain(t, store, "L1", "low", base)
	highPriority := createPriorityTestDomain(t, store, "L1", "high", base.Add(time.Hour))
	unrankedNewest := createPriorityTestDomain(t, store, "L1", "unranked", base.Add(2*time.Hour))

	if err := store.SetDomainPriority(context.Background(), lowPriority.ID, "L1", 5); err != nil {
		t.Fatalf("set low priority: %v", err)
	}
	if err := store.SetDomainPriority(context.Background(), highPriority.ID, "L1", 2); err != nil {
		t.Fatalf("set high priority: %v", err)
	}

	got, err := store.GetDomainByLearner(context.Background(), "L1")
	if err != nil {
		t.Fatalf("GetDomainByLearner: %v", err)
	}
	if got.ID != highPriority.ID {
		t.Fatalf("lower explicit rank should win over higher rank and unranked newest: got %s, want %s", got.ID, highPriority.ID)
	}

	if err := store.SetDomainPriority(context.Background(), unrankedNewest.ID, "L1", 2); err != nil {
		t.Fatalf("set tied priority: %v", err)
	}
	got, err = store.GetDomainByLearner(context.Background(), "L1")
	if err != nil {
		t.Fatalf("GetDomainByLearner after tie: %v", err)
	}
	if got.ID != unrankedNewest.ID {
		t.Fatalf("same explicit rank should fall back to created_at DESC: got %s, want %s", got.ID, unrankedNewest.ID)
	}
}

func TestSetDomainPriority_RejectsArchivedDomain(t *testing.T) {
	store := setupTestDB(t)
	d := createPriorityTestDomain(t, store, "L1", "archived", time.Now().UTC())
	if err := store.ArchiveDomain(context.Background(), d.ID, "L1"); err != nil {
		t.Fatalf("archive domain: %v", err)
	}
	if err := store.SetDomainPriority(context.Background(), d.ID, "L1", 1); err == nil {
		t.Fatalf("expected archived domain priority update to fail")
	}

	got, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDomainByID: %v", err)
	}
	if got.PriorityRank != nil {
		t.Fatalf("archived domain priority should remain NULL, got %d", *got.PriorityRank)
	}
}
