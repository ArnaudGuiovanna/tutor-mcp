// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"math"
	"sort"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

type MasteryConfidenceLabel string

const (
	MasteryConfidenceLow    MasteryConfidenceLabel = "low"
	MasteryConfidenceMedium MasteryConfidenceLabel = "medium"
	MasteryConfidenceHigh   MasteryConfidenceLabel = "high"
)

type MasteryUncertaintyReason string

const (
	UncertaintyReasonFewObservations    MasteryUncertaintyReason = "few_observations"
	UncertaintyReasonStaleEvidence      MasteryUncertaintyReason = "stale_evidence"
	UncertaintyReasonHighErrorRate      MasteryUncertaintyReason = "high_error_rate"
	UncertaintyReasonModelNearThreshold MasteryUncertaintyReason = "model_near_threshold"
	UncertaintyReasonLowDiversity       MasteryUncertaintyReason = "low_diversity"
)

type MasteryUncertainty struct {
	UncertaintyScore float64                    `json:"uncertainty_score"`
	ConfidenceLabel  MasteryConfidenceLabel     `json:"confidence_label"`
	Reasons          []MasteryUncertaintyReason `json:"reasons"`
}

// MasteryEvidenceProfile tunes ComputeMasteryUncertainty without coupling the
// pure calculation to wall-clock time, storage, or a specific caller.
//
// Zero-valued fields keep the defaults. Set Now when stale evidence should be
// evaluated; without an explicit decision clock the stale-evidence signal is
// omitted rather than misusing FSRS ElapsedDays as a current age.
type MasteryEvidenceProfile struct {
	Now                          time.Time
	MinObservations              int
	StaleAfter                   time.Duration
	RecentErrorWindow            time.Duration
	RecentErrorSampleSize        int
	HighErrorRateMinObservations int
	HighErrorRateThreshold       float64
	ThresholdMargin              float64
	MinDiversity                 int
}

// ComputeMasteryUncertainty estimates how much trust the engine should put in
// the current mastery decision for one concept. It is intentionally pure:
// callers pass the concept state, the available interactions, and optionally a
// deterministic evidence profile.
func ComputeMasteryUncertainty(
	cs *models.ConceptState,
	interactions []*models.Interaction,
	profiles ...MasteryEvidenceProfile,
) MasteryUncertainty {
	profile := normalizeMasteryEvidenceProfile(profiles...)
	relevant := relevantMasteryInteractions(cs, interactions)

	observationCount := len(relevant)
	if cs != nil && cs.Reps > observationCount {
		observationCount = cs.Reps
	}

	reasons := make([]MasteryUncertaintyReason, 0, 5)
	score := 0.0

	if observationCount < profile.MinObservations {
		reasons = append(reasons, UncertaintyReasonFewObservations)
		missingRatio := float64(profile.MinObservations-observationCount) / float64(profile.MinObservations)
		score += 0.45 + 0.25*missingRatio
	}

	if isMasteryEvidenceStale(cs, relevant, profile) {
		reasons = append(reasons, UncertaintyReasonStaleEvidence)
		score += 0.30
	}

	if errorRate, count := recentMasteryErrorRate(relevant, profile); count >= profile.HighErrorRateMinObservations && errorRate >= profile.HighErrorRateThreshold {
		reasons = append(reasons, UncertaintyReasonHighErrorRate)
		score += 0.20 + 0.30*errorRate
	}

	if isMasteryNearThreshold(cs, profile.ThresholdMargin) {
		reasons = append(reasons, UncertaintyReasonModelNearThreshold)
		distance := math.Abs(cs.PMastery - algorithms.MasteryBKT())
		closeness := 1 - distance/profile.ThresholdMargin
		score += 0.20 + 0.20*clamp01(closeness)
	}

	if observationCount >= profile.MinObservations && len(relevant) > 0 && masteryEvidenceDiversity(relevant) < profile.MinDiversity {
		reasons = append(reasons, UncertaintyReasonLowDiversity)
		score += 0.20
	}

	score = clamp01(score)
	return MasteryUncertainty{
		UncertaintyScore: score,
		ConfidenceLabel:  masteryConfidenceLabel(score),
		Reasons:          reasons,
	}
}

