// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Package engine — Open Learner Model global aggregator.
//
// BuildGlobalOLMSnapshot rolls up across all non-archived domains for a
// learner — exposed via get_olm_snapshot(scope:"global") for the
// "Global model" view.

package engine

import (
	"context"
	"fmt"
	"time"

	storeport "tutor-mcp/store"
)

const (
	// sparklineWindow is the number of samples kept for the calibration,
	// autonomy and satisfaction sparklines surfaced in the global snapshot.
	sparklineWindow = 30
	// recentEventDays is the look-back window for the recent events timeline.
	recentEventDays = 7
)

// TimePoint is a daily aggregate sample (for sparklines).
type TimePoint struct {
	Day   string  `json:"day"` // YYYY-MM-DD UTC
	Value float64 `json:"value"`
}

// DomainSummary is one row of the Savoir column.
type DomainSummary struct {
	DomainID     string  `json:"domain_id"`
	DomainName   string  `json:"domain_name"`
	PersonalGoal string  `json:"personal_goal"`
	Solid        int     `json:"solid"`
	InProgress   int     `json:"in_progress"`
	Fragile      int     `json:"fragile"`
	NotStarted   int     `json:"not_started"`
	KSTProgress  float64 `json:"kst_progress"`
}

// GoalProgress is one row of the objectives column.
type GoalProgress struct {
	DomainID     string  `json:"domain_id"`
	PersonalGoal string  `json:"personal_goal"`
	Progress     float64 `json:"progress"` // mirror of KSTProgress
}

// LearnerEvent is one row of the recent events timeline.
type LearnerEvent struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // "mastery_threshold"|"calibration_threshold"|"retention_drop"|"streak_start"
	Message string    `json:"message"`
	Concept string    `json:"concept,omitempty"`
}

// GlobalOLMSnapshot is the aggregator payload for the global OLM scope.
type GlobalOLMSnapshot struct {
	Streak              int             `json:"streak"`
	TotalSolid          int             `json:"total_solid"`
	Domains             []DomainSummary `json:"domains"`
	CalibrationHistory  []TimePoint     `json:"calibration_history"`
	AutonomyHistory     []TimePoint     `json:"autonomy_history"`
	SatisfactionHistory []TimePoint     `json:"satisfaction_history"`
	Goals               []GoalProgress  `json:"goals"`
	RecentEvents        []LearnerEvent  `json:"recent_events"`
}

// BuildGlobalOLMSnapshot aggregates across all non-archived domains for a
// learner — powers get_olm_snapshot(scope:"global").
func BuildGlobalOLMSnapshot(ctx context.Context, store storeport.Store, learnerID string) (*GlobalOLMSnapshot, error) {
	g := &GlobalOLMSnapshot{}

	domains, err := store.GetDomainsByLearner(ctx, learnerID, false /*includeArchived*/)
	if err != nil {
		return nil, fmt.Errorf("global olm: list domains: %w", err)
	}

	for _, d := range domains {
		snap, err := BuildOLMSnapshot(ctx, store, learnerID, d.ID)
		if err != nil {
			return nil, fmt.Errorf("global olm: build domain %s: %w", d.ID, err)
		}
		g.Domains = append(g.Domains, DomainSummary{
			DomainID:     d.ID,
			DomainName:   d.Name,
			PersonalGoal: d.PersonalGoal,
			Solid:        snap.Solid,
			InProgress:   snap.InProgress,
			Fragile:      snap.Fragile,
			NotStarted:   snap.NotStarted,
			KSTProgress:  snap.KSTProgress,
		})
		g.TotalSolid += snap.Solid
		g.Goals = append(g.Goals, GoalProgress{
			DomainID:     d.ID,
			PersonalGoal: d.PersonalGoal,
			Progress:     snap.KSTProgress,
		})
	}

	g.Streak, err = store.GetActivityStreak(ctx, learnerID)
	if err != nil {
		return nil, fmt.Errorf("global olm: activity streak: %w", err)
	}

	// Calibration sparkline — last 30 samples. GetCalibrationBiasHistory returns
	// DESC (newest-first); reverse-iterate so the resulting slice is oldest-first
	// to match the autonomy/satisfaction sparklines. Each entry's Day is its own
	// day-offset (i=0 newest → today; i=len-1 oldest → today - (len-1)).
	hist, err := store.GetCalibrationBiasHistory(ctx, learnerID, sparklineWindow)
	if err != nil {
		return nil, fmt.Errorf("global olm: calibration history: %w", err)
	}
	now := time.Now().UTC()
	for i := len(hist) - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		g.CalibrationHistory = append(g.CalibrationHistory, TimePoint{Day: day, Value: hist[i]})
	}

	affects, err := store.GetRecentAffectStates(ctx, learnerID, sparklineWindow)
	if err != nil {
		return nil, fmt.Errorf("global olm: affect history: %w", err)
	}
	for i := len(affects) - 1; i >= 0; i-- {
		af := affects[i]
		day := af.CreatedAt.UTC().Format("2006-01-02")
		g.AutonomyHistory = append(g.AutonomyHistory, TimePoint{Day: day, Value: af.AutonomyScore})
		g.SatisfactionHistory = append(g.SatisfactionHistory, TimePoint{Day: day, Value: float64(af.Satisfaction)})
	}

	// Recent events — past 7 days. GetRecentLearnerEvents returns DESC
	// (newest-first), preserved as the natural "newest at top" ordering.
	since := time.Now().UTC().AddDate(0, 0, -recentEventDays)
	rawEvents, err := store.GetRecentLearnerEvents(ctx, learnerID, since)
	if err != nil {
		return nil, fmt.Errorf("global olm: recent events: %w", err)
	}
	for _, re := range rawEvents {
		g.RecentEvents = append(g.RecentEvents, LearnerEvent{
			At: re.At, Kind: re.Kind, Message: re.Message, Concept: re.Concept,
		})
	}

	return g, nil
}
