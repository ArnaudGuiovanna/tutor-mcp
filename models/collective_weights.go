// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

const (
	CollectiveWeightsPolicyVersion     = "institution-collective-weights-v2"
	BKTReferencePolicyVersion          = "bkt-reference-v1"
	BKTIndividualEstimatePolicyVersion = "independent-reference-estimate-v1"
	BKTPersonalParameterPolicy         = "personal-fixed-reference-v1"
	BKTCommonParameterPolicy           = "common-collective-reference-v2"
	BKTCalibrationProfileWindow        = 20
	BKTCalibrationCandidateHorizon     = 256
	// A concept is weighted only from this many distinct learners...
	CollectiveWeightMinContributors = 30
	// ...each with at least this many admissible committed observations of it.
	CollectiveWeightMinObservations = 5
	// Retained for callers of the original policy. These are admissible
	// observations, not the FSRS review/card repetition count.
	CollectiveWeightMinReps = CollectiveWeightMinObservations
	// A new version is published only if a parameter moves at least this much,
	// and never by more than the cap, so the collective stays stable.
	CollectiveWeightMinChange = 0.01
	CollectiveWeightMaxStep   = 0.02
	// Applying a weight to a learner who already has reviews.
	CollectiveBlendFactor  = 0.25
	CollectiveBlendMaxStep = 0.05
	CollectiveParamMin     = 0.01
	CollectiveParamMax     = 0.5
)

const (
	CollectiveApplyPrior = "prior"
	CollectiveApplyBlend = "blend"
)

// BKTParams are the four BKT probabilities a collective weight can carry.
type BKTParams struct {
	PLearn  float64 `json:"p_learn"`
	PForget float64 `json:"p_forget"`
	PSlip   float64 `json:"p_slip"`
	PGuess  float64 `json:"p_guess"`
}

// BKTReferenceParams is independent of learner state and collective outputs.
// Personal effective parameters and independent calibration estimates share
// this versioned reference, so applying a collective weight cannot feed it.
func BKTReferenceParams() BKTParams {
	return BKTParams{PLearn: .15, PForget: .05, PSlip: .1, PGuess: .2}
}

// BKTCalibrationEvidence identifies the bounded current admissible source.
// Count and digest cover exactly the estimator's newest-first profile window,
// selected from the latest BKTCalibrationCandidateHorizon scoped candidates.
// A non-nil value also identifies a common domain with no evidence; limit zero
// requests only this classification without reading observation history.
type BKTCalibrationEvidence struct {
	TenantID           string
	EnrollmentID       string
	FormationVersionID string
	FormationConceptID string
	CurriculumVersion  int
	Observations       int
	LastInteractionID  int64
	EvidenceHash       string
	Interactions       []*Interaction
}

// CollectiveWeight is one immutable version of a concept's collective
// parameters. It holds no learner identifier.
type CollectiveWeight struct {
	Version       int64     `json:"version"`
	Contributors  int       `json:"contributors"`
	PolicyVersion string    `json:"policy_version"`
	ComputedAt    time.Time `json:"computed_at"`
	BKTParams
}

// CollectiveWeightApplication traces one change to one learner's parameters.
type CollectiveWeightApplication struct {
	Version int64     `json:"version"`
	Mode    string    `json:"mode"`
	Before  BKTParams `json:"before"`
	After   BKTParams `json:"after"`
}
