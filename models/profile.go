// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

// LearnerProfile contains learner-declared preferences that are safe for the
// coaching layer to persist and consume. Algorithm-derived values (for
// example calibration bias and autonomy) deliberately do not belong here:
// they are computed from evidence and must not be writable by the host LLM.
type LearnerProfile struct {
	Device         string `json:"device,omitempty"`
	Language       string `json:"language,omitempty"`
	AffectBaseline string `json:"affect_baseline,omitempty"`
}
