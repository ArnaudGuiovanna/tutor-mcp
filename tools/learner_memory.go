// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"tutor-mcp/memory"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxMemoryContentLen = 64 * 1024

var allowedMemoryScopes = []string{
	string(memory.ScopeMemory),
	string(memory.ScopeMemoryPending),
	string(memory.ScopeSession),
	string(memory.ScopeConcept),
	string(memory.ScopeArchive),
}

var allowedMemoryOperations = []string{
	string(memory.OpAppend),
	string(memory.OpReplaceSection),
	string(memory.OpReplaceFile),
}

var (
	writeLearnerMemory        = memory.Write
	ensureLearnerMemoryDirs   = memory.EnsureLearnerDirs
	readLearnerMemory         = memory.Read
	listLearnerMemorySessions = memory.ListSessions
	listLearnerMemoryArchives = memory.ListArchives
	listLearnerMemoryConcepts = memory.ListConcepts
	pathForLearnerMemoryRead  = memory.PathForRead
	statLearnerMemoryPath     = os.Lstat
)

// memoryWriteDependencyError distinguishes an unavailable persistence
// dependency from caller-owned validation failures. Its cause is retained for
// structured logging but is never returned verbatim in a tool result.
type memoryWriteDependencyError struct {
	err error
}

func (e *memoryWriteDependencyError) Error() string { return "memory validation dependency failed" }
func (e *memoryWriteDependencyError) Unwrap() error { return e.err }

type UpdateLearnerMemoryParams struct {
	IdempotentMutationParams
	Scope       string `json:"scope" jsonschema:"memory scope: memory, memory_pending, session, concept, or archive"`
	Content     string `json:"content" jsonschema:"markdown content to write"`
	Operation   string `json:"operation,omitempty" jsonschema:"write operation: append, replace_section, or replace_file"`
	ConceptSlug string `json:"concept_slug,omitempty" jsonschema:"required when scope=concept; must match an active concept"`
	DomainID    string `json:"domain_id,omitempty" jsonschema:"domain owning a concept note or session; required for new domain-scoped concept memory"`
	Period      string `json:"period,omitempty" jsonschema:"archive period key alias, for example 2026-05 or 2026-Q2"`
	PeriodType  string `json:"period_type,omitempty" jsonschema:"required with period_key when scope=archive consolidation completion: monthly, quarterly, or annual"`
	PeriodKey   string `json:"period_key,omitempty" jsonschema:"required when scope=archive; for example 2026-05, 2026-Q2, or 2026"`
	Timestamp   string `json:"timestamp,omitempty" jsonschema:"required when scope=session, ISO 8601 timestamp"`
	SessionID   string `json:"session_id,omitempty" jsonschema:"durable learning session ID for a session summary; omit only when importing legacy summaries"`
	SectionKey  string `json:"section_key,omitempty" jsonschema:"required when operation=replace_section"`
}

