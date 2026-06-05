// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"tutor-mcp/engine"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Input caps for domain-management tools. These bound the cost of a single
// MCP call and stop a misbehaving client from pushing arbitrarily large
// strings or graphs into SQLite.
const (
	maxDomainNameLen        = 200
	maxPersonalGoalLen      = 2000
	maxConceptNameLen       = 200
	maxConceptsPerCall      = 500
	maxPrereqEntriesPerNode = 20
	maxValueFramingLen      = 2000
)

// validateConcepts enforces the size caps on a concept list and its
// prerequisite graph, AND cross-checks that every prerequisite key/value
// references a concept declared in `concepts`. The `concepts` slice is the
// universe of valid concept names — for init_domain it is the user-supplied
// concepts[]; for add_concepts callers must pass the merged existing+new
// set so prereq arrows pointing at already-declared concepts remain valid.
//
// Without this cross-check a malformed graph silently locks concepts behind
// unknown prereqs (concept_selector treats missing prereqs as mastery=0
// forever — see engine/concept_selector.go).
func validateConcepts(concepts []string, prereqs map[string][]string) error {
	if len(concepts) > maxConceptsPerCall {
		return fmt.Errorf("too many concepts: %d (max %d)", len(concepts), maxConceptsPerCall)
	}
	for _, c := range concepts {
		if c == "" {
			return fmt.Errorf("empty concept name")
		}
		if len(c) > maxConceptNameLen {
			return fmt.Errorf("concept name too long (max %d chars)", maxConceptNameLen)
		}
	}
	if len(prereqs) > maxConceptsPerCall {
		return fmt.Errorf("too many prerequisite entries: %d (max %d)", len(prereqs), maxConceptsPerCall)
	}

	// Build the universe of declared concepts for cross-referencing.
	// Reject duplicates: a repeated concept inflates TotalGoalRelevant
	// and skews mastery ratios in the FSM observables (issue #27).
	universe := make(map[string]bool, len(concepts))
	for _, c := range concepts {
		if universe[c] {
			return fmt.Errorf("duplicate concept name %q", c)
		}
		universe[c] = true
	}

	for k, vs := range prereqs {
		if len(k) > maxConceptNameLen {
			return fmt.Errorf("prerequisite key too long (max %d chars)", maxConceptNameLen)
		}
		if !universe[k] {
			return fmt.Errorf("prerequisite %q references concept not declared in concepts[]", k)
		}
		if len(vs) > maxPrereqEntriesPerNode {
			return fmt.Errorf("too many prerequisites for %q (max %d)", k, maxPrereqEntriesPerNode)
		}
		for _, v := range vs {
			if len(v) > maxConceptNameLen {
				return fmt.Errorf("prerequisite value too long (max %d chars)", maxConceptNameLen)
			}
			if !universe[v] {
				return fmt.Errorf("prerequisite %q references concept not declared in concepts[]", v)
			}
		}
	}

	// Issue #62: the prerequisite graph MUST be a DAG. A cycle would make
	// algorithms/kst.go ComputeFrontier and the concept_selector loop —
	// every node in the cycle is `locked` because its prereq chain
	// transitively depends on itself, and learners can never make
	// progress. Reject up-front with a descriptive MCP error.
	if cycle := findPrereqCycle(concepts, prereqs); len(cycle) > 0 {
		return fmt.Errorf("prerequisite graph contains a cycle: %s", formatCycle(cycle))
	}
	return nil
}

