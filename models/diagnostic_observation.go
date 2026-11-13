// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "time"

// DiagnosticObservation is one qualified diagnostic answer: an evaluated,
// unassisted DIAGNOSTIC_ASSESSMENT interaction bound to its assessment attempt
// and to the current curriculum version.
type DiagnosticObservation struct {
	Concept    string
	Success    bool
	ObservedAt time.Time
}
