// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna>
// SPDX-License-Identifier: MIT

package engine

import (
	"math"
	"strings"
	"time"

	"tutor-mcp/models"
)

type TransferDimension string

const (
	TransferDimensionNear      TransferDimension = "near"
	TransferDimensionFar       TransferDimension = "far"
	TransferDimensionDebugging TransferDimension = "debugging"
	TransferDimensionTeaching  TransferDimension = "teaching"
	TransferDimensionCreative  TransferDimension = "creative"
)

type TransferReadinessLabel string

type TransferEvidenceTrust string

const (
	TransferReadinessUnobserved TransferReadinessLabel = "unobserved"
	TransferReadinessBlocked    TransferReadinessLabel = "blocked"
	TransferReadinessNarrow     TransferReadinessLabel = "narrow"
	TransferReadinessDeveloping TransferReadinessLabel = "developing"
	TransferReadinessReady      TransferReadinessLabel = "ready"
	TransferReadinessRobust     TransferReadinessLabel = "robust"
)

const (
	TransferEvidenceUnverified TransferEvidenceTrust = "unverified_observations"
	TransferEvidenceTrusted    TransferEvidenceTrust = "trusted_assessment_evidence"
)

const (
	TransferFailureThreshold     = 0.50
	TransferPassingThreshold     = 0.60
	TransferReadyScoreThreshold  = 0.65
	TransferRobustScoreThreshold = 0.80
	// TransferBlockingFailureWindow keeps a recent, trusted failure visible
	// even when older successes make the aggregate average look healthy. A
	// later passing observation in the same dimension resolves the block.
	TransferBlockingFailureWindow = 30 * 24 * time.Hour
)

var canonicalTransferDimensions = []TransferDimension{
	TransferDimensionNear,
	TransferDimensionFar,
	TransferDimensionDebugging,
	TransferDimensionTeaching,
	TransferDimensionCreative,
}

type TransferProfile struct {
	Concept            string                     `json:"concept"`
	GlobalScore        float64                    `json:"global_score"`
	ObservedScore      float64                    `json:"observed_score"`
	Coverage           float64                    `json:"coverage"`
	PassingCoverage    float64                    `json:"passing_coverage"`
	FailureRate        float64                    `json:"failure_rate"`
	Attempts           int                        `json:"attempts"`
	FailureCount       int                        `json:"failure_count"`
	BlockingFailures   int                        `json:"blocking_failures"`
	LatestFailureAt    *time.Time                 `json:"latest_blocking_failure_at,omitempty"`
	UnknownCount       int                        `json:"unknown_count"`
	CoveredDimensions  []TransferDimension        `json:"covered_dimensions"`
	MissingDimensions  []TransferDimension        `json:"missing_dimensions"`
	WeakestDimensions  []TransferDimension        `json:"weakest_dimensions"`
	ReadinessLabel     TransferReadinessLabel     `json:"readiness_label"`
	EvidenceTrust      TransferEvidenceTrust      `json:"evidence_trust"`
	DimensionSummaries []TransferDimensionSummary `json:"dimension_summaries"`
}

type TransferDimensionSummary struct {
	Dimension TransferDimension `json:"dimension"`
	Attempts  int               `json:"attempts"`
	// AverageScore keeps its legacy JSON name but is the score of the latest
	// observation of the dimension (mean of records sharing that timestamp),
	// which is what Passing and the readiness label are judged on.
	AverageScore float64 `json:"average_score"`
	BestScore    float64 `json:"best_score"`
	Covered      bool    `json:"covered"`
	Passing      bool    `json:"passing"`
	FailureCount int     `json:"failure_count"`
}

type transferDimensionBucket struct {
	attempts     int
	best         float64
	failureCount int
	latestSum    float64
	latestCount  int
	latestAt     time.Time
}

