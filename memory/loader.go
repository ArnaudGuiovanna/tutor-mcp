// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"tutor-mcp/models"

	"gopkg.in/yaml.v3"
)

const contextBudgetBytes = 40 * 1024

type EpisodicContext struct {
	LearnerMemory      string             `json:"learner_memory"`
	PendingMemory      string             `json:"pending_memory,omitempty"`
	ConceptNotes       string             `json:"concept_notes,omitempty"`
	RecentSessions     []SessionPayload   `json:"recent_sessions"`
	RecentArchives     []ArchivePayload   `json:"recent_archives,omitempty"`
	OLMInconsistencies []OLMInconsistency `json:"olm_inconsistencies,omitempty"`
	LoadedAt           time.Time          `json:"loaded_at"`

	memoryModifiedAt  time.Time
	pendingModifiedAt time.Time
}

type SessionPayload struct {
	Timestamp   time.Time      `json:"timestamp"`
	Frontmatter map[string]any `json:"frontmatter"`
	Body        string         `json:"body"`
}

type ArchivePayload struct {
	Period string `json:"period"`
	Body   string `json:"body"`
}

type OLMInconsistency struct {
	Type        string `json:"type"`
	Concept     string `json:"concept"`
	Description string `json:"description"`
}

// OLMView is the memory package's narrow read-only view of the Open Learner
// Model. It deliberately avoids importing engine to keep memory usable from
// engine/scheduler without creating an import cycle.
type OLMView struct {
	FocusConcept  string
	ConceptBucket map[string]string
}

func LoadContext(learnerID, focusConcept string, olmSnapshot *OLMView, alerts []models.Alert) (*EpisodicContext, error) {
	if !Enabled() {
		return &EpisodicContext{LoadedAt: time.Now().UTC()}, nil
	}
	if err := EnsureLearnerDirs(learnerID); err != nil {
		return nil, err
	}
	ec := &EpisodicContext{
		RecentSessions: []SessionPayload{},
		LoadedAt:       time.Now().UTC(),
	}

	var err error
	ec.LearnerMemory, err = Read(learnerID, ScopeMemory, "")
	if err != nil {
		return nil, err
	}
	ec.memoryModifiedAt = modifiedAt(learnerID, ScopeMemory, "")

	ec.PendingMemory, err = Read(learnerID, ScopeMemoryPending, "")
	if err != nil {
		return nil, err
	}
	ec.pendingModifiedAt = modifiedAt(learnerID, ScopeMemoryPending, "")

	if focusConcept != "" {
		ec.ConceptNotes, err = Read(learnerID, ScopeConcept, focusConcept)
		if err != nil {
			return nil, err
		}
	}

	sessions, err := ListSessions(learnerID)
	if err != nil {
		return nil, err
	}
	if len(sessions) > 3 {
		sessions = sessions[:3]
	}
	for _, ts := range sessions {
		raw, err := Read(learnerID, ScopeSession, ts.Format(time.RFC3339))
		if err != nil {
			return nil, err
		}
		payload, err := ParseSessionPayload(ts, raw)
		if err != nil {
			return nil, err
		}
		ec.RecentSessions = append(ec.RecentSessions, payload)
	}

	archives, err := ListArchives(learnerID)
	if err != nil {
		return nil, err
	}
	if len(archives) > 2 {
		archives = archives[:2]
	}
	for _, period := range archives {
		body, err := Read(learnerID, ScopeArchive, period)
		if err != nil {
			return nil, err
		}
		ec.RecentArchives = append(ec.RecentArchives, ArchivePayload{Period: period, Body: body})
	}

	ec.OLMInconsistencies = DetectOLMInconsistencies(olmSnapshot, alerts)
	enforceContextBudget(ec, contextBudgetBytes)
	return ec, nil
}

func ParseSessionPayload(fallbackTimestamp time.Time, raw string) (SessionPayload, error) {
	payload := SessionPayload{
		Timestamp:   fallbackTimestamp.UTC(),
		Frontmatter: map[string]any{},
	}
	trimmed := strings.TrimLeft(raw, "\ufeff\r\n\t ")
	if !strings.HasPrefix(trimmed, "---\n") && !strings.HasPrefix(trimmed, "---\r\n") {
		payload.Body = strings.TrimSpace(raw)
		return payload, nil
	}

	body := strings.TrimPrefix(trimmed, "---\r\n")
	body = strings.TrimPrefix(body, "---\n")
	endUnix := strings.Index(body, "\n---")
	endWin := strings.Index(body, "\r\n---")
	end := endUnix
	if end == -1 || (endWin != -1 && endWin < end) {
		end = endWin
	}
	if end == -1 {
		payload.Body = strings.TrimSpace(raw)
		return payload, nil
	}
	fmRaw := body[:end]
	rest := body[end:]
	rest = strings.TrimPrefix(rest, "\r\n---")
	rest = strings.TrimPrefix(rest, "\n---")
	rest = strings.TrimPrefix(rest, "\r\n")
	rest = strings.TrimPrefix(rest, "\n")

	var fm map[string]any
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return payload, fmt.Errorf("memory: parse session frontmatter: %w", err)
	}
	payload.Frontmatter = fm
	if tsRaw, ok := fm["timestamp"].(string); ok && tsRaw != "" {
		if ts, err := time.Parse(time.RFC3339, tsRaw); err == nil {
			payload.Timestamp = ts.UTC()
		}
	}
	payload.Body = strings.TrimSpace(rest)
	return payload, nil
}

