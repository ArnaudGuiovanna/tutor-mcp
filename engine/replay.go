// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

const (
	// DefaultSuspiciousMasteryJump is the absolute p_mastery delta above
	// which an offline replay should flag the update for human review.
	DefaultSuspiciousMasteryJump = 0.25
)

// DecisionReplaySummary is an offline audit summary over persisted
// pedagogical snapshots and their interaction rows.
type DecisionReplaySummary struct {
	TotalSnapshots                 int                       `json:"total_snapshots"`
	ActivityDistribution           map[string]int            `json:"activity_distribution"`
	MasteryDeltaSamples            int                       `json:"mastery_delta_samples"`
	AverageMasteryDelta            float64                   `json:"average_mastery_delta"`
	SuspiciousMasteryJumpThreshold float64                   `json:"suspicious_mastery_jump_threshold"`
	MasteryThreshold               float64                   `json:"mastery_threshold"`
	SuspiciousJumpCount            int                       `json:"suspicious_jump_count"`
	SuspiciousJumps                []MasteryDeltaFinding     `json:"suspicious_jumps"`
	NegativeDeltaCount             int                       `json:"negative_delta_count"`
	NegativeDeltas                 []MasteryDeltaFinding     `json:"negative_deltas"`
	MissingRubricEvidenceCount     int                       `json:"missing_rubric_evidence_count"`
	MissingRubricEvidence          []RubricEvidenceFinding   `json:"missing_rubric_evidence"`
	TransferAfterMasteryGapCount   int                       `json:"transfer_after_mastery_gap_count"`
	TransferAfterMasteryGaps       []TransferAfterMasteryGap `json:"transfer_after_mastery_gaps"`
	SnapshotJSONIssueCount         int                       `json:"snapshot_json_issue_count"`
	SnapshotJSONIssues             []SnapshotJSONIssue       `json:"snapshot_json_issues"`
}

type MasteryDeltaFinding struct {
	SnapshotID          int64     `json:"snapshot_id"`
	InteractionID       int64     `json:"interaction_id"`
	LearnerID           string    `json:"learner_id"`
	DomainID            string    `json:"domain_id"`
	Concept             string    `json:"concept"`
	ActivityType        string    `json:"activity_type"`
	InterpretationBrief string    `json:"interpretation_brief,omitempty"`
	Before              float64   `json:"before"`
	After               float64   `json:"after"`
	Delta               float64   `json:"delta"`
	CreatedAt           time.Time `json:"created_at"`
}

type RubricEvidenceFinding struct {
	SnapshotID          int64     `json:"snapshot_id"`
	InteractionID       int64     `json:"interaction_id"`
	LearnerID           string    `json:"learner_id"`
	DomainID            string    `json:"domain_id"`
	Concept             string    `json:"concept"`
	ActivityType        string    `json:"activity_type"`
	InterpretationBrief string    `json:"interpretation_brief,omitempty"`
	Missing             []string  `json:"missing"`
	CreatedAt           time.Time `json:"created_at"`
}

type TransferAfterMasteryGap struct {
	LearnerID                  string    `json:"learner_id"`
	DomainID                   string    `json:"domain_id"`
	Concept                    string    `json:"concept"`
	MasterySnapshotID          int64     `json:"mastery_snapshot_id"`
	MasteryInteractionID       int64     `json:"mastery_interaction_id"`
	MasteryInterpretationBrief string    `json:"mastery_interpretation_brief,omitempty"`
	MasteryAt                  time.Time `json:"mastery_at"`
	MasteryPMastery            float64   `json:"mastery_p_mastery"`
	PostMasteryDecisions       int       `json:"post_mastery_decisions"`
}

type SnapshotJSONIssue struct {
	SnapshotID    int64  `json:"snapshot_id"`
	InteractionID int64  `json:"interaction_id"`
	Field         string `json:"field"`
	Error         string `json:"error"`
}

type DecisionReplayConfig struct {
	SuspiciousMasteryJump float64
	MasteryThreshold      float64
}

type DecisionReplayOption func(*DecisionReplayConfig)