// findPrereqCycle runs an iterative DFS with white/gray/black coloring on
// the prerequisite graph and, if it finds a back-edge, returns the nodes
// forming the cycle (in dependency order, with the closing node repeated
// at the end for clarity). Returns nil if the graph is a DAG.
//
// Edge convention: prereqs[c] = [p1, p2] encodes p1 -> c and p2 -> c
// (a prereq points at the concept it unlocks). We DFS in that direction
// so a discovered back-edge is reported as `pk -> ... -> pk`.
func findPrereqCycle(concepts []string, prereqs map[string][]string) []string {
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // fully explored
	)

	// Build the forward adjacency list (prereq -> dependent). Only
	// include nodes that appear in `concepts` to stay aligned with the
	// universe check above.
	adj := make(map[string][]string, len(concepts))
	for dependent, ps := range prereqs {
		for _, p := range ps {
			adj[p] = append(adj[p], dependent)
		}
	}

	color := make(map[string]int, len(concepts))
	parent := make(map[string]string, len(concepts))

	// Self-loops are degenerate cycles that DFS reports correctly, but
	// surfacing them explicitly keeps the error message readable.
	for dependent, ps := range prereqs {
		for _, p := range ps {
			if p == dependent {
				return []string{dependent, dependent}
			}
		}
	}

	var cycleStart, cycleEnd string
	var found bool

	// Iterative DFS; each stack frame holds the node and its next-child index.
	type frame struct {
		node string
		idx  int
	}

	for _, start := range concepts {
		if color[start] != white {
			continue
		}
		stack := []frame{{node: start, idx: 0}}
		color[start] = gray

		for len(stack) > 0 && !found {
			top := &stack[len(stack)-1]
			children := adj[top.node]
			if top.idx >= len(children) {
				color[top.node] = black
				stack = stack[:len(stack)-1]
				continue
			}
			child := children[top.idx]
			top.idx++

			switch color[child] {
			case white:
				color[child] = gray
				parent[child] = top.node
				stack = append(stack, frame{node: child, idx: 0})
			case gray:
				// Back-edge: top.node -> child closes a cycle starting at child.
				cycleStart = child
				cycleEnd = top.node
				found = true
			}
		}
		if found {
			break
		}
	}

	if !found {
		return nil
	}

	// Reconstruct the cycle by walking parents from cycleEnd back to cycleStart.
	path := []string{cycleEnd}
	for n := cycleEnd; n != cycleStart; {
		p, ok := parent[n]
		if !ok {
			break
		}
		n = p
		path = append(path, n)
	}
	// Reverse so we list nodes in dependency order, then close the loop.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	path = append(path, cycleStart)
	return path
}

// formatCycle renders a cycle path like "a -> b -> c -> a".
func formatCycle(cycle []string) string {
	if len(cycle) == 0 {
		return ""
	}
	out := cycle[0]
	for _, n := range cycle[1:] {
		out += " -> " + n
	}
	return out
}

func validateValueFramings(vf *ValueFramingsInput) error {
	if vf == nil {
		return nil
	}
	for _, f := range []string{vf.Financial, vf.Employment, vf.Intellectual, vf.Innovation} {
		if len(f) > maxValueFramingLen {
			return fmt.Errorf("value_framing too long (max %d chars)", maxValueFramingLen)
		}
	}
	return nil
}

type ValueFramingsInput struct {
	Financial    string `json:"financial,omitempty" jsonschema:"financial gain (1-2 sentences)"`
	Employment   string `json:"employment,omitempty" jsonschema:"employability / career gain (1-2 sentences)"`
	Intellectual string `json:"intellectual,omitempty" jsonschema:"intellectual / reasoning gain (1-2 sentences)"`
	Innovation   string `json:"innovation,omitempty" jsonschema:"creation / innovation gain (1-2 sentences)"`
}

type InitDomainParams struct {
	Name          string              `json:"name" jsonschema:"learning domain name"`
	Concepts      []string            `json:"concepts" jsonschema:"list of domain concepts"`
	Prerequisites map[string][]string `json:"prerequisites" jsonschema:"prerequisite graph (concept -> list of prerequisites)"`
	PersonalGoal  string              `json:"personal_goal,omitempty" jsonschema:"learner's personal goal within this domain (optional)"`
	ValueFramings *ValueFramingsInput `json:"value_framings,omitempty" jsonschema:"4 value axes (financial/employment/intellectual/innovation). 1-2 sentences per axis. Optional - can be filled in later."`
}