// NormalizeTransferDimension maps stored transfer context labels to the stable
// transfer dimensions used by the profile. Legacy tool labels are normalized so
// existing transfer_records can be summarized without a DB migration.
func NormalizeTransferDimension(contextType string) (TransferDimension, bool) {
	switch strings.ToLower(strings.TrimSpace(contextType)) {
	case string(TransferDimensionNear):
		return TransferDimensionNear, true
	case string(TransferDimensionFar), "real_world", "interview":
		return TransferDimensionFar, true
	case string(TransferDimensionDebugging):
		return TransferDimensionDebugging, true
	case string(TransferDimensionTeaching):
		return TransferDimensionTeaching, true
	case string(TransferDimensionCreative):
		return TransferDimensionCreative, true
	default:
		return "", false
	}
}

// BuildTransferProfile aggregates transfer records for one concept. It is pure:
// callers provide all records, no clock or storage is consulted, and input order
// does not affect the result.
func BuildTransferProfile(concept string, scores []*models.TransferRecord) TransferProfile {
	buckets := make(map[TransferDimension]*transferDimensionBucket, len(canonicalTransferDimensions))
	for _, dimension := range canonicalTransferDimensions {
		buckets[dimension] = &transferDimensionBucket{}
	}

	profile := TransferProfile{
		Concept:       concept,
		EvidenceTrust: TransferEvidenceUnverified,
	}

	for _, score := range scores {
		if score == nil {
			continue
		}
		if concept != "" && score.ConceptID != concept {
			continue
		}
		dimension, ok := NormalizeTransferDimension(score.ContextType)
		if !ok {
			profile.UnknownCount++
			continue
		}

		value := clampTransferScore(score.Score)
		bucket := buckets[dimension]
		bucket.attempts++
		// The current state of a dimension is its latest observation (records
		// sharing the latest timestamp, e.g. undated legacy rows, are
		// averaged, so input order never matters). History stays in attempts
		// and failure counts but no longer averages an old failure into a
		// permanent block after the learner repaired it.
		switch {
		case bucket.attempts == 1 || score.CreatedAt.After(bucket.latestAt):
			bucket.latestSum, bucket.latestCount, bucket.latestAt = value, 1, score.CreatedAt
		case score.CreatedAt.Equal(bucket.latestAt):
			bucket.latestSum += value
			bucket.latestCount++
		}
		if bucket.attempts == 1 || value > bucket.best {
			bucket.best = value
		}
		if value < TransferFailureThreshold {
			bucket.failureCount++
			profile.FailureCount++
		}
		profile.Attempts++
	}

	totalObservedScore := 0.0
	totalEffectiveScore := 0.0
	coveredCount := 0
	passingCount := 0
	minObservedScore := math.Inf(1)

	for _, dimension := range canonicalTransferDimensions {
		bucket := buckets[dimension]
		summary := TransferDimensionSummary{
			Dimension:    dimension,
			Attempts:     bucket.attempts,
			FailureCount: bucket.failureCount,
		}
		if bucket.attempts > 0 {
			summary.Covered = true
			summary.AverageScore = roundTransferScore(bucket.latestSum / float64(bucket.latestCount))
			summary.BestScore = roundTransferScore(bucket.best)
			summary.Passing = summary.AverageScore >= TransferPassingThreshold

			coveredCount++
			totalObservedScore += summary.AverageScore
			totalEffectiveScore += summary.AverageScore
			profile.CoveredDimensions = append(profile.CoveredDimensions, dimension)
			if summary.Passing {
				passingCount++
			}
			if summary.AverageScore < minObservedScore {
				minObservedScore = summary.AverageScore
				profile.WeakestDimensions = []TransferDimension{dimension}
			} else if summary.AverageScore == minObservedScore {
				profile.WeakestDimensions = append(profile.WeakestDimensions, dimension)
			}
		} else {
			profile.MissingDimensions = append(profile.MissingDimensions, dimension)
		}
		profile.DimensionSummaries = append(profile.DimensionSummaries, summary)
	}

	dimensionCount := float64(len(canonicalTransferDimensions))
	if coveredCount > 0 {
		profile.ObservedScore = roundTransferScore(totalObservedScore / float64(coveredCount))
	}
	profile.GlobalScore = roundTransferScore(totalEffectiveScore / dimensionCount)
	profile.Coverage = roundTransferScore(float64(coveredCount) / dimensionCount)
	profile.PassingCoverage = roundTransferScore(float64(passingCount) / dimensionCount)
	if profile.Attempts > 0 {
		profile.FailureRate = roundTransferScore(float64(profile.FailureCount) / float64(profile.Attempts))
	}

	profile.ReadinessLabel = transferReadinessLabel(
		profile.Attempts,
		profile.Coverage,
		profile.PassingCoverage,
		profile.ObservedScore,
		minObservedScore,
	)

	return profile
}