func registerUpdateLearnerMemory(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "update_learner_memory",
		Description: "Write learner memory markdown files for session summaries, concept notes, pending observations, stable memory, or archives.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params UpdateLearnerMemoryParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "update_learner_memory", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if !memory.Enabled() {
			r, _ := jsonResult(map[string]any{"ok": false, "status": "not_enabled"})
			return r, nil, nil
		}
		if err := validateMemoryWriteParams(ctx, deps, learnerID, params); err != nil {
			var dependencyErr *memoryWriteDependencyError
			if errors.As(err, &dependencyErr) {
				r, _ := safeErrorResult(deps.Logger, "memory validation unavailable", err)
				return r, nil, nil
			}
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		ts, err := parseMemoryTimestamp(params.Timestamp)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		scope := memory.Scope(params.Scope)
		op := memory.Operation(params.Operation)
		if op == "" {
			op = defaultMemoryOperation(scope)
		}
		periodKey := archivePeriodKey(params)
		writeReq := memory.WriteRequest{
			LearnerID:   learnerID,
			DomainID:    params.DomainID,
			Scope:       scope,
			ConceptSlug: params.ConceptSlug,
			Period:      periodKey,
			Timestamp:   ts,
			Operation:   op,
			Content:     params.Content,
			SectionKey:  params.SectionKey,
		}
		var degradedComponents []string
		if err := writeLearnerMemory(writeReq); err != nil {
			if !memory.IsCommittedWriteError(err) {
				if errors.Is(err, memory.ErrQuotaExceeded) {
					r, _ := errorResult("memory quota exceeded")
					return r, nil, nil
				}
				r, _ := safeErrorResult(deps.Logger, "memory write failed", err)
				return r, nil, nil
			}
			// Rename already made the exact new content visible. Retrying an
			// append could duplicate it, so retain a successful idempotency result
			// while exposing the directory-sync durability degradation.
			degradedComponents = append(degradedComponents, "memory_directory_sync")
			deps.Logger.Warn("update_learner_memory: file committed with degraded directory sync", "err", err, "learner", learnerID, "scope", params.Scope)
		}
		consolidationCompletionRecorded := false
		if scope == memory.ScopeArchive && params.PeriodType != "" && periodKey != "" {
			if err := deps.Store.MarkConsolidationCompleted(ctx, learnerID, params.PeriodType, periodKey, time.Now().UTC()); err != nil {
				deps.Logger.Warn("update_learner_memory: mark consolidation completed failed", "err", err, "learner", learnerID, "period_type", params.PeriodType, "period_key", periodKey)
				degradedComponents = append(degradedComponents, "consolidation_completion_marker")
			} else {
				consolidationCompletionRecorded = true
			}
		}
		key := memoryReadKey(scope, params.ConceptSlug, periodKey, ts)
		payload := map[string]any{
			"ok":            true,
			"memory_key":    key,
			"bytes_written": len(params.Content),
			"session_id":    params.SessionID,
		}
		if params.DomainID != "" {
			payload["domain_id"] = params.DomainID
		}
		if scope == memory.ScopeArchive && params.PeriodType != "" && periodKey != "" {
			payload["archive_saved"] = true
			payload["consolidation_completion_recorded"] = consolidationCompletionRecorded
			if !consolidationCompletionRecorded {
				payload["warning"] = "archive saved, but consolidation completion could not be recorded; reconcile the consolidation marker without rewriting the archive"
			}
		}
		if len(degradedComponents) > 0 {
			payload["degraded_components"] = degradedComponents
		}
		r, _ := jsonResult(payload)
		return r, nil, nil
	})
}

type ReadRawSessionParams struct {
	Timestamp string `json:"timestamp" jsonschema:"ISO 8601 timestamp of an existing memory session"`
}

func registerReadRawSession(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "read_raw_session",
		Description: "Read one raw learner memory session by timestamp, including parsed YAML frontmatter and markdown body.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params ReadRawSessionParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "read_raw_session", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if !memory.Enabled() {
			r, _ := jsonResult(map[string]any{"ok": false, "status": "not_enabled", "session_payload": nil})
			return r, nil, nil
		}
		if err := validateString("timestamp", params.Timestamp, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		ts, err := parseMemoryTimestamp(params.Timestamp)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		raw, err := readLearnerMemory(learnerID, memory.ScopeSession, ts.Format(time.RFC3339))
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "memory session unavailable", err)
			return r, nil, nil
		}
		if strings.TrimSpace(raw) == "" {
			r, _ := jsonResult(map[string]any{"ok": true, "session_payload": nil})
			return r, nil, nil
		}
		payload, err := memory.ParseSessionPayload(ts, raw)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "memory session is invalid", err)
			return r, nil, nil
		}
		r, _ := jsonResult(map[string]any{"ok": true, "session_payload": payload})
		return r, nil, nil
	})
}

