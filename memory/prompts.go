// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

const SessionSummaryTemplate = `You have just closed a session with the learner. Generate a structured summary
and call update_learner_memory to store it.

Required summary format, including YAML frontmatter:

---
session_id: {session_id}
domain_id: {domain_id}
timestamp: {session start time in ISO 8601}
duration_minutes: {approximate duration}
affect_start: {focused|distracted|tired|energized|frustrated|...}
affect_end: {satisfied|partial|stuck|breakthrough|...}
energy_level: {high|medium|low}
concepts_touched: ["concept1", "concept2"]
session_type: {first_pass|deep_dive|review|debug|consolidation}
novelty_flag: {true if a new breakthrough or blocker emerged, false otherwise}
---

## Summary
[1-2 factual sentences.]

## Salient exchanges
[2-3 verbatim or semi-verbatim moments that reveal reasoning.]

## Mental model observations
[Hypotheses about how the learner conceptualizes the material. Do not invent.
Mark as "to verify" when uncertain.]

## Implementation intention
[Explicit learner commitment for the next session.
If none was stated: "No explicit intention collected."]

Calls to perform:

update_learner_memory(scope="session", session_id="{session_id}", domain_id="{domain_id}", timestamp="{ISO}", operation="replace_file", content="<complete markdown>")

If there is a durable concept-level observation:
update_learner_memory(scope="concept", domain_id="{domain_id}", concept_slug="{slug}", operation="replace_section", section_key="Current state", content="...")

If a new fact may be durable but is not yet confirmed:
update_learner_memory(scope="memory_pending", operation="append", content="- {date}: {dated factual observation}")

DO NOT write directly to scope='memory' unless the learner explicitly stated a
clearly durable fact.`

const ConsolidationTemplate = `You receive multiple learner traces for {learner_id} over the period {start} - {end}.

### Current neocortical state (PRESERVE unless there is strong contradiction)

#### MEMORY.md
{current content}

#### Concepts touched
{current content of each concepts/{c}.md for every concept appearing in the period}

#### Pending observations (to arbitrate during this consolidation)
{current content of MEMORY_pending.md}

### Hippocampal episodes for the period
{full content of each sessions/*.md for the period, including frontmatter}

### Older interleaved episodes (random replay for cross-period coherence)
{1-3 older sessions selected randomly, full content}

---

Mission:

(a) Produce archives/{period}.md using this structure:

    ## Period trajectory
    [1-2 paragraphs about observable evolution]

    ## Consolidated concepts
    - {concept}: {state at the start -> state at the end, with evidence}

    ## Resolved misconceptions
    - {misconception -> how it was resolved}

    ## Still-active misconceptions
    - {misconception -> frequency and context}

    ## Emerging patterns
    [Strategies, recurring blockers, and pedagogical preferences observed repeatedly]

    ## Notable verbatims (max 3)
    - [{date}] "{short quote}"

(b) Patch MEMORY.md: integrate ONLY elements confirmed by multiple sessions OR
    explicitly stated by the learner as durable. Preserve existing sections
    unless there is strong contradiction. Format: explicit diff.

(c) Patch concepts/{c}.md for each touched concept.
    Format: for each concept, provide the new content of the "Current state" section.

(d) Arbitrate MEMORY_pending.md:
    - Confirmed by a second occurrence -> promote to MEMORY.md
    - Still isolated -> keep in pending
    - Contradicted -> remove from pending

(e) Emit the corresponding update_learner_memory calls.

Absolute constraints:
- Do not invent.
- Do not rewrite MEMORY.md from scratch - always patch.
- Preserve important verbatims.
- If uncertain, keep the item in pending.`

const ReasoningRequestTask = "Before generating the activity, write a 2-3 sentence hypothesis about the learner's current cognitive state. Use the distributed traces (learner_memory, concept_notes, recent_sessions, recent_archives) for pattern completion. Identify one likely tension point or confusion. Adapt the exercise to that hypothesis instead of generating a generic exercise."

var ReasoningRequestConstraints = []string{
	"do not invent facts that are absent from the provided sources",
	"state hypotheses, not certainties",
	"stay brief: 2-3 sentences",
	"if a contradiction appears between the OLM and memory chunks, mention it explicitly",
	"if focus_concept appears poorly chosen given the episodic chunks, propose learning_negotiation instead of generating the exercise",
	"if pending_memory contains a relevant observation, mention it without treating it as established",
	"if the OLM counts a concept as 'solid' but a FORGETTING-Critical alert identifies it as forgotten, prioritize the forgetting signal - the OLM counter is misleading",
	"if multiple FORGETTING-Critical alerts exist, identify the lowest retention instead of following the order in olm.focus_concept",
	"consult olm_inconsistencies first to identify divergences to compensate",
}

const EpisodicContextInstruction = `You receive multiple distributed traces for the learner:
- Stable state (neocortex): learner_memory + concept_notes
- Pending state: pending_memory
- Recent episodes (hippocampus): recent_sessions with frontmatter and body
- Medium-term trajectory: recent_archives
- Detected OLM inconsistencies: olm_inconsistencies (to compensate at consumption time)

Perform pattern completion: reconstruct the learner's current cognitive state from
the distributed cues. Use session frontmatter to distinguish contexts (affect,
energy, session type). Identify what is stable, emerging, and contradictory.

If olm_inconsistencies is non-empty, handle those signals carefully: the OLM can be
misleading on those specific points. The textual details in episodic chunks take
priority over OLM counters when they conflict.

This reconstruction feeds your interpretation_brief. Do not invent. If a trace is
ambiguous, say so.`
