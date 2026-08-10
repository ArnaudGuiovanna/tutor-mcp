// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestNoLiteralMasteryThresholds is an anti-drift guard. It scans the
// production source tree for literal comparisons against 0.70/0.80/0.85
// in the same line as a "mastery"/"PMastery" identifier. Each match must
// either live in algorithms/thresholds.go or appear in the explicit
// allow-list below — see docs/regulation-design/07-threshold-resolver.md
// §2 (Non-refactored sites) for the rationale of each entry.
//
// Adding a new entry here is a deliberate decision. The default action
// when this test fires is to refactor the call site to use
// algorithms.MasteryBKT() / MasteryKST() / MasteryMid().
func TestNoLiteralMasteryThresholds(t *testing.T) {
	allowed := map[string]bool{
		// User-facing description string, not a code comparison.
		"tools/mastery.go:24": true,
	}

	repoRoot := findRepoRoot(t)
	// Require at least one of <, >, <=, >= in the operator (single '=' is
	// either assignment or text inside a comment/string and is not an
	// adversarial threshold comparison).
	pattern := regexp.MustCompile(`(?i)\b(p?mastery)\b[^\n]*[<>]=?\s*0\.(70|80|85)\b`)

	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := info.Name()
			if base == ".git" || base == "vendor" || base == "node_modules" || base == "docs" || base == "data" || base == "assets" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		if rel == filepath.Join("algorithms", "thresholds.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(data), "\n") {
			if pattern.MatchString(line) {
				key := rel + ":" + strconv.Itoa(i+1)
				if !allowed[key] {
					t.Errorf("literal mastery threshold at %s — use algorithms.Mastery{BKT,KST,Mid}() or add to allowed map with justification\n  > %s", key, strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := cwd; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatal("repo root (go.mod) not found")
	return ""
}
