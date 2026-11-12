// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