func registerInitDomain(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "init_domain",
		Description: "Initialize a learning domain with its concepts and prerequisites. Does not destroy existing progress.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params InitDomainParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "init_domain", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		if params.Name == "" {
			r, _ := errorResult("name is required")
			return r, nil, nil
		}
		if len(params.Name) > maxDomainNameLen {
			r, _ := errorResult(fmt.Sprintf("name too long (max %d chars)", maxDomainNameLen))
			return r, nil, nil
		}
		if len(params.PersonalGoal) > maxPersonalGoalLen {
			r, _ := errorResult(fmt.Sprintf("personal_goal too long (max %d chars)", maxPersonalGoalLen))
			return r, nil, nil
		}
		if len(params.Concepts) == 0 {
			r, _ := errorResult("at least one concept is required")
			return r, nil, nil
		}
		if err := validateConcepts(params.Concepts, params.Prerequisites); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateValueFramings(params.ValueFramings); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		graph := models.KnowledgeSpace{
			Concepts:      params.Concepts,
			Prerequisites: params.Prerequisites,
		}
		if graph.Prerequisites == nil {
			graph.Prerequisites = make(map[string][]string)
		}
		graphQualityReport := engine.EvaluateGraphQuality(graph)
		if graphQualityReport.HasCriticalIssues() {
			r, _ := graphQualityBlockedResult(graphQualityReport)
			return r, nil, nil
		}

		valueFramingsJSON := ""
		if params.ValueFramings != nil {
			vf := models.DomainValueFramings{
				Financial:    params.ValueFramings.Financial,
				Employment:   params.ValueFramings.Employment,
				Intellectual: params.ValueFramings.Intellectual,
				Innovation:   params.ValueFramings.Innovation,
			}
			if buf, merr := json.Marshal(vf); merr == nil {
				valueFramingsJSON = string(buf)
			}
		}

		domain, err := deps.Store.CreateDomainWithValueFramings(ctx, learnerID, params.Name, params.PersonalGoal, graph, valueFramingsJSON)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to create domain", err)
			return r, nil, nil
		}

		// Initialize ConceptState for each concept — INSERT OR IGNORE preserves existing progress
		for _, concept := range params.Concepts {
			cs := models.NewConceptState(learnerID, concept)
			if err := deps.Store.InsertConceptStateIfNotExists(ctx, cs); err != nil {
				r, _ := safeErrorResult(deps.Logger, fmt.Sprintf("failed to initialize concept %s", concept), err)
				return r, nil, nil
			}
		}

		// [2] PhaseController — initialises the domain in DIAGNOSTIC.
		// The concept_states were just created at PMastery=0.1 —
		// the entry entropy is now computable.
		states, _ := deps.Store.GetConceptStatesByLearner(ctx, learnerID)
		stateMap := map[string]*models.ConceptState{}
		for _, cs := range states {
			stateMap[cs.Concept] = cs
		}
		entryEntropy := engine.MeanBinaryEntropyOverGraph(domain.Graph, stateMap)
		if err := deps.Store.UpdateDomainPhase(ctx, domain.ID, models.PhaseDiagnostic, entryEntropy, time.Now().UTC()); err != nil {
			deps.Logger.Error("init_domain: failed to set initial phase",
				"err", err, "domain", domain.ID)
			// Non-fatal: domain stays in phase NULL → INSTRUCTION
			// fallback. Regulation continues to work.
		}

		response := map[string]interface{}{
			"domain_id":              domain.ID,
			"concept_count":          len(params.Concepts),
			"graph_quality_report":   graphQualityReport,
			"graph_quality_guidance": graphQualityGuidance(graphQualityReport),
			"message":                fmt.Sprintf("Domain %q was created with %d concepts. Existing progress was preserved.", params.Name, len(params.Concepts)),
		}
		// [1] GoalDecomposer — instruct the LLM (versioned, structured,
		// non-blocking per Q2). Only emitted when REGULATION_GOAL=on so
		// pre-flag clients see no behavioural change.
		if regulationGoalEnabled() {
			reason := fmt.Sprintf("Decompose the personal_goal against the %d concepts via set_goal_relevance to activate goal-aware routing.", len(params.Concepts))
			if params.PersonalGoal == "" {
				reason = "personal_goal is empty - set_goal_relevance remains optional; call it if you want to manually annotate relevance per concept."
			}
			response["next_action"] = map[string]any{
				"version":  1,
				"tool":     "set_goal_relevance",
				"reason":   reason,
				"required": false,
			}
		}
		r, _ := jsonResult(response)
		return r, nil, nil
	})
}

