// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestCreateDomainEnforcesActiveDomainQuota(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	store.SetMaxActiveDomainsPerLearner(2)
	graph := models.KnowledgeSpace{Concepts: []string{"a"}, Prerequisites: map[string][]string{}}
	var created []*models.Domain
	for i := range 2 {
		domain, err := store.CreateDomain(ctx, "L1", fmt.Sprintf("domain-%d", i), "", graph)
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, domain)
	}
	_, err := store.CreateDomain(ctx, "L1", "over-quota", "", graph)
	var quota *DomainQuotaError
	if !errors.As(err, &quota) || quota.Limit != 2 || !strings.Contains(err.Error(), "archive or delete") {
		t.Fatalf("over-quota creation err=%v", err)
	}
	if err := store.ArchiveDomain(ctx, created[0].ID, "L1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDomain(ctx, "L1", "after-archive", "", graph); err != nil {
		t.Fatalf("archiving did not free a slot: %v", err)
	}
	if err := store.WithTx(ctx, func(tx storeport.Store) error {
		_, err := tx.CreateDomain(ctx, "L1", "in-tx", "", graph)
		if !errors.As(err, &quota) {
			return fmt.Errorf("transaction-scoped store ignored the quota: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store.SetMaxActiveDomainsPerLearner(0)
	if got := store.activeDomainLimit(); got != DefaultMaxActiveDomainsPerLearner {
		t.Fatalf("default quota=%d", got)
	}
}

// Concurrent creations must not overrun the quota. Under PostgreSQL READ
// COMMITTED each transaction would otherwise count the same free slot.
func TestCreateDomainQuotaIsAtomicUnderConcurrency(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	const limit, attempts = 3, 12
	store.SetMaxActiveDomainsPerLearner(limit)
	graph := models.KnowledgeSpace{Concepts: []string{"a"}, Prerequisites: map[string][]string{}}
	errs := make(chan error, attempts)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i := range attempts {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			_, err := store.CreateDomain(ctx, "L1", fmt.Sprintf("concurrent-%d", i), "", graph)
			errs <- err
		}()
	}
	start.Done()
	done.Wait()
	close(errs)
	created := 0
	for err := range errs {
		var quota *DomainQuotaError
		switch {
		case err == nil:
			created++
		case errors.As(err, &quota):
		default:
			t.Fatalf("unexpected creation error: %v", err)
		}
	}
	domains, err := store.GetDomainsByLearner(ctx, "L1", false)
	if err != nil {
		t.Fatal(err)
	}
	if created != limit || len(domains) != limit {
		t.Fatalf("created=%d stored=%d, want exactly %d", created, len(domains), limit)
	}
}

// Restoring an archived free domain consumes a slot like creating one: it
// must not push the learner past the quota, and restoring an already active
// domain stays a no-op success.
func TestUnarchiveDomainEnforcesActiveDomainQuota(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	store.SetMaxActiveDomainsPerLearner(2)
	graph := models.KnowledgeSpace{Concepts: []string{"a"}, Prerequisites: map[string][]string{}}
	first, err := store.CreateDomain(ctx, "L1", "first", "", graph)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveDomain(ctx, first.ID, "L1"); err != nil {
		t.Fatal(err)
	}
	var active []*models.Domain
	for i := range 2 {
		domain, err := store.CreateDomain(ctx, "L1", fmt.Sprintf("active-%d", i), "", graph)
		if err != nil {
			t.Fatal(err)
		}
		active = append(active, domain)
	}
	err = store.UnarchiveDomain(ctx, first.ID, "L1")
	var quota *DomainQuotaError
	if !errors.As(err, &quota) || quota.Limit != 2 {
		t.Fatalf("over-quota restoration err=%v", err)
	}
	domains, err := store.GetDomainsByLearner(ctx, "L1", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) != 2 {
		t.Fatalf("active domains after refused restoration = %d, want 2", len(domains))
	}
	if err := store.UnarchiveDomain(ctx, active[0].ID, "L1"); err != nil {
		t.Fatalf("restoring an already active domain at the quota: %v", err)
	}
	if err := store.UnarchiveDomain(ctx, "missing", "L1"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing domain err=%v", err)
	}
	if err := store.ArchiveDomain(ctx, active[1].ID, "L1"); err != nil {
		t.Fatal(err)
	}
	if err := store.UnarchiveDomain(ctx, first.ID, "L1"); err != nil {
		t.Fatalf("restoration with a free slot: %v", err)
	}
}

// Concurrent restorations and creations share the quota lock.
func TestUnarchiveDomainQuotaIsAtomicUnderConcurrency(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	const limit, archivedCount, creations = 2, 4, 4
	store.SetMaxActiveDomainsPerLearner(limit + archivedCount)
	graph := models.KnowledgeSpace{Concepts: []string{"a"}, Prerequisites: map[string][]string{}}
	var archived []string
	for i := range archivedCount {
		domain, err := store.CreateDomain(ctx, "L1", fmt.Sprintf("archived-%d", i), "", graph)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.ArchiveDomain(ctx, domain.ID, "L1"); err != nil {
			t.Fatal(err)
		}
		archived = append(archived, domain.ID)
	}
	store.SetMaxActiveDomainsPerLearner(limit)
	errs := make(chan error, archivedCount+creations)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range archivedCount + creations {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if i < archivedCount {
				errs <- store.UnarchiveDomain(ctx, archived[i], "L1")
				return
			}
			_, err := store.CreateDomain(ctx, "L1", fmt.Sprintf("concurrent-%d", i), "", graph)
			errs <- err
		}()
	}
	start.Done()
	done.Wait()
	close(errs)
	for err := range errs {
		var quota *DomainQuotaError
		if err != nil && !errors.As(err, &quota) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	domains, err := store.GetDomainsByLearner(ctx, "L1", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) != limit {
		t.Fatalf("active domains = %d, want exactly %d", len(domains), limit)
	}
}