func registerGetMemoryState(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "get_memory_state",
		Description: "Inspect learner memory file counts, sizes, session bounds, consolidation lag, and recent narrative signal status.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params struct{}) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_memory_state", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if !memory.Enabled() {
			r, _ := jsonResult(map[string]any{"ok": false, "status": "not_enabled"})
			return r, nil, nil
		}
		if err := ensureLearnerMemoryDirs(learnerID); err != nil {
			r, _ := safeErrorResult(deps.Logger, "memory state unavailable", err)
			return r, nil, nil
		}
		sessions, err := listLearnerMemorySessions(learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "memory state unavailable", err)
			return r, nil, nil
		}
		archives, err := listLearnerMemoryArchives(learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "memory state unavailable", err)
			return r, nil, nil
		}
		concepts, err := listLearnerMemoryConcepts(learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "memory state unavailable", err)
			return r, nil, nil
		}
		pending, err := readLearnerMemory(learnerID, memory.ScopeMemoryPending, "")
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "memory state unavailable", err)
			return r, nil, nil
		}

		var degradedComponents []string
		var recentNarrativeSignal any
		ec, contextErr := memory.LoadContext(learnerID, "", nil, nil)
		if contextErr != nil {
			logMemoryStateDegradation(deps, "narrative_context", contextErr)
			degradedComponents = append(degradedComponents, "narrative_context")
		} else {
			recentNarrativeSignal = ec != nil && ec.HasRecentNarrativeSignal()
		}

		var memorySizeValue any
		memorySize, sizeErr := learnerMemorySize(learnerID, concepts, archives, sessions)
		if sizeErr != nil {
			logMemoryStateDegradation(deps, "memory_statistics", sizeErr)
			degradedComponents = append(degradedComponents, "memory_statistics")
			// HasRecentNarrativeSignal also depends on file metadata. Do not
			// report a potentially false negative when metadata is unavailable.
			if contextErr == nil {
				recentNarrativeSignal = nil
				degradedComponents = append(degradedComponents, "narrative_signal")
			}
		} else {
			memorySizeValue = memorySize
		}

		var consolidationLagValue any
		consolidationLag, lagErr := consolidationLagDays(learnerID, sessions, archives)
		if lagErr != nil {
			logMemoryStateDegradation(deps, "consolidation_lag", lagErr)
			degradedComponents = append(degradedComponents, "consolidation_lag")
		} else {
			consolidationLagValue = consolidationLag
		}

		var oldest, newest any
		if len(sessions) > 0 {
			newest = sessions[0]
			oldest = sessions[len(sessions)-1]
		}
		payload := map[string]any{
			"ok":                          true,
			"memory_size_bytes":           memorySizeValue,
			"pending_count":               countPendingMemoryItems(pending),
			"session_count":               len(sessions),
			"archive_count":               len(archives),
			"concept_count":               len(concepts),
			"oldest_session":              oldest,
			"newest_session":              newest,
			"consolidation_lag_days":      consolidationLagValue,
			"has_recent_narrative_signal": recentNarrativeSignal,
		}
		if len(degradedComponents) > 0 {
			payload["status"] = "degraded"
			payload["degraded_components"] = degradedComponents
		}
		r, _ := jsonResult(payload)
		return r, nil, nil
	})
}