// BuildDecisionReplaySummary computes a pure offline replay summary. It never
// fails on malformed snapshot JSON; malformed or non-object JSON is recorded in
// SnapshotJSONIssues and the affected metric is skipped.
func BuildDecisionReplaySummary(
	snapshots []*models.PedagogicalSnapshot,
	interactions []*models.Interaction,
	opts ...DecisionReplayOption,
) DecisionReplaySummary {
	cfg := defaultDecisionReplayConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	summary := DecisionReplaySummary{
		TotalSnapshots:                 len(snapshots),
		ActivityDistribution:           map[string]int{},
		SuspiciousMasteryJumpThreshold: cfg.SuspiciousMasteryJump,
		MasteryThreshold:               cfg.MasteryThreshold,
	}

	interactionByID := map[int64]*models.Interaction{}
	for _, interaction := range interactions {
		if interaction != nil && interaction.ID != 0 {
			interactionByID[interaction.ID] = interaction
		}
	}

	var deltaTotal float64
	parsed := make([]replaySnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot == nil {
			summary.SnapshotJSONIssues = append(summary.SnapshotJSONIssues, SnapshotJSONIssue{
				Error: "nil snapshot",
			})
			continue
		}

		summary.ActivityDistribution[snapshot.ActivityType]++

		ps := parseReplaySnapshot(snapshot)
		summary.SnapshotJSONIssues = append(summary.SnapshotJSONIssues, ps.issues...)
		parsed = append(parsed, ps)

		if ps.hasBeforePMastery && ps.hasAfterPMastery {
			delta := ps.afterPMastery - ps.beforePMastery
			deltaTotal += delta
			summary.MasteryDeltaSamples++

			finding := masteryDeltaFinding(snapshot, ps.beforePMastery, ps.afterPMastery, delta)
			if math.Abs(delta) >= cfg.SuspiciousMasteryJump {
				summary.SuspiciousJumps = append(summary.SuspiciousJumps, finding)
			}
			if delta < 0 {
				summary.NegativeDeltas = append(summary.NegativeDeltas, finding)
			}
		}

		if replayRequiresRubricEvidence(snapshot.ActivityType) {
			if missing := missingRubricEvidence(ps.observation, interactionByID[snapshot.InteractionID]); len(missing) > 0 {
				summary.MissingRubricEvidence = append(summary.MissingRubricEvidence, RubricEvidenceFinding{
					SnapshotID:          snapshot.ID,
					InteractionID:       snapshot.InteractionID,
					LearnerID:           snapshot.LearnerID,
					DomainID:            snapshot.DomainID,
					Concept:             snapshot.Concept,
					ActivityType:        snapshot.ActivityType,
					InterpretationBrief: snapshot.InterpretationBrief,
					Missing:             missing,
					CreatedAt:           snapshot.CreatedAt,
				})
			}
		}
	}

	if summary.MasteryDeltaSamples > 0 {
		summary.AverageMasteryDelta = deltaTotal / float64(summary.MasteryDeltaSamples)
	}

	summary.TransferAfterMasteryGaps = transferAfterMasteryGaps(parsed, interactions, cfg.MasteryThreshold)
	summary.SuspiciousJumpCount = len(summary.SuspiciousJumps)
	summary.NegativeDeltaCount = len(summary.NegativeDeltas)
	summary.MissingRubricEvidenceCount = len(summary.MissingRubricEvidence)
	summary.TransferAfterMasteryGapCount = len(summary.TransferAfterMasteryGaps)
	summary.SnapshotJSONIssueCount = len(summary.SnapshotJSONIssues)
	return summary
}

func defaultDecisionReplayConfig() DecisionReplayConfig {
	return DecisionReplayConfig{
		SuspiciousMasteryJump: DefaultSuspiciousMasteryJump,
		MasteryThreshold:      algorithms.MasteryBKT(),
	}
}

type replaySnapshot struct {
	snapshot          *models.PedagogicalSnapshot
	beforePMastery    float64
	hasBeforePMastery bool
	afterPMastery     float64
	hasAfterPMastery  bool
	observation       map[string]any
	issues            []SnapshotJSONIssue
}

func parseReplaySnapshot(snapshot *models.PedagogicalSnapshot) replaySnapshot {
	ps := replaySnapshot{snapshot: snapshot}

	before, errText := decodeReplayJSONObject(snapshot.BeforeJSON)
	if errText != "" {
		ps.issues = append(ps.issues, snapshotJSONIssue(snapshot, "before_json", errText))
	}
	if beforeValue, ok, errText := replayPMastery(before); errText != "" {
		ps.issues = append(ps.issues, snapshotJSONIssue(snapshot, "before_json.p_mastery", errText))
	} else if ok {
		ps.beforePMastery = beforeValue
		ps.hasBeforePMastery = true
	}

	observation, errText := decodeReplayJSONObject(snapshot.ObservationJSON)
	if errText != "" {
		ps.issues = append(ps.issues, snapshotJSONIssue(snapshot, "observation_json", errText))
	}
	ps.observation = observation

	after, errText := decodeReplayJSONObject(snapshot.AfterJSON)
	if errText != "" {
		ps.issues = append(ps.issues, snapshotJSONIssue(snapshot, "after_json", errText))
	}
	if afterValue, ok, errText := replayPMastery(after); errText != "" {
		ps.issues = append(ps.issues, snapshotJSONIssue(snapshot, "after_json.p_mastery", errText))
	} else if ok {
		ps.afterPMastery = afterValue
		ps.hasAfterPMastery = true
	}

	if _, errText := decodeReplayJSONObject(snapshot.DecisionJSON); errText != "" {
		ps.issues = append(ps.issues, snapshotJSONIssue(snapshot, "decision_json", errText))
	}

	return ps
}