func normalizeMasteryEvidenceProfile(profiles ...MasteryEvidenceProfile) MasteryEvidenceProfile {
	profile := MasteryEvidenceProfile{
		MinObservations:              3,
		StaleAfter:                   21 * 24 * time.Hour,
		RecentErrorWindow:            7 * 24 * time.Hour,
		RecentErrorSampleSize:        5,
		HighErrorRateMinObservations: 3,
		HighErrorRateThreshold:       0.50,
		ThresholdMargin:              0.05,
		MinDiversity:                 2,
	}

	for _, override := range profiles {
		if !override.Now.IsZero() {
			profile.Now = override.Now
		}
		if override.MinObservations > 0 {
			profile.MinObservations = override.MinObservations
		}
		if override.StaleAfter > 0 {
			profile.StaleAfter = override.StaleAfter
		}
		if override.RecentErrorWindow > 0 {
			profile.RecentErrorWindow = override.RecentErrorWindow
		}
		if override.RecentErrorSampleSize > 0 {
			profile.RecentErrorSampleSize = override.RecentErrorSampleSize
		}
		if override.HighErrorRateMinObservations > 0 {
			profile.HighErrorRateMinObservations = override.HighErrorRateMinObservations
		}
		if override.HighErrorRateThreshold > 0 {
			profile.HighErrorRateThreshold = override.HighErrorRateThreshold
		}
		if override.ThresholdMargin > 0 {
			profile.ThresholdMargin = override.ThresholdMargin
		}
		if override.MinDiversity > 0 {
			profile.MinDiversity = override.MinDiversity
		}
	}

	return profile
}

func relevantMasteryInteractions(cs *models.ConceptState, interactions []*models.Interaction) []*models.Interaction {
	concept := ""
	if cs != nil {
		concept = cs.Concept
	}

	relevant := make([]*models.Interaction, 0, len(interactions))
	for _, in := range interactions {
		if in == nil {
			continue
		}
		if concept != "" && in.Concept != concept {
			continue
		}
		relevant = append(relevant, in)
	}

	sort.SliceStable(relevant, func(i, j int) bool {
		return relevant[i].CreatedAt.After(relevant[j].CreatedAt)
	})
	return relevant
}

func isMasteryEvidenceStale(cs *models.ConceptState, interactions []*models.Interaction, profile MasteryEvidenceProfile) bool {
	latest := latestMasteryEvidenceAt(cs, interactions)
	if profile.Now.IsZero() || latest.IsZero() || latest.After(profile.Now) {
		return false
	}
	return profile.Now.Sub(latest) > profile.StaleAfter
}

func latestMasteryEvidenceAt(cs *models.ConceptState, interactions []*models.Interaction) time.Time {
	var latest time.Time
	if cs != nil {
		if cs.LastReview != nil && cs.LastReview.After(latest) {
			latest = *cs.LastReview
		}
		if cs.UpdatedAt.After(latest) {
			latest = cs.UpdatedAt
		}
	}
	for _, in := range interactions {
		if in.CreatedAt.After(latest) {
			latest = in.CreatedAt
		}
	}
	return latest
}

func recentMasteryErrorRate(interactions []*models.Interaction, profile MasteryEvidenceProfile) (float64, int) {
	failures := 0
	total := 0

	if !profile.Now.IsZero() {
		cutoff := profile.Now.Add(-profile.RecentErrorWindow)
		for _, in := range interactions {
			if in.CreatedAt.IsZero() || in.CreatedAt.Before(cutoff) {
				continue
			}
			total++
			if !in.Success {
				failures++
			}
		}
		if total == 0 {
			return 0, 0
		}
		return float64(failures) / float64(total), total
	}

	limit := profile.RecentErrorSampleSize
	if len(interactions) < limit {
		limit = len(interactions)
	}
	for _, in := range interactions[:limit] {
		total++
		if !in.Success {
			failures++
		}
	}
	if total == 0 {
		return 0, 0
	}
	return float64(failures) / float64(total), total
}

func isMasteryNearThreshold(cs *models.ConceptState, margin float64) bool {
	if cs == nil || margin <= 0 || math.IsNaN(cs.PMastery) || math.IsInf(cs.PMastery, 0) {
		return false
	}
	return math.Abs(cs.PMastery-algorithms.MasteryBKT()) <= margin
}

func masteryEvidenceDiversity(interactions []*models.Interaction) int {
	activityTypes := make(map[string]bool)
	for _, in := range interactions {
		activityTypes[in.ActivityType] = true
	}
	return len(activityTypes)
}

func masteryConfidenceLabel(score float64) MasteryConfidenceLabel {
	switch {
	case score >= 0.60:
		return MasteryConfidenceLow
	case score >= 0.33:
		return MasteryConfidenceMedium
	default:
		return MasteryConfidenceHigh
	}
}

func clamp01(v float64) float64 {
	switch {
	case math.IsNaN(v) || v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
