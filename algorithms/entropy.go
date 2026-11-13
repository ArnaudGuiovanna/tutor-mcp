// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

import "math"

// BinaryEntropy computes Shannon binary entropy in bits:
//
//	H(p) = -p log2(p) - (1-p) log2(1-p)
//
// Returns 0 at the saturation edges (p ≤ 0 or p ≥ 1). Used by
// engine/phase_fsm.go for the audit-only mean-entropy signal of DIAGNOSTIC.
func BinaryEntropy(p float64) float64 {
	if p <= 0 || p >= 1 {
		return 0
	}
	return -p*math.Log2(p) - (1-p)*math.Log2(1-p)
}
