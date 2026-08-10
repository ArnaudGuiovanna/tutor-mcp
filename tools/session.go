// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// learningSessionRequestError is safe to expose to callers. Persistence and
// transport failures deliberately use ordinary wrapped errors so callers can
// route them through safeErrorResult without leaking backend details.
type learningSessionRequestError struct {
	message string
}

func (e *learningSessionRequestError) Error() string { return e.message }

func learningSessionResolutionErrorResult(deps *Deps, err error) *mcp.CallToolResult {
	var requestErr *learningSessionRequestError
	if errors.As(err, &requestErr) {
		result, _ := errorResult(requestErr.Error())
		return result
	}
	result, _ := safeErrorResult(deps.Logger, "failed to resolve learning session", err)
	return result
}

type StartLearningSessionParams struct {
	IdempotentMutationParams
	SessionID string `json:"session_id,omitempty" jsonschema:"optional stable session identifier; omit to generate a secure session ID"`
	DomainID  string `json:"domain_id,omitempty" jsonschema:"optional active domain ID"`
}

func registerStartLearningSession(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "start_learning_session",
		Description: "Open or resume the learner's durable learning session. The operation is idempotent and returns the canonical session_id used by interactions, affect, assessment attempts, transfer evidence, intentions and summaries.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params StartLearningSessionParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "start_learning_session", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		for _, field := range []struct {
			name, value string
		}{
			{"session_id", params.SessionID},
			{"domain_id", params.DomainID},
		} {
			if err := validateString(field.name, field.value, maxShortLabelLen); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}
		if params.DomainID != "" {
			if _, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID); err != nil {
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
		}
		session, err := deps.Store.OpenLearningSession(ctx, learnerID, params.DomainID, params.SessionID, time.Now().UTC())
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to start learning session", err)
			return r, nil, nil
		}
		r, _ := jsonResult(map[string]any{
			"session":    session,
			"session_id": session.ID,
		})
		return r, nil, nil
	})
}

// resolveOpenLearningSession validates a caller-supplied session or opens the
// learner's canonical active one for compatibility with older clients that do
// not yet send session_id. A supplied ID is never silently substituted.
func resolveOpenLearningSession(ctx context.Context, deps *Deps, learnerID, domainID, sessionID string, now time.Time) (*models.LearningSession, error) {
	if sessionID != "" {
		session, err := deps.Store.GetLearningSession(ctx, learnerID, sessionID)
		if err != nil {
			if errors.Is(err, storeport.ErrNotFound) {
				return nil, &learningSessionRequestError{message: "learning session not found"}
			}
			return nil, fmt.Errorf("load learning session: %w", err)
		}
		if session.Status != models.LearningSessionStatusOpen {
			return nil, &learningSessionRequestError{message: "learning session is closed"}
		}
		// DomainID is the current active domain, not an immutable scope: one
		// explicit session may intentionally switch subjects while all events
		// remain correlated by the same session ID.
		resumed, err := deps.Store.OpenLearningSession(ctx, learnerID, domainID, sessionID, now)
		if err != nil {
			return nil, fmt.Errorf("resume learning session: %w", err)
		}
		return resumed, nil
	}
	session, err := deps.Store.OpenLearningSession(ctx, learnerID, domainID, "", now)
	if err != nil {
		return nil, fmt.Errorf("open learning session: %w", err)
	}
	return session, nil
}

type ListImplementationIntentionsParams struct {
	Since    string `json:"since,omitempty" jsonschema:"optional RFC3339 lower bound; defaults to 30 days ago"`
	Status   string `json:"status,omitempty" jsonschema:"optional status filter: pending, honored, missed, cancelled"`
	DomainID string `json:"domain_id,omitempty" jsonschema:"optional domain filter"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum rows, 1..100; defaults to 20"`
}

func registerListImplementationIntentions(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "list_implementation_intentions",
		Description: "List the learner's if-then practice commitments with their explicit pending, honored, missed or cancelled lifecycle state.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params ListImplementationIntentionsParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "list_implementation_intentions", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.Limit < 0 || params.Limit > 100 {
			r, _ := errorResult("limit must be 0 (default) or between 1 and 100")
			return r, nil, nil
		}
		if params.Status != "" && !isImplementationIntentionStatus(params.Status) {
			r, _ := errorResult("status must be one of: pending, honored, missed, cancelled")
			return r, nil, nil
		}
		if err := validateString("domain_id", params.DomainID, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		since := time.Now().UTC().Add(-30 * 24 * time.Hour)
		if params.Since != "" {
			parsed, err := time.Parse(time.RFC3339, params.Since)
			if err != nil {
				r, _ := errorResult("since must be RFC3339")
				return r, nil, nil
			}
			since = parsed.UTC()
		}
		limit := params.Limit
		if limit == 0 {
			limit = 20
		}
		// Fetch up to the public maximum before applying optional filters so
		// a filtered request does not accidentally inspect another learner.
		intentions, err := deps.Store.GetRecentImplementationIntentions(ctx, learnerID, since, 100)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to list implementation intentions", err)
			return r, nil, nil
		}
		filtered := make([]*models.ImplementationIntention, 0, limit)
		for _, intention := range intentions {
			if params.Status != "" && intention.Status != params.Status {
				continue
			}
			if params.DomainID != "" && intention.DomainID != params.DomainID {
				continue
			}
			filtered = append(filtered, intention)
			if len(filtered) == limit {
				break
			}
		}
		r, _ := jsonResult(map[string]any{"intentions": filtered})
		return r, nil, nil
	})
}

type UpdateImplementationIntentionParams struct {
	IdempotentMutationParams
	ID     int64  `json:"id" jsonschema:"implementation intention ID"`
	Status string `json:"status" jsonschema:"terminal status: honored, missed or cancelled"`
}

func registerUpdateImplementationIntention(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "update_implementation_intention",
		Description: "Resolve one pending practice commitment as honored, missed or cancelled. Ownership and one-way lifecycle transitions are enforced by storage.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params UpdateImplementationIntentionParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "update_implementation_intention", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.ID <= 0 {
			r, _ := errorResult("id must be positive")
			return r, nil, nil
		}
		if params.Status != models.IntentionStatusHonored &&
			params.Status != models.IntentionStatusMissed &&
			params.Status != models.IntentionStatusCancelled {
			r, _ := errorResult("status must be one of: honored, missed, cancelled")
			return r, nil, nil
		}
		intention, err := deps.Store.UpdateImplementationIntentionStatus(ctx, learnerID, params.ID, params.Status, time.Now().UTC())
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "implementation intention not found or already resolved", err)
			return r, nil, nil
		}
		r, _ := jsonResult(map[string]any{"intention": intention})
		return r, nil, nil
	})
}

func isImplementationIntentionStatus(status string) bool {
	switch status {
	case models.IntentionStatusPending,
		models.IntentionStatusHonored,
		models.IntentionStatusMissed,
		models.IntentionStatusCancelled:
		return true
	default:
		return false
	}
}
