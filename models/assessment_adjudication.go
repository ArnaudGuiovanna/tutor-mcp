// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

// AssessmentAdjudication is an append-only disposition of one frozen review.
// Original host grades and reviews remain intact, including disagreements.
type AssessmentAdjudication struct {
	ID                           string    `json:"id"`
	AttemptID                    string    `json:"attempt_id"`
	ReviewID                     string    `json:"review_id"`
	Revision                     int       `json:"revision"`
	Verdict                      string    `json:"verdict"`
	AuthorityID                  string    `json:"authority_id"`
	KeyID                        string    `json:"key_id"`
	CertificateID                string    `json:"certificate_id"`
	CertificateHash              string    `json:"certificate_hash"`
	ActorUserID                  string    `json:"actor_user_id"`
	MaterialHash                 string    `json:"material_hash"`
	ScoreHash                    string    `json:"score_hash"`
	CreatedAt                    time.Time `json:"created_at"`
	CurriculumInvalidatedVersion int       `json:"curriculum_invalidated_version"`
}