// ─── Add Concepts ────────────────────────────────────────────────────────────

type AddConceptsParams struct {
	DomainID      string              `json:"domain_id" jsonschema:"target domain ID"`
	Concepts      []string            `json:"concepts" jsonschema:"new concepts to add"`
	Prerequisites map[string][]string `json:"prerequisites" jsonschema:"new prerequisites (concept -> list of prerequisites). May include links to existing concepts."`
}

func registerAddConcepts(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_concepts",
		Description: "Add concepts to an existing domain without destroying progress. Use to enrich a domain mid-course.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params AddConceptsParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "add_concepts", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		if len(params.Concepts) == 0 {
			r, _ := errorResult("at least one concept is required")
			return r, nil, nil
		}

		// Resolve domain — needed so we can validate prerequisites
		// against the merged (existing + new) concept universe.
		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "domain not found", err)
			return r, nil, nil
		}

		// Reject duplicates BEFORE merging: (a) intra-batch duplicates and
		// (b) batch entries that already live in the existing graph. A
		// silent skip would let the caller believe the add succeeded while
		// inflating their TotalGoalRelevant view (issue #27).
		existingSet := make(map[string]bool)
		for _, c := range domain.Graph.Concepts {
			existingSet[c] = true
		}
		batchSet := make(map[string]bool, len(params.Concepts))
		for _, c := range params.Concepts {
			if existingSet[c] || batchSet[c] {
				r, _ := errorResult(fmt.Sprintf("duplicate concept name %q", c))
				return r, nil, nil
			}
			batchSet[c] = true
		}

		candidateGraph := models.KnowledgeSpace{
			Concepts:      append([]string(nil), domain.Graph.Concepts...),
			Prerequisites: copyPrerequisites(domain.Graph.Prerequisites),
		}
		added := 0
		for _, c := range params.Concepts {
			candidateGraph.Concepts = append(candidateGraph.Concepts, c)
			existingSet[c] = true
			added++
		}
		mergePrerequisites(candidateGraph.Prerequisites, params.Prerequisites)

		// Validate against the MERGED universe — prereqs may legitimately
		// reference concepts that already exist on the domain.
		if err := validateConcepts(candidateGraph.Concepts, candidateGraph.Prerequisites); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		graphQualityReport := engine.EvaluateGraphQuality(candidateGraph)
		if graphQualityReport.HasCriticalIssues() {
			r, _ := graphQualityBlockedResult(graphQualityReport)
			return r, nil, nil
		}
		domain.Graph = candidateGraph

		// Persist updated graph
		if err := deps.Store.UpdateDomainGraph(ctx, domain.ID, domain.Graph); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to update domain graph", err)
			return r, nil, nil
		}

		// Initialize concept states for new concepts only (INSERT OR IGNORE)
		for _, concept := range params.Concepts {
			cs := models.NewConceptState(learnerID, concept)
			if err := deps.Store.InsertConceptStateIfNotExists(ctx, cs); err != nil {
				r, _ := safeErrorResult(deps.Logger, fmt.Sprintf("failed to initialize concept %s", concept), err)
				return r, nil, nil
			}
		}

		response := map[string]interface{}{
			"domain_id":              domain.ID,
			"added":                  added,
			"total_concepts":         len(domain.Graph.Concepts),
			"graph_quality_report":   graphQualityReport,
			"graph_quality_guidance": graphQualityGuidance(graphQualityReport),
			"message":                fmt.Sprintf("%d new concepts added. Total: %d. Existing progress was preserved.", added, len(domain.Graph.Concepts)),
		}
		// [1] GoalDecomposer — after add_concepts the graph_version has
		// advanced; per OQ-1.1 existing relevance entries remain valid but
		// the new concepts are uncovered. The LLM is invited to top-up.
		if regulationGoalEnabled() && added > 0 {
			response["next_action"] = map[string]any{
				"version":  1,
				"tool":     "set_goal_relevance",
				"reason":   fmt.Sprintf("%d new concepts added; call set_goal_relevance with their scores to preserve goal-aware routing (semantics are incremental - existing concepts are not erased).", added),
				"required": false,
			}
		}
		r, _ := jsonResult(response)
		return r, nil, nil
	})
}
