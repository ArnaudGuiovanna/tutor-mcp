// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

import (
	"math"
	"testing"
)

func TestBinaryEntropy(t *testing.T) {
	for _, tc := range []struct{ p, want float64 }{
		{-0.1, 0}, {0, 0}, {1, 0}, {1.2, 0}, {0.5, 1},
		{0.1, -0.1*math.Log2(0.1) - 0.9*math.Log2(0.9)},
	} {
		if got := BinaryEntropy(tc.p); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("BinaryEntropy(%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
	if BinaryEntropy(0.2) != BinaryEntropy(0.8) {
		t.Error("binary entropy must be symmetric")
	}
}