func decodeReplayJSONObject(raw string) (map[string]any, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, err.Error()
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, err.Error()
		}
		return nil, "contains multiple JSON values"
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, fmt.Sprintf("expected JSON object, got %T", parsed)
	}
	return obj, ""
}

func replayPMastery(snapshot map[string]any) (float64, bool, string) {
	if snapshot == nil {
		return 0, false, ""
	}
	raw, ok := snapshot["p_mastery"]
	if !ok {
		return 0, false, ""
	}
	value, err := replayFloat(raw)
	if err != nil {
		return 0, false, err.Error()
	}
	if !isReplayUnitInterval(value) {
		return 0, false, fmt.Sprintf("p_mastery must be finite and in [0, 1], got %v", value)
	}
	return value, true, ""
}

func replayFloat(raw any) (float64, error) {
	switch v := raw.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, err
		}
		return f, nil
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, err
		}
		return f, nil
	default:
		return 0, fmt.Errorf("expected numeric p_mastery, got %T", raw)
	}
}

func isReplayUnitInterval(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func snapshotJSONIssue(snapshot *models.PedagogicalSnapshot, field, errText string) SnapshotJSONIssue {
	return SnapshotJSONIssue{
		SnapshotID:    snapshot.ID,
		InteractionID: snapshot.InteractionID,
		Field:         field,
		Error:         errText,
	}
}

func masteryDeltaFinding(snapshot *models.PedagogicalSnapshot, before, after, delta float64) MasteryDeltaFinding {
	return MasteryDeltaFinding{
		SnapshotID:          snapshot.ID,
		InteractionID:       snapshot.InteractionID,
		LearnerID:           snapshot.LearnerID,
		DomainID:            snapshot.DomainID,
		Concept:             snapshot.Concept,
		ActivityType:        snapshot.ActivityType,
		InterpretationBrief: snapshot.InterpretationBrief,
		Before:              before,
		After:               after,
		Delta:               delta,
		CreatedAt:           snapshot.CreatedAt,
	}
}

func replayRequiresRubricEvidence(activityType string) bool {
	switch models.ActivityType(activityType) {
	case models.ActivityDiagnosticAssessment,
		models.ActivityRecall,
		models.ActivityPractice,
		models.ActivityMasteryChallenge,
		models.ActivityDebuggingCase,
		models.ActivityDebugMisconception,
		models.ActivityFeynmanPrompt,
		models.ActivityTransferProbe:
		return true
	default:
		return false
	}
}

func missingRubricEvidence(observation map[string]any, interaction *models.Interaction) []string {
	hasRubric := observationHasKey(observation, "rubric")
	hasRubricScore := observationHasKey(observation, "rubric_score")
	if interaction != nil {
		hasRubric = hasRubric || strings.TrimSpace(interaction.RubricJSON) != ""
		hasRubricScore = hasRubricScore || strings.TrimSpace(interaction.RubricScoreJSON) != ""
	}

	var missing []string
	if !hasRubric {
		missing = append(missing, "rubric")
	}
	if !hasRubricScore {
		missing = append(missing, "rubric_score")
	}
	return missing
}

func observationHasKey(observation map[string]any, key string) bool {
	value, ok := observation[key]
	return ok && value != nil
}

type replayConceptKey struct {
	learnerID string
	domainID  string
	concept   string
}

type replayMasteryEvent struct {
	key                 replayConceptKey
	snapshotID          int64
	interactionID       int64
	interpretationBrief string
	createdAt           time.Time
	pMastery            float64
}

func transferAfterMasteryGaps(
	snapshots []replaySnapshot,
	interactions []*models.Interaction,
	threshold float64,
) []TransferAfterMasteryGap {
	masteryEvents := map[replayConceptKey]replayMasteryEvent{}
	for _, ps := range snapshots {
		if ps.snapshot == nil {
			continue
		}
		value, ok := replayMasteryValue(ps, threshold)
		if !ok {
			continue
		}

		event := replayMasteryEvent{
			key:                 replayKey(ps.snapshot),
			snapshotID:          ps.snapshot.ID,
			interactionID:       ps.snapshot.InteractionID,
			interpretationBrief: ps.snapshot.InterpretationBrief,
			createdAt:           ps.snapshot.CreatedAt,
			pMastery:            value,
		}
		if existing, exists := masteryEvents[event.key]; !exists || replayEventBefore(event.createdAt, event.snapshotID, existing.createdAt, existing.snapshotID) {
			masteryEvents[event.key] = event
		}
	}

	keys := make([]replayConceptKey, 0, len(masteryEvents))
	for key := range masteryEvents {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].learnerID != keys[j].learnerID {
			return keys[i].learnerID < keys[j].learnerID
		}
		if keys[i].domainID != keys[j].domainID {
			return keys[i].domainID < keys[j].domainID
		}
		return keys[i].concept < keys[j].concept
	})

	gaps := make([]TransferAfterMasteryGap, 0)
	for _, key := range keys {
		event := masteryEvents[key]
		postMasteryDecisions := 0
		hasTransfer := false

		for _, ps := range snapshots {
			if ps.snapshot == nil || replayKey(ps.snapshot) != key {
				continue
			}
			if !replayOccurredAtOrAfter(ps.snapshot.CreatedAt, ps.snapshot.ID, event.createdAt, event.snapshotID) {
				continue
			}
			postMasteryDecisions++
			if models.ActivityType(ps.snapshot.ActivityType) == models.ActivityTransferProbe {
				hasTransfer = true
			}
		}

		if !hasTransfer {
			for _, interaction := range interactions {
				if !replayInteractionMatchesKey(interaction, key) {
					continue
				}
				if !replayOccurredAtOrAfter(interaction.CreatedAt, interaction.ID, event.createdAt, event.interactionID) {
					continue
				}
				if models.ActivityType(interaction.ActivityType) == models.ActivityTransferProbe {
					hasTransfer = true
					break
				}
			}
		}

		if hasTransfer {
			continue
		}
		gaps = append(gaps, TransferAfterMasteryGap{
			LearnerID:                  key.learnerID,
			DomainID:                   key.domainID,
			Concept:                    key.concept,
			MasterySnapshotID:          event.snapshotID,
			MasteryInteractionID:       event.interactionID,
			MasteryInterpretationBrief: event.interpretationBrief,
			MasteryAt:                  event.createdAt,
			MasteryPMastery:            event.pMastery,
			PostMasteryDecisions:       postMasteryDecisions,
		})
	}
	return gaps
}

