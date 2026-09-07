// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package assessment

import (
	"fmt"
	"sort"
	"strings"

	"tutor-mcp/models"
)

// ValidateCurriculumFindings measures explicit review coverage; it cannot
// verify semantic truth or certify the reviewer. Missing coverage stays visible.
func ValidateCurriculumFindings(snapshot *models.CurriculumSnapshot, raw string) ([]models.CurriculumFinding, string, int, int, error) {
	var findings []models.CurriculumFinding
	if snapshot == nil || DecodeDocument(raw, &findings) != nil || len(findings) == 0 || len(findings) > 1000 {
		return nil, "", 0, 0, fmt.Errorf("invalid curriculum findings")
	}
	concepts, keys := map[string]bool{}, map[string]string{}
	expected := map[string]bool{}
	key := func(concept, aspect, related string) string { return concept + "\x00" + aspect + "\x00" + related }
	for _, c := range snapshot.Concepts {
		if c.Status != models.CurriculumConceptActive {
			continue
		}
		concepts[c.ID] = true
		keys[c.Key] = c.ID
		for _, aspect := range []string{"definition", "outcomes", "criteria"} {
			expected[key(c.ID, aspect, "")] = true
		}
	}
	for target, prerequisites := range snapshot.Graph.Prerequisites {
		for _, prerequisite := range prerequisites {
			if keys[target] != "" && keys[prerequisite] != "" {
				expected[key(keys[target], "prerequisite", keys[prerequisite])] = true
			}
		}
	}
	seen := map[string]bool{}
	reviewed := 0
	needsRevision := false
	for _, f := range findings {
		if !concepts[f.ConceptID] || strings.TrimSpace(f.Rationale) == "" || len(f.Rationale) > 2000 ||
			(f.Judgment != "adequate" && f.Judgment != "needs_revision" && f.Judgment != "not_assessed") {
			return nil, "", 0, 0, fmt.Errorf("invalid finding scope or judgment")
		}
		k := key(f.ConceptID, f.Aspect, f.RelatedConceptID)
		if seen[k] {
			return nil, "", 0, 0, fmt.Errorf("duplicate curriculum finding")
		}
		seen[k] = true
		switch f.Aspect {
		case "definition", "outcomes", "criteria":
			if f.RelatedConceptID != "" {
				return nil, "", 0, 0, fmt.Errorf("unexpected related concept")
			}
		case "prerequisite":
			if !concepts[f.RelatedConceptID] || f.RelatedConceptID == f.ConceptID || (!expected[k] && f.Judgment != "needs_revision") {
				return nil, "", 0, 0, fmt.Errorf("invalid prerequisite finding")
			}
		default:
			return nil, "", 0, 0, fmt.Errorf("unknown review aspect")
		}
		if expected[k] && f.Judgment != "not_assessed" {
			reviewed++
		}
		needsRevision = needsRevision || f.Judgment == "needs_revision"
	}
	disposition := "partial_opinion"
	if reviewed == len(expected) {
		disposition = "complete_opinion"
	}
	if needsRevision {
		disposition = "changes_requested"
	}
	sort.Slice(findings, func(i, j int) bool {
		return key(findings[i].ConceptID, findings[i].Aspect, findings[i].RelatedConceptID) < key(findings[j].ConceptID, findings[j].Aspect, findings[j].RelatedConceptID)
	})
	return findings, disposition, reviewed, len(expected), nil
}