// BuildTrustedTransferProfileAt adds temporal failure semantics to the
// aggregate transfer profile. A trusted failure inside the bounded window is
// blocking unless a later passing observation in the same dimension repairs
// it. Passing a zero clock conservatively treats every unresolved failure as
// recent; production callers always supply their decision clock.
func BuildTrustedTransferProfileAt(concept string, scores []*models.TransferRecord, now time.Time) TransferProfile {
	profile := BuildTransferProfile(concept, scores)
	profile.EvidenceTrust = TransferEvidenceTrusted

	type dimensionObservation struct {
		at    time.Time
		score float64
	}
	latest := make(map[TransferDimension]dimensionObservation, len(canonicalTransferDimensions))
	for _, record := range scores {
		if record == nil || (concept != "" && record.ConceptID != concept) {
			continue
		}
		dimension, ok := NormalizeTransferDimension(record.ContextType)
		if !ok {
			continue
		}
		current, exists := latest[dimension]
		if !exists || record.CreatedAt.After(current.at) {
			latest[dimension] = dimensionObservation{at: record.CreatedAt, score: clampTransferScore(record.Score)}
		}
	}
	for _, observation := range latest {
		if observation.score >= TransferFailureThreshold {
			continue
		}
		if !now.IsZero() && !observation.at.IsZero() && now.Sub(observation.at) > TransferBlockingFailureWindow {
			continue
		}
		profile.BlockingFailures++
		failureAt := observation.at
		if !failureAt.IsZero() && (profile.LatestFailureAt == nil || failureAt.After(*profile.LatestFailureAt)) {
			profile.LatestFailureAt = &failureAt
		}
	}
	// One rule for the trusted profile: blocked iff a dimension's latest
	// trusted observation failed inside the blocking window. A failure that
	// aged out of the window no longer blocks through the aggregate label.
	if profile.BlockingFailures > 0 {
		profile.ReadinessLabel = TransferReadinessBlocked
	} else if profile.ReadinessLabel == TransferReadinessBlocked {
		profile.ReadinessLabel = transferReadinessLabel(
			profile.Attempts, profile.Coverage, profile.PassingCoverage,
			max(profile.ObservedScore, TransferFailureThreshold), 1,
		)
	}
	return profile
}

func transferReadinessLabel(attempts int, coverage, passingCoverage, observedScore, minObservedScore float64) TransferReadinessLabel {
	switch {
	case attempts == 0:
		return TransferReadinessUnobserved
	case minObservedScore < TransferFailureThreshold || observedScore < TransferFailureThreshold:
		return TransferReadinessBlocked
	case coverage < 0.40:
		return TransferReadinessNarrow
	case passingCoverage >= 0.80 && observedScore >= TransferRobustScoreThreshold:
		return TransferReadinessRobust
	case passingCoverage >= 0.60 && observedScore >= TransferReadyScoreThreshold:
		return TransferReadinessReady
	default:
		return TransferReadinessDeveloping
	}
}

func roundTransferScore(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func clampTransferScore(v float64) float64 {
	switch {
	case math.IsNaN(v) || v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
