// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
)

type ConsolidationRequest struct {
	Instruction string                    `json:"instruction"`
	Template    string                    `json:"template"`
	PendingJobs []ConsolidationJobPayload `json:"pending_jobs"`
}

type ConsolidationJobPayload struct {
	PeriodType                string            `json:"period_type"`
	PeriodKey                 string            `json:"period_key"`
	StartDate                 time.Time         `json:"start_date"`
	EndDate                   time.Time         `json:"end_date"`
	SessionsInPeriod          []string          `json:"sessions_in_period"`
	InterleavedReplaySessions []string          `json:"interleaved_replay_sessions"`
	CurrentMemorySnapshot     string            `json:"current_memory_snapshot"`
	CurrentPendingSnapshot    string            `json:"current_pending_snapshot"`
	TouchedConcepts           []ConceptSnapshot `json:"touched_concepts"`
}

type ConceptSnapshot struct {
	Slug    string `json:"slug"`
	Content string `json:"content"`
}

const consolidationRequestInstruction = `You have pending consolidation jobs. Before generating the activity, process each pending_job sequentially. For each job:

1. Read the sessions listed in sessions_in_period via read_raw_session(timestamp).
2. Combine them with current_memory_snapshot, current_pending_snapshot, touched_concepts, and the interleaved_replay_sessions (also fetchable via read_raw_session).
3. Apply the ConsolidationTemplate logic (provided in template) to produce:
   - the archive content
   - patches to MEMORY.md (additive, preserve stable sections)
   - patches to concepts/{c}.md for each touched concept
   - arbitration of MEMORY_pending.md (promote confirmed observations, keep isolated ones, remove contradicted ones)
4. Emit the following calls in order:
   - update_learner_memory(scope="archive", period_type=..., period_key=..., operation="replace_file", content=<archive markdown>)
   - update_learner_memory(scope="memory", operation="replace_section", section_key=..., content=...) for each section to patch in MEMORY.md
   - update_learner_memory(scope="concept", concept_slug=..., operation="replace_section", section_key="Current state", content=...) for each touched concept
   - update_learner_memory(scope="memory_pending", operation="replace_file", content=<new pending content after arbitration>)

After all consolidation jobs are processed, proceed to generate the activity described in the pedagogical_contract.

If you detect that a consolidation is impossible (corrupted sessions, ambiguous period, etc.), skip it and continue. The server will re-propose it at the next session.

Section headers in all written markdown MUST remain in English. Content under headers should be in the learner's primary language.`

func maybeBuildConsolidationRequest(ctx context.Context, deps *Deps, learnerID string, now time.Time) *ConsolidationRequest {
	if !memory.Enabled() {
		return nil
	}
	pending, err := deps.Store.GetPendingConsolidations(ctx, learnerID)
	if err != nil {
		deps.Logger.Warn("get_next_activity: pending consolidations lookup failed", "err", err, "learner", learnerID)
		return nil
	}
	if len(pending) == 0 {
		return nil
	}
	req, ids, err := buildConsolidationRequest(learnerID, pending)
	if err != nil {
		deps.Logger.Warn("get_next_activity: consolidation_request build failed", "err", err, "learner", learnerID)
		return nil
	}
	if len(req.PendingJobs) == 0 {
		return nil
	}
	if err := deps.Store.MarkConsolidationsDelivered(ctx, learnerID, ids, now); err != nil {
		deps.Logger.Warn("get_next_activity: mark consolidations delivered failed", "err", err, "learner", learnerID)
	}
	return req
}

func buildConsolidationRequest(learnerID string, pending []*models.PendingConsolidation) (*ConsolidationRequest, []int64, error) {
	req := &ConsolidationRequest{
		Instruction: consolidationRequestInstruction,
		Template:    memory.ConsolidationTemplate,
	}
	ids := make([]int64, 0, len(pending))
	for _, item := range pending {
		if item == nil {
			continue
		}
		start, end, err := consolidationPeriodBounds(item.PeriodType, item.PeriodKey)
		if err != nil {
			return nil, nil, err
		}
		sessions, err := memory.SessionsInRange(learnerID, start, end)
		if err != nil {
			return nil, nil, err
		}
		replay, err := memory.InterleavedReplaySessions(learnerID, start, 3)
		if err != nil {
			return nil, nil, err
		}
		memorySnapshot, err := memory.Read(learnerID, memory.ScopeMemory, "")
		if err != nil {
			return nil, nil, err
		}
		pendingSnapshot, err := memory.Read(learnerID, memory.ScopeMemoryPending, "")
		if err != nil {
			return nil, nil, err
		}
		concepts, err := consolidationConceptSnapshots(learnerID, memory.ConceptsFromSessions(sessions))
		if err != nil {
			return nil, nil, err
		}
		req.PendingJobs = append(req.PendingJobs, ConsolidationJobPayload{
			PeriodType:                item.PeriodType,
			PeriodKey:                 item.PeriodKey,
			StartDate:                 start,
			EndDate:                   end,
			SessionsInPeriod:          sessionTimestampStrings(sessions),
			InterleavedReplaySessions: sessionTimestampStrings(replay),
			CurrentMemorySnapshot:     memorySnapshot,
			CurrentPendingSnapshot:    pendingSnapshot,
			TouchedConcepts:           concepts,
		})
		ids = append(ids, item.ID)
	}
	return req, ids, nil
}

func consolidationPeriodBounds(periodType, periodKey string) (time.Time, time.Time, error) {
	periodType = strings.TrimSpace(periodType)
	periodKey = strings.TrimSpace(periodKey)
	switch periodType {
	case string(memory.PeriodMonthly):
		start, err := time.ParseInLocation("2006-01", periodKey, time.UTC)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid monthly period_key %q", periodKey)
		}
		return start, start.AddDate(0, 1, 0), nil
	case string(memory.PeriodQuarterly):
		parts := strings.Split(periodKey, "-Q")
		if len(parts) != 2 {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid quarterly period_key %q", periodKey)
		}
		year, err := strconv.Atoi(parts[0])
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid quarterly period_key %q", periodKey)
		}
		q, err := strconv.Atoi(parts[1])
		if err != nil || q < 1 || q > 4 {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid quarterly period_key %q", periodKey)
		}
		start := time.Date(year, time.Month((q-1)*3+1), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 3, 0), nil
	case string(memory.PeriodAnnual):
		year, err := strconv.Atoi(periodKey)
		if err != nil || year < 1 {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid annual period_key %q", periodKey)
		}
		start := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(1, 0, 0), nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("invalid period_type %q", periodType)
	}
}

func consolidationConceptSnapshots(learnerID string, slugs []string) ([]ConceptSnapshot, error) {
	out := make([]ConceptSnapshot, 0, len(slugs))
	for _, slug := range slugs {
		content, err := memory.Read(learnerID, memory.ScopeConcept, slug)
		if err != nil {
			return nil, err
		}
		out = append(out, ConceptSnapshot{Slug: slug, Content: content})
	}
	return out, nil
}

func sessionTimestampStrings(sessions []memory.SessionPayload) []string {
	out := make([]string, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, session.Timestamp.Format(time.RFC3339))
	}
	return out
}
