// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// learnerBatchStore is implemented by the SQL store (db/learner_batch_reads.go):
// one statement answers for every domain of a multi-domain view. Other Store
// implementations fall back to one query per domain.
type learnerBatchStore interface {
	GetConceptStatesByDomains(ctx context.Context, scope models.TenantScope, learnerID string, domainIDs []string) (map[string][]*models.ConceptState, error)
	GetRecentInteractionsByDomains(ctx context.Context, scope models.TenantScope, learnerID string, domainIDs []string, limit int) (map[string][]*models.Interaction, error)
	GetInteractionsSinceByDomains(ctx context.Context, scope models.TenantScope, learnerID string, domainIDs []string, since time.Time) (map[string][]*models.Interaction, error)
	InsertDomainConceptStates(ctx context.Context, scope models.TenantScope, learnerID, domainID string, concepts []string) error
}

// learnerTenantScope is the tenant scope of the authenticated call, or the
// legacy single-tenant scope of learnerID for embedders without a principal.
func learnerTenantScope(ctx context.Context, learnerID string) models.TenantScope {
	if principal, ok := auth.GetPrincipal(ctx); ok && principal.LearnerID == learnerID {
		return principal.TenantScope()
	}
	return models.LegacyPrincipal(learnerID).TenantScope()
}

func domainIDsOf(domains []*models.Domain) []string {
	ids := make([]string, 0, len(domains))
	for _, domain := range domains {
		ids = append(ids, domain.ID)
	}
	return ids
}

func conceptStatesByDomains(ctx context.Context, store storeport.ConceptStateStore, learnerID string, domains []*models.Domain) (map[string][]*models.ConceptState, error) {
	if batch, ok := store.(learnerBatchStore); ok {
		return batch.GetConceptStatesByDomains(ctx, learnerTenantScope(ctx, learnerID), learnerID, domainIDsOf(domains))
	}
	out := make(map[string][]*models.ConceptState, len(domains))
	for _, domain := range domains {
		states, err := store.GetConceptStatesByDomain(ctx, learnerID, domain.ID)
		if err != nil {
			return nil, err
		}
		out[domain.ID] = states
	}
	return out, nil
}

func recentInteractionsByDomains(ctx context.Context, store storeport.InteractionStore, learnerID string, domains []*models.Domain, limit int) (map[string][]*models.Interaction, error) {
	if batch, ok := store.(learnerBatchStore); ok {
		return batch.GetRecentInteractionsByDomains(ctx, learnerTenantScope(ctx, learnerID), learnerID, domainIDsOf(domains), limit)
	}
	out := make(map[string][]*models.Interaction, len(domains))
	for _, domain := range domains {
		interactions, err := store.GetRecentInteractionsByDomain(ctx, learnerID, domain.ID, limit)
		if err != nil {
			return nil, err
		}
		out[domain.ID] = interactions
	}
	return out, nil
}

func interactionsSinceByDomains(ctx context.Context, store storeport.InteractionStore, learnerID string, domains []*models.Domain, since time.Time) (map[string][]*models.Interaction, error) {
	if batch, ok := store.(learnerBatchStore); ok {
		return batch.GetInteractionsSinceByDomains(ctx, learnerTenantScope(ctx, learnerID), learnerID, domainIDsOf(domains), since)
	}
	out := make(map[string][]*models.Interaction, len(domains))
	for _, domain := range domains {
		interactions, err := store.GetInteractionsSinceInDomain(ctx, learnerID, domain.ID, since)
		if err != nil {
			return nil, err
		}
		out[domain.ID] = interactions
	}
	return out, nil
}

// initializeDomainConceptStates creates the fresh state of every concept of a
// new domain inside the caller's transaction store.
func initializeDomainConceptStates(ctx context.Context, tx storeport.ConceptStateStore, learnerID, domainID string, concepts []string) error {
	if batch, ok := tx.(learnerBatchStore); ok {
		return batch.InsertDomainConceptStates(ctx, learnerTenantScope(ctx, learnerID), learnerID, domainID, concepts)
	}
	for _, concept := range concepts {
		if err := tx.InsertConceptStateIfNotExists(ctx, models.NewConceptStateInDomain(learnerID, domainID, concept)); err != nil {
			return err
		}
	}
	return nil
}
