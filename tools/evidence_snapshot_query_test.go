// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestEvidenceHotPathsUseOneBatchPerDomain is a query-shape regression guard.
// Each batch Store method maps to one SQL query; the single-concept counters
// ensure a future enrichment does not quietly reintroduce N+1 reads.
func TestEvidenceHotPathsUseOneBatchPerDomain(t *testing.T) {
	tests := []struct {
		name     string
		register func(*mcp.Server, *Deps)
		tool     string
	}{
		{name: "dashboard", register: registerGetDashboardState, tool: "get_dashboard_state"},
		{name: "next_activity", register: registerGetNextActivity, tool: "get_next_activity"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, deps := setupToolsTest(t)
			concepts := make([]string, 12)
			for i := range concepts {
				concepts[i] = fmt.Sprintf("concept-%02d", i)
			}
			domain := seedDomain(t, store, "L_owner", concepts...)
			counting := &evidenceCountingStore{Store: store}
			deps.Store = counting

			res := callTool(t, deps, tc.register, "L_owner", tc.tool, map[string]any{
				"domain_id": domain.ID,
			})
			if res.IsError {
				t.Fatalf("%s failed: %q", tc.tool, resultText(res))
			}
			if got := counting.transferBatch.Load(); got != 1 {
				t.Fatalf("transfer batch calls=%d, want exactly 1 for 12 concepts", got)
			}
			if got := counting.assessmentBatch.Load(); got != 1 {
				t.Fatalf("assessment batch calls=%d, want exactly 1 for 12 concepts", got)
			}
			if got := counting.transferSingle.Load(); got != 0 {
				t.Fatalf("single-concept transfer calls=%d, N+1 path reintroduced", got)
			}
			if got := counting.assessmentSingle.Load(); got != 0 {
				t.Fatalf("single-concept assessment calls=%d, N+1 path reintroduced", got)
			}
		})
	}
}

type evidenceCountingStore struct {
	storeport.Store
	transferBatch    atomic.Int64
	assessmentBatch  atomic.Int64
	transferSingle   atomic.Int64
	assessmentSingle atomic.Int64
}

func (s *evidenceCountingStore) GetTransferScoresBatchInDomain(ctx context.Context, learnerID, domainID string, concepts []string) (map[string][]*models.TransferRecord, error) {
	s.transferBatch.Add(1)
	return s.Store.GetTransferScoresBatchInDomain(ctx, learnerID, domainID, concepts)
}

func (s *evidenceCountingStore) GetEvaluatedAssessmentAttemptsBatchInDomain(ctx context.Context, learnerID, domainID string, concepts []string, limitPerConcept int) (map[string][]*models.AssessmentAttempt, error) {
	s.assessmentBatch.Add(1)
	return s.Store.GetEvaluatedAssessmentAttemptsBatchInDomain(ctx, learnerID, domainID, concepts, limitPerConcept)
}

func (s *evidenceCountingStore) GetTransferScoresInDomain(ctx context.Context, learnerID, domainID, conceptID string) ([]*models.TransferRecord, error) {
	s.transferSingle.Add(1)
	return s.Store.GetTransferScoresInDomain(ctx, learnerID, domainID, conceptID)
}

func (s *evidenceCountingStore) GetEvaluatedAssessmentAttemptsInDomain(ctx context.Context, learnerID, domainID, conceptID string, limit int) ([]*models.AssessmentAttempt, error) {
	s.assessmentSingle.Add(1)
	return s.Store.GetEvaluatedAssessmentAttemptsInDomain(ctx, learnerID, domainID, conceptID, limit)
}
