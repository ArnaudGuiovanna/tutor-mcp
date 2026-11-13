// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"tutor-mcp/models"
)

type GraphQuality string

const (
	GraphQualityOK       GraphQuality = "ok"
	GraphQualityWarning  GraphQuality = "warning"
	GraphQualityCritical GraphQuality = "critical"
)

type GraphIssueSeverity string

const (
	GraphIssueWarning  GraphIssueSeverity = "warning"
	GraphIssueCritical GraphIssueSeverity = "critical"
)

type GraphIssue struct {
	Type       string             `json:"type"`
	Severity   GraphIssueSeverity `json:"severity"`
	Concepts   []string           `json:"concepts,omitempty"`
	Message    string             `json:"message"`
	Suggestion string             `json:"suggestion,omitempty"`
}

type GraphQualityMetrics struct {
	ConceptCount   int     `json:"concept_count"`
	EdgeCount      int     `json:"edge_count"`
	RootCount      int     `json:"root_count"`
	LeafCount      int     `json:"leaf_count"`
	MaxDepth       int     `json:"max_depth"`
	ComponentCount int     `json:"component_count"`
	IsolatedCount  int     `json:"isolated_count"`
	RootRatio      float64 `json:"root_ratio"`
}

type GraphQualityReport struct {
	Quality            GraphQuality        `json:"quality"`
	Metrics            GraphQualityMetrics `json:"metrics"`
	Issues             []GraphIssue        `json:"issues"`
	LLMRepairPrompt    string              `json:"llm_repair_prompt,omitempty"`
	ShouldAskLLMReview bool                `json:"should_ask_llm_review"`
}

func (r GraphQualityReport) HasCriticalIssues() bool {
	for _, issue := range r.Issues {
		if issue.Severity == GraphIssueCritical {
			return true
		}
	}
	return false
}

// EvaluateGraphQuality performs a deterministic structural audit of a domain
// graph. It never calls an LLM; the LLMRepairPrompt is only guidance for the
// coaching layer to propose human-readable fixes from the audited facts.
func EvaluateGraphQuality(graph models.KnowledgeSpace) GraphQualityReport {
	concepts := append([]string(nil), graph.Concepts...)
	prereqs := graph.Prerequisites
	if prereqs == nil {
		prereqs = map[string][]string{}
	}

	report := GraphQualityReport{}
	conceptSet := map[string]bool{}
	dependents := map[string][]string{}
	validPrereqCount := map[string]int{}
	connected := map[string]map[string]bool{}

	for _, concept := range concepts {
		if connected[concept] == nil {
			connected[concept] = map[string]bool{}
		}
		if conceptSet[concept] {
			report.Issues = append(report.Issues, graphIssue(
				"duplicate_concept",
				GraphIssueCritical,
				[]string{concept},
				fmt.Sprintf("duplicate concept %q", concept),
				"Keep one canonical concept name and migrate prerequisites to it.",
			))
		}
		conceptSet[concept] = true
	}

	report.Issues = append(report.Issues, normalizedDuplicateIssues(concepts)...)
	report.Issues = append(report.Issues, vagueConceptIssues(concepts)...)

	edgeCount := 0
	for concept, ps := range prereqs {
		if !conceptSet[concept] {
			report.Issues = append(report.Issues, graphIssue(
				"unknown_prerequisite_key",
				GraphIssueCritical,
				[]string{concept},
				fmt.Sprintf("prerequisite entry %q references a concept not present in concepts", concept),
				"Add the concept to the graph or remove this prerequisite entry.",
			))
			continue
		}
		if len(ps) > 5 {
			report.Issues = append(report.Issues, graphIssue(
				"too_many_prerequisites",
				GraphIssueWarning,
				[]string{concept},
				fmt.Sprintf("concept %q has %d prerequisites", concept, len(ps)),
				"Consider splitting the concept or keeping only the prerequisites that truly block progress.",
			))
		}
		for _, prereq := range ps {
			if !conceptSet[prereq] {
				report.Issues = append(report.Issues, graphIssue(
					"unknown_prerequisite",
					GraphIssueCritical,
					[]string{concept, prereq},
					fmt.Sprintf("concept %q depends on unknown prerequisite %q", concept, prereq),
					"Add the prerequisite as a concept or remove the edge.",
				))
				continue
			}
			if prereq == concept {
				report.Issues = append(report.Issues, graphIssue(
					"self_loop",
					GraphIssueCritical,
					[]string{concept},
					fmt.Sprintf("concept %q depends on itself", concept),
					"Remove the self dependency.",
				))
				continue
			}
			edgeCount++
			validPrereqCount[concept]++
			dependents[prereq] = append(dependents[prereq], concept)
			addUndirectedEdge(connected, concept, prereq)
		}
	}

	if cycle := graphQualityCycle(concepts, prereqs, conceptSet); len(cycle) > 0 {
		report.Issues = append(report.Issues, graphIssue(
			"cycle",
			GraphIssueCritical,
			cycle,
			"prerequisite graph contains a cycle: "+strings.Join(cycle, " -> "),
			"Break the loop by removing or reversing the weakest prerequisite edge.",
		))
	}

	metrics := graphQualityMetrics(concepts, edgeCount, validPrereqCount, dependents, connected)
	report.Metrics = metrics
	report.Issues = append(report.Issues, topologyWarningIssues(metrics)...)
	report.Quality = graphQualityFromIssues(report.Issues)
	report.ShouldAskLLMReview = report.Quality != GraphQualityOK
	if report.ShouldAskLLMReview {
		report.LLMRepairPrompt = graphQualityLLMRepairPrompt(report)
	}
	return report
}