func validateMemoryWriteParams(ctx context.Context, deps *Deps, learnerID string, params UpdateLearnerMemoryParams) error {
	for _, f := range []struct {
		name  string
		value string
		max   int
	}{
		{"scope", params.Scope, maxShortLabelLen},
		{"operation", params.Operation, maxShortLabelLen},
		{"concept_slug", params.ConceptSlug, maxShortLabelLen},
		{"domain_id", params.DomainID, maxShortLabelLen},
		{"period", params.Period, maxShortLabelLen},
		{"period_type", params.PeriodType, maxShortLabelLen},
		{"period_key", params.PeriodKey, maxShortLabelLen},
		{"timestamp", params.Timestamp, maxShortLabelLen},
		{"session_id", params.SessionID, maxShortLabelLen},
		{"section_key", params.SectionKey, maxShortLabelLen},
		{"content", params.Content, maxMemoryContentLen},
	} {
		if err := validateString(f.name, f.value, f.max); err != nil {
			return err
		}
	}
	if params.Scope == "" {
		return fmt.Errorf("scope is required")
	}
	if err := validateEnum("scope", params.Scope, allowedMemoryScopes); err != nil {
		return err
	}
	if params.Operation != "" {
		if err := validateEnum("operation", params.Operation, allowedMemoryOperations); err != nil {
			return err
		}
	}
	if strings.TrimSpace(params.Content) == "" {
		return fmt.Errorf("content is required")
	}
	scope := memory.Scope(params.Scope)
	op := memory.Operation(params.Operation)
	if op == "" {
		op = defaultMemoryOperation(scope)
	}
	if op == memory.OpReplaceSection && strings.TrimSpace(params.SectionKey) == "" {
		return fmt.Errorf("section_key is required for replace_section")
	}
	switch scope {
	case memory.ScopeConcept:
		if params.ConceptSlug == "" {
			return fmt.Errorf("concept_slug is required for concept scope")
		}
		if params.DomainID != "" {
			domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
			if err != nil {
				if !errors.Is(err, storeport.ErrNotFound) {
					return &memoryWriteDependencyError{err: fmt.Errorf("resolve concept domain: %w", err)}
				}
				return fmt.Errorf("domain not found")
			}
			if domain == nil {
				return fmt.Errorf("domain not found")
			}
			if err := validateConceptInDomain(domain, params.ConceptSlug); err != nil {
				return err
			}
		} else {
			active, err := deps.Store.ActiveDomainConceptSet(ctx, learnerID)
			if err != nil {
				return &memoryWriteDependencyError{err: fmt.Errorf("load active concept set: %w", err)}
			}
			if !active[params.ConceptSlug] {
				return fmt.Errorf("concept_slug must match an active concept")
			}
		}
	case memory.ScopeArchive:
		periodKey := archivePeriodKey(params)
		if periodKey == "" {
			return fmt.Errorf("period_key is required for archive scope")
		}
		if params.PeriodType != "" {
			if err := validateEnum("period_type", params.PeriodType, []string{string(memory.PeriodMonthly), string(memory.PeriodQuarterly), string(memory.PeriodAnnual)}); err != nil {
				return err
			}
		}
		if !validMemoryPeriod(periodKey) {
			return fmt.Errorf("period_key must look like YYYY-MM, YYYY-Qn, or YYYY")
		}
	case memory.ScopeSession:
		if params.Timestamp == "" {
			return fmt.Errorf("timestamp is required for session scope")
		}
		ts, err := parseMemoryTimestamp(params.Timestamp)
		if err != nil {
			return err
		}
		expectedDomainID := params.DomainID
		if params.SessionID != "" {
			session, err := deps.Store.GetLearningSession(ctx, learnerID, params.SessionID)
			if err != nil {
				if !errors.Is(err, storeport.ErrNotFound) {
					return &memoryWriteDependencyError{err: fmt.Errorf("load learning session: %w", err)}
				}
				return fmt.Errorf("learning session not found")
			}
			if expectedDomainID != "" && expectedDomainID != session.DomainID {
				return fmt.Errorf("domain_id does not match the durable session")
			}
			expectedDomainID = session.DomainID
		}
		if err := validateSessionMemoryContent(ts, params.SessionID, expectedDomainID, params.Content); err != nil {
			return err
		}
	}
	return nil
}

func defaultMemoryOperation(scope memory.Scope) memory.Operation {
	switch scope {
	case memory.ScopeMemoryPending:
		return memory.OpAppend
	case memory.ScopeSession, memory.ScopeArchive:
		return memory.OpReplaceFile
	default:
		return memory.OpReplaceSection
	}
}

func parseMemoryTimestamp(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp must be ISO 8601/RFC3339: %v", err)
	}
	return ts.UTC(), nil
}

func memoryReadKey(scope memory.Scope, conceptSlug, period string, ts time.Time) string {
	switch scope {
	case memory.ScopeConcept:
		return conceptSlug
	case memory.ScopeArchive:
		return period
	case memory.ScopeSession:
		return ts.Format(time.RFC3339)
	default:
		return ""
	}
}

func archivePeriodKey(params UpdateLearnerMemoryParams) string {
	if strings.TrimSpace(params.PeriodKey) != "" {
		return strings.TrimSpace(params.PeriodKey)
	}
	return strings.TrimSpace(params.Period)
}