func replayMasteryValue(ps replaySnapshot, threshold float64) (float64, bool) {
	if ps.hasAfterPMastery && ps.afterPMastery >= threshold {
		return ps.afterPMastery, true
	}
	if !ps.hasAfterPMastery && ps.hasBeforePMastery && ps.beforePMastery >= threshold {
		return ps.beforePMastery, true
	}
	return 0, false
}

func replayKey(snapshot *models.PedagogicalSnapshot) replayConceptKey {
	return replayConceptKey{
		learnerID: snapshot.LearnerID,
		domainID:  snapshot.DomainID,
		concept:   snapshot.Concept,
	}
}

func replayInteractionMatchesKey(interaction *models.Interaction, key replayConceptKey) bool {
	if interaction == nil {
		return false
	}
	if interaction.LearnerID != key.learnerID || interaction.Concept != key.concept {
		return false
	}
	return interaction.DomainID == "" || key.domainID == "" || interaction.DomainID == key.domainID
}

func replayEventBefore(aTime time.Time, aID int64, bTime time.Time, bID int64) bool {
	if !aTime.IsZero() && !bTime.IsZero() && !aTime.Equal(bTime) {
		return aTime.Before(bTime)
	}
	if aID != 0 && bID != 0 {
		return aID < bID
	}
	return false
}

func replayOccurredAtOrAfter(candidateTime time.Time, candidateID int64, anchorTime time.Time, anchorID int64) bool {
	if !candidateTime.IsZero() && !anchorTime.IsZero() && !candidateTime.Equal(anchorTime) {
		return candidateTime.After(anchorTime)
	}
	if candidateID != 0 && anchorID != 0 {
		return candidateID >= anchorID
	}
	return true
}