func graphIssue(issueType string, severity GraphIssueSeverity, concepts []string, message, suggestion string) GraphIssue {
	concepts = append([]string(nil), concepts...)
	sort.Strings(concepts)
	return GraphIssue{
		Type:       issueType,
		Severity:   severity,
		Concepts:   concepts,
		Message:    message,
		Suggestion: suggestion,
	}
}

func normalizedDuplicateIssues(concepts []string) []GraphIssue {
	byNorm := map[string][]string{}
	for _, concept := range concepts {
		if norm := compactConceptKey(concept); norm != "" {
			byNorm[norm] = append(byNorm[norm], concept)
		}
	}
	var keys []string
	for key, values := range byNorm {
		unique := uniqueStrings(values)
		if len(unique) > 1 {
			byNorm[key] = unique
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	var issues []GraphIssue
	for _, key := range keys {
		concepts := byNorm[key]
		issues = append(issues, graphIssue(
			"near_duplicate_concept",
			GraphIssueWarning,
			concepts,
			fmt.Sprintf("concepts look equivalent after normalization: %s", strings.Join(concepts, ", ")),
			"Ask the learner whether these should be merged or renamed with clearer scope.",
		))
	}
	return issues
}

func vagueConceptIssues(concepts []string) []GraphIssue {
	vague := map[string]bool{
		"advanced": true, "basics": true, "general": true, "intro": true,
		"introduction": true, "misc": true, "other": true, "overview": true,
		"practice": true, "project": true, "stuff": true,
	}
	var issues []GraphIssue
	for _, concept := range concepts {
		key := strings.ToLower(strings.TrimSpace(concept))
		if vague[key] {
			issues = append(issues, graphIssue(
				"vague_concept",
				GraphIssueWarning,
				[]string{concept},
				fmt.Sprintf("concept %q is too vague to support reliable mastery tracking", concept),
				"Rename it as a specific observable skill.",
			))
			continue
		}
		if len([]rune(strings.TrimSpace(concept))) < 3 {
			issues = append(issues, graphIssue(
				"underspecified_concept",
				GraphIssueWarning,
				[]string{concept},
				fmt.Sprintf("concept %q is very short and may be underspecified", concept),
				"Use a more descriptive concept name.",
			))
		}
	}
	return issues
}

func graphQualityCycle(concepts []string, prereqs map[string][]string, conceptSet map[string]bool) []string {
	color := map[string]int{}
	parent := map[string]string{}
	adj := map[string][]string{}
	for dependent, ps := range prereqs {
		if !conceptSet[dependent] {
			continue
		}
		for _, prereq := range ps {
			if !conceptSet[prereq] || prereq == dependent {
				continue
			}
			adj[prereq] = append(adj[prereq], dependent)
		}
	}
	for node := range adj {
		sort.Strings(adj[node])
	}
	ordered := append([]string(nil), concepts...)
	sort.Strings(ordered)

	var cycleStart, cycleEnd string
	var visit func(string) bool
	visit = func(node string) bool {
		color[node] = 1
		for _, child := range adj[node] {
			switch color[child] {
			case 0:
				parent[child] = node
				if visit(child) {
					return true
				}
			case 1:
				cycleStart = child
				cycleEnd = node
				return true
			}
		}
		color[node] = 2
		return false
	}

	for _, concept := range ordered {
		if color[concept] == 0 && visit(concept) {
			break
		}
	}
	if cycleStart == "" {
		return nil
	}
	path := []string{cycleEnd}
	for node := cycleEnd; node != cycleStart; {
		p, ok := parent[node]
		if !ok {
			return []string{cycleStart, cycleEnd, cycleStart}
		}
		node = p
		path = append(path, node)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return append(path, cycleStart)
}

func graphQualityMetrics(
	concepts []string,
	edgeCount int,
	prereqCount map[string]int,
	dependents map[string][]string,
	connected map[string]map[string]bool,
) GraphQualityMetrics {
	roots := 0
	leaves := 0
	isolated := 0
	for _, concept := range concepts {
		if prereqCount[concept] == 0 {
			roots++
		}
		if len(dependents[concept]) == 0 {
			leaves++
		}
		if prereqCount[concept] == 0 && len(dependents[concept]) == 0 {
			isolated++
		}
	}
	componentCount := graphComponentCount(concepts, connected)
	maxDepth := graphMaxDepth(concepts, prereqCount, dependents)
	rootRatio := 0.0
	if len(concepts) > 0 {
		rootRatio = float64(roots) / float64(len(concepts))
	}
	return GraphQualityMetrics{
		ConceptCount:   len(concepts),
		EdgeCount:      edgeCount,
		RootCount:      roots,
		LeafCount:      leaves,
		MaxDepth:       maxDepth,
		ComponentCount: componentCount,
		IsolatedCount:  isolated,
		RootRatio:      rootRatio,
	}
}

func graphComponentCount(concepts []string, connected map[string]map[string]bool) int {
	seen := map[string]bool{}
	count := 0
	for _, concept := range concepts {
		if seen[concept] {
			continue
		}
		count++
		stack := []string{concept}
		seen[concept] = true
		for len(stack) > 0 {
			node := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for next := range connected[node] {
				if !seen[next] {
					seen[next] = true
					stack = append(stack, next)
				}
			}
		}
	}
	return count
}

func graphMaxDepth(concepts []string, prereqCount map[string]int, dependents map[string][]string) int {
	depth := map[string]int{}
	queue := make([]string, 0, len(concepts))
	for _, concept := range concepts {
		if prereqCount[concept] == 0 {
			depth[concept] = 1
			queue = append(queue, concept)
		}
	}
	sort.Strings(queue)
	maxDepth := 0
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if depth[node] > maxDepth {
			maxDepth = depth[node]
		}
		children := append([]string(nil), dependents[node]...)
		sort.Strings(children)
		for _, child := range children {
			if depth[child] < depth[node]+1 {
				depth[child] = depth[node] + 1
			}
			prereqCount[child]--
			if prereqCount[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	return maxDepth
}

func topologyWarningIssues(metrics GraphQualityMetrics) []GraphIssue {
	if metrics.ConceptCount < 4 {
		return nil
	}
	var issues []GraphIssue
	if metrics.EdgeCount == 0 {
		issues = append(issues, graphIssue(
			"graph_too_flat",
			GraphIssueWarning,
			nil,
			"graph has no prerequisite edges",
			"Ask the LLM to identify the minimum prerequisite order between concepts.",
		))
	} else if metrics.RootRatio >= 0.75 {
		issues = append(issues, graphIssue(
			"too_many_roots",
			GraphIssueWarning,
			nil,
			fmt.Sprintf("%.0f%% of concepts have no prerequisites", metrics.RootRatio*100),
			"Check whether some roots should depend on more foundational concepts.",
		))
	}
	if metrics.ComponentCount > 1 {
		issues = append(issues, graphIssue(
			"disconnected_graph",
			GraphIssueWarning,
			nil,
			fmt.Sprintf("graph has %d disconnected components", metrics.ComponentCount),
			"Ask whether the domain should be split or bridged with missing prerequisite links.",
		))
	}
	if metrics.IsolatedCount > 0 {
		issues = append(issues, graphIssue(
			"isolated_concepts",
			GraphIssueWarning,
			nil,
			fmt.Sprintf("graph has %d isolated concepts", metrics.IsolatedCount),
			"Connect isolated concepts or move them to a separate domain.",
		))
	}
	if metrics.MaxDepth > 8 {
		issues = append(issues, graphIssue(
			"graph_too_deep",
			GraphIssueWarning,
			nil,
			fmt.Sprintf("graph depth is %d", metrics.MaxDepth),
			"Check whether the chain should be compressed into milestones.",
		))
	}
	return issues
}

func graphQualityFromIssues(issues []GraphIssue) GraphQuality {
	quality := GraphQualityOK
	for _, issue := range issues {
		switch issue.Severity {
		case GraphIssueCritical:
			return GraphQualityCritical
		case GraphIssueWarning:
			quality = GraphQualityWarning
		}
	}
	return quality
}

func graphQualityLLMRepairPrompt(report GraphQualityReport) string {
	return fmt.Sprintf(
		"Use graph_quality_report to propose a concise graph repair plan. Do not invent learner progress. For critical issues, propose the smallest structural fix before continuing. For warnings, suggest merges, renames, splits, or prerequisite edges and ask for confirmation before mutating the domain. Metrics: concepts=%d edges=%d roots=%d components=%d.",
		report.Metrics.ConceptCount,
		report.Metrics.EdgeCount,
		report.Metrics.RootCount,
		report.Metrics.ComponentCount,
	)
}

func addUndirectedEdge(connected map[string]map[string]bool, a, b string) {
	if connected[a] == nil {
		connected[a] = map[string]bool{}
	}
	if connected[b] == nil {
		connected[b] = map[string]bool{}
	}
	connected[a][b] = true
	connected[b][a] = true
}

func compactConceptKey(concept string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(concept)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