func validMemoryPeriod(period string) bool {
	period = strings.TrimSpace(period)
	if len(period) == 4 {
		for _, r := range period {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	if len(period) == 7 && period[4] == '-' && period[5] == 'Q' {
		return period[6] >= '1' && period[6] <= '4'
	}
	if len(period) == 7 && period[4] == '-' {
		_, err := time.Parse("2006-01", period)
		return err == nil
	}
	return false
}

func validateSessionMemoryContent(fallback time.Time, expectedSessionID, expectedDomainID, content string) error {
	payload, err := memory.ParseSessionPayload(fallback, content)
	if err != nil {
		return err
	}
	required := []string{
		"timestamp",
		"duration_minutes",
		"affect_start",
		"affect_end",
		"energy_level",
		"concepts_touched",
		"session_type",
		"novelty_flag",
	}
	for _, key := range required {
		if _, ok := payload.Frontmatter[key]; !ok {
			return fmt.Errorf("session memory frontmatter missing %q", key)
		}
	}
	if expectedSessionID != "" {
		stored, ok := payload.Frontmatter["session_id"]
		if !ok {
			return fmt.Errorf("session memory frontmatter missing %q", "session_id")
		}
		if fmt.Sprint(stored) != expectedSessionID {
			return fmt.Errorf("session memory frontmatter session_id does not match the durable session")
		}
	}
	if expectedDomainID != "" {
		stored, ok := payload.Frontmatter["domain_id"]
		if !ok {
			return fmt.Errorf("session memory frontmatter missing %q", "domain_id")
		}
		if fmt.Sprint(stored) != expectedDomainID {
			return fmt.Errorf("session memory frontmatter domain_id does not match the durable session")
		}
	}
	return nil
}

func countPendingMemoryItems(content string) int {
	count := 0
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			count++
		}
	}
	return count
}

func learnerMemorySize(learnerID string, concepts, archives []string, sessions []time.Time) (int64, error) {
	var total int64
	for _, scope := range []memory.Scope{memory.ScopeMemory, memory.ScopeMemoryPending} {
		info, exists, err := learnerMemoryFileInfo(learnerID, scope, "")
		if err != nil {
			return 0, err
		}
		if exists {
			total += info.Size()
		}
	}
	for _, concept := range concepts {
		info, exists, err := learnerMemoryFileInfo(learnerID, memory.ScopeConcept, concept)
		if err != nil {
			return 0, err
		}
		if exists {
			total += info.Size()
		}
	}
	for _, archive := range archives {
		info, exists, err := learnerMemoryFileInfo(learnerID, memory.ScopeArchive, archive)
		if err != nil {
			return 0, err
		}
		if exists {
			total += info.Size()
		}
	}
	for _, ts := range sessions {
		info, exists, err := learnerMemoryFileInfo(learnerID, memory.ScopeSession, ts.Format(time.RFC3339))
		if err != nil {
			return 0, err
		}
		if exists {
			total += info.Size()
		}
	}
	return total, nil
}

func consolidationLagDays(learnerID string, sessions []time.Time, archives []string) (int, error) {
	if len(sessions) == 0 {
		return 0, nil
	}
	var newestArchive time.Time
	for _, archive := range archives {
		info, exists, err := learnerMemoryFileInfo(learnerID, memory.ScopeArchive, archive)
		if err != nil {
			return 0, err
		}
		if exists && info.ModTime().After(newestArchive) {
			newestArchive = info.ModTime()
		}
	}
	if newestArchive.IsZero() {
		return int(time.Since(sessions[len(sessions)-1]).Hours() / 24), nil
	}
	if sessions[0].Before(newestArchive) {
		return 0, nil
	}
	return int(time.Since(newestArchive).Hours() / 24), nil
}

func learnerMemoryFileInfo(learnerID string, scope memory.Scope, key string) (os.FileInfo, bool, error) {
	path, err := pathForLearnerMemoryRead(learnerID, scope, key)
	if err != nil {
		return nil, false, err
	}
	info, err := statLearnerMemoryPath(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("memory entry is not a regular file")
	}
	return info, true, nil
}

func logMemoryStateDegradation(deps *Deps, component string, err error) {
	if deps == nil || deps.Logger == nil {
		return
	}
	// File-system errors frequently embed absolute paths. Keep ordinary logs
	// and the MCP response limited to a stable component name and error class.
	deps.Logger.Warn("get_memory_state: optional component unavailable", "component", component, "error_type", fmt.Sprintf("%T", err))
}