func (ec *EpisodicContext) HasRecentNarrativeSignal() bool {
	if ec == nil {
		return false
	}
	now := time.Now().UTC()
	if !ec.pendingModifiedAt.IsZero() && now.Sub(ec.pendingModifiedAt) <= 7*24*time.Hour {
		return true
	}
	for _, session := range ec.RecentSessions {
		if truthy(session.Frontmatter["novelty_flag"]) {
			return true
		}
	}
	if !ec.memoryModifiedAt.IsZero() && now.Sub(ec.memoryModifiedAt) <= 3*24*time.Hour {
		return true
	}
	return false
}

func DetectOLMInconsistencies(view *OLMView, alerts []models.Alert) []OLMInconsistency {
	if view == nil {
		view = &OLMView{}
	}
	bucket := func(concept string) string {
		if view.ConceptBucket == nil {
			return ""
		}
		return view.ConceptBucket[concept]
	}
	focus := view.FocusConcept
	var out []OLMInconsistency
	var critical []models.Alert
	for _, alert := range alerts {
		if alert.Type == models.AlertForgetting && alert.Urgency == models.UrgencyCritical {
			critical = append(critical, alert)
			if bucket(alert.Concept) == "solid" {
				out = append(out, OLMInconsistency{
					Type:    "solid_but_forgetting",
					Concept: alert.Concept,
					Description: fmt.Sprintf(
						"The concept %s is counted as 'solid' in OLM counters, but it is in critical forgetting (retention=%.2f). The OLM counter is misleading on this point.",
						alert.Concept,
						alert.Retention,
					),
				})
			}
		}
		if alert.Concept != "" && bucket(alert.Concept) == "fragile" && focus != "" && alert.Concept != focus {
			out = append(out, OLMInconsistency{
				Type:        "fragile_but_in_focus",
				Concept:     alert.Concept,
				Description: fmt.Sprintf("The concept %s is fragile according to the OLM but is not the current focus (%s). Check whether the focus should be negotiated.", alert.Concept, focus),
			})
		}
	}
	if len(critical) > 0 && focus == "" {
		out = append(out, OLMInconsistency{
			Type:        "no_focus_despite_critical",
			Concept:     critical[0].Concept,
			Description: fmt.Sprintf("A FORGETTING-Critical alert exists on %s, but the OLM exposes no focus.", critical[0].Concept),
		})
	}
	if len(critical) > 1 && focus != "" {
		sort.Slice(critical, func(i, j int) bool {
			return critical[i].Retention < critical[j].Retention
		})
		if critical[0].Concept != focus {
			out = append(out, OLMInconsistency{
				Type:    "multiple_critical_unsorted",
				Concept: critical[0].Concept,
				Description: fmt.Sprintf(
					"Multiple concepts are in critical forgetting. The OLM chose '%s' as focus, but '%s' has the lowest retention (%.2f). Consider prioritizing the latter.",
					focus,
					critical[0].Concept,
					critical[0].Retention,
				),
			})
		}
	}
	return out
}

func enforceContextBudget(ec *EpisodicContext, budget int) {
	if ec == nil || budget <= 0 {
		return
	}
	size := contextSize(ec)
	// Drop the oldest whole sessions first, but keep a floor of one: the most
	// recent session always survives so the learner never loses their latest
	// context outright. Once that survivor is the only one left we stop dropping
	// and fall through to the graduated stages below (archives, then trimming
	// the survivor's frontmatter, then its body). Without this floor the loop
	// would empty RecentSessions entirely and the frontmatter/body stages could
	// never fire.
	for size > budget && len(ec.RecentSessions) > 1 {
		ec.RecentSessions = ec.RecentSessions[:len(ec.RecentSessions)-1]
		size = contextSize(ec)
	}
	for size > budget && len(ec.RecentArchives) > 0 {
		ec.RecentArchives = ec.RecentArchives[:len(ec.RecentArchives)-1]
		size = contextSize(ec)
	}
	for size > budget {
		cleared := false
		for i := range ec.RecentSessions {
			if len(ec.RecentSessions[i].Frontmatter) > 0 {
				ec.RecentSessions[i].Frontmatter = map[string]any{}
				cleared = true
			}
		}
		if !cleared {
			break
		}
		size = contextSize(ec)
	}
	for size > budget {
		cleared := false
		for i := range ec.RecentSessions {
			if ec.RecentSessions[i].Body != "" {
				ec.RecentSessions[i].Body = ""
				cleared = true
				size = contextSize(ec)
				if size <= budget {
					return
				}
			}
		}
		if !cleared {
			return
		}
	}
}

func contextSize(ec *EpisodicContext) int {
	if ec == nil {
		return 0
	}
	size := len(ec.LearnerMemory) + len(ec.PendingMemory) + len(ec.ConceptNotes)
	for _, s := range ec.RecentSessions {
		size += len(s.Body)
		for k, v := range s.Frontmatter {
			size += len(k) + len(fmt.Sprint(v))
		}
	}
	for _, a := range ec.RecentArchives {
		size += len(a.Body)
	}
	for _, inc := range ec.OLMInconsistencies {
		size += len(inc.Type) + len(inc.Concept) + len(inc.Description)
	}
	return size
}

func modifiedAt(learnerID string, scope Scope, key string) time.Time {
	path, err := PathForRead(learnerID, scope, key)
	if err != nil {
		return time.Time{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime().UTC()
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "1", "on":
			return true
		}
	}
	return false
}
