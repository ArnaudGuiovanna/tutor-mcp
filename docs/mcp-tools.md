# MCP tools

[Back to README](../README.md#documentation) · [Architecture](architecture.md) · [Algorithms](algorithms.md)

In local mode Tutor MCP exposes 57 tools with the default feature flags: 41
learning tools and 16 training tools. The two goal-relevance tools are hidden
when `REGULATION_GOAL=off`; institution profiles add the formation, progress and
badge tools. The connected server's `tools/list` response is the reference for
every schema: each property has a description and every closed vocabulary is a
JSON Schema `enum`.

## Contents

- [Calling conventions](#calling-conventions)
- [Learning loop](#learning-loop-8)
- [Learner state](#learner-state-1)
- [Domains and curriculum](#domains-and-curriculum-10)
- [Metacognition](#metacognition-5)
- [Audit and replay](#audit-and-replay-3)
- [Transfer and negotiation](#transfer-and-negotiation-3)
- [Memory and session](#memory-and-session-9)
- [Availability](#availability-2)
- [Trainings](#trainings-16)
- [Alert engine](#alert-engine)

## Calling conventions

- **Domains.** Domain-scoped tools take `domain_id`; where documented, omitting
  it selects the most recently active non-archived domain. A bound native
  conversation fixes the domain of its selected training.
- **Trainings.** Training tools identify a programme with `training_id`
  (`personal:<domain_id>` or `formation:<id>`).
- **Retries.** Every mutation takes `idempotency_key` (the equivalent
  `_meta.idempotency_key` is also accepted). Reusing a key with canonically
  equivalent arguments replays the first successful response; reusing it with
  different arguments is rejected. Training mutations require the key.
- **Optimistic concurrency.** Writers that protect a version take
  `expected_revision`: the curriculum `version` from `get_curriculum_snapshot`
  (`add_concepts`, `publish_curriculum_revision`), the policy `version` from
  `get_availability_model` (`update_availability_model`), the training
  `revision` from `get_training`, or the conversation revision
  (`prepare_pedagogical_turn`). A stale value is rejected.
- **Structured inputs.** Structured values are JSON objects, never JSON-encoded
  strings: `rubric` (prepare_assessment_attempt, record_interaction),
  `rubric_score`, `semantic_observation`, `evaluation_provenance`, the training
  `document` and its `metadata` members.
- **Actions.** Tools that group the transitions of one resource select them
  with an `action` enum (`manage_domain`, `edit_training`,
  `manage_training_lifecycle`); read-only previews use `action=preview` or
  `preview=true` and need only a read grant.
- **Results.** Successful results return the JSON payload both as
  `structuredContent` and as a serialized text block, as the MCP specification
  recommends for compatibility. Correctable rejections name the problem.

The `initialize` result carries short `instructions` describing the learning
loop and the `idempotency_key` rule; the `tutor_mcp` prompt holds the full
protocol. Neither repeats the tool reference: `tools/list` is the reference.

Decision-bound assessment rubric criterion IDs are opaque and case-sensitive.
Copy a curriculum criterion ID exactly, including its upper-case letters,
underscores and hyphens. The frozen rubric and each later score use the same
exact ID. Duplicate IDs, unknown scoring criteria and scores contradicting the
frozen rubric are errors.

## Learning loop (8)

| Tool | Purpose |
|---|---|
| `start_learning_session` | Idempotently open/resume the durable session ID shared by interactions, affect, transfer, intentions, assessments and summaries |
| `prepare_pedagogical_turn` | Main loop tool. `mode=context` (read-only): profile, accessibility, focused curriculum, difficulties, recent observations, session and narrative memory. `mode=activity`: also opens the session and freezes the canonical engine decision in `decision` (activity, `pedagogical_contract`, tutor mode, motivation brief, metacognitive mirror, misconceptions, mastery evidence and uncertainty, transfer profile), with alert-aware routing. Without a conversation the domain comes from `domain_id`, `domain_name` or the last active domain; bound conversations select trainings with `training_id`/`training_name` |
| `prepare_assessment_attempt` / `submit_assessment_attempt` / `cancel_assessment_attempt` | Freeze task and `rubric` object before the response, commit the response before evaluation, or cancel the attempt |
| `record_interaction` | Persist an observation and update BKT/FSRS; unlinked practice stays explicitly unverified, while retention/demonstration/transfer evidence references a submitted attempt graded with `rubric_score`. An attempt-linked diagnostic also applies the adaptive placement estimates |
| `record_learning_event` | Record delivered feedback/instruction separately from responses, with an idempotent event key |
| `check_mastery` | Mastery-challenge readiness: BKT + evidence diversity + uncertainty + transfer status |

`pedagogical_contract.reasoning_request`, when present, lists only why it is
enabled (`enabled_because`); the fixed task (a 2-3 sentence
`interpretation_brief` written before the activity) is documented once in the
server instructions and the prompt. `decision_id` lives in
`decision.pedagogical_contract.decision_id`.

## Learner state (1)

| Tool | Purpose |
|---|---|
| `get_learner_state` | Read-only views. `view=overview` (default): session-start context, domains, priority concept, opening message and progress narrative. `view=progress`: evidence-backed progress per concept, retention alerts, the full pending-alert list with `has_critical`, trajectory signal, `autonomy`, calibration and affect. `view=model`: open learner model of one domain. `view=model_all_domains`: the same aggregated across domains |

## Domains and curriculum (10)

| Tool | Purpose |
|---|---|
| `init_domain` | Create a domain with concept graph, prerequisites, personal goal and immutable curriculum version 1 |
| `add_concepts` | Append concepts with `expected_revision` CAS; optional outcomes/level/criteria metadata; progress is not reset |
| `get_curriculum_snapshot` | Read latest/historical immutable versions, stable concept IDs, outcomes, criteria, provenance and review state |
| `publish_curriculum_revision` | CAS-protected rename, definition update, split, merge, safe removal or explicit prerequisite repair |
| `validate_domain_graph` | Audit graph: cycles, orphans, depth, disconnections |
| `manage_domain` | `action=archive`, `unarchive` or `delete` (`confirm=true`); deletion is a runtime-hidden tombstone that preserves curriculum and learning evidence |
| `set_domain_priority` | Re-rank domains for scheduling weight |
| `mark_domain_high_stakes` | One-way safety classification; demonstrated claims and intrusive suggestions then require trusted human-reviewed evaluation |
| `set_goal_relevance` / `get_goal_relevance` | LLM-decomposed relevance vector over the concept graph (biases the concept selector), hidden by `REGULATION_GOAL=off` |

## Metacognition (5)

| Tool | Purpose |
|---|---|
| `record_affect` | Energy + confidence (start), satisfaction + difficulty + intent (end) |
| `calibration_check` / `record_calibration_result` | Self-prediction (1-5) + bias update |
| `get_autonomy_metrics` | Descriptive learning rates, prediction accuracy and observation counts |
| `update_learner_profile` | Persist learner-declared metadata (objective, language, device, affect baseline) |

The metacognitive mirror is part of every activity decision
(`decision.metacognitive_mirror`).

## Audit and replay (3)

| Tool | Purpose |
|---|---|
| `get_pedagogical_snapshots` | Before / observation / after / decision trace |
| `get_decision_replay_summary` | Offline audit: replay coverage, missing rubrics, transfer gaps, JSON issues |
| `get_misconceptions` | Per-concept misconceptions with status (active / resolved) and frequency |

## Transfer and negotiation (3)

| Tool | Purpose |
|---|---|
| `feynman_challenge` | Learner explains a high-estimate concept; confirmed gaps become versioned curriculum revisions |
| `transfer_challenge` | Generate structured probes across `near`/`far`/`debugging`/`teaching`/`creative`; evidence is recorded through an assessment attempt and `record_interaction(TRANSFER_PROBE)` |
| `learning_negotiation` | Expose system plan + tradeoffs; the learner can propose alternatives |

## Memory and session (9)

| Tool | Purpose |
|---|---|
| `update_learner_memory` / `read_raw_session` / `get_memory_state` | Narrative memory: sessions, concepts, stable memory and archives |
| `claim_memory_maintenance` / `commit_memory_maintenance` | Background, bounded memory consolidation; never a prerequisite for a learner response |
| `record_session_close` | Idempotently close the durable session + recap brief + optional implementation intention |
| `list_implementation_intentions` / `update_implementation_intention` | Inspect and resolve commitments (pending/honored/missed/cancelled) |
| `queue_webhook_message` | Queue a structured Discord nudge (`why_now`, `learning_gain`, `open_loop`, `next_action`) |

## Availability (2)

| Tool | Purpose |
|---|---|
| `get_availability_model` | Learner-owned IANA timezone, weekly local windows, DND, consent/frequency/cap, accessibility preferences and policy version |
| `update_availability_model` | Optimistic (`expected_revision`) replacement of availability/accessibility policy |

## Trainings (16)

| Tool | Purpose |
|---|---|
| `list_training_library` / `get_training` | Unified, deduplicated library and one programme with progress and `can_*` capabilities |
| `get_conversation_context` / `select_conversation_training` | Confirmed training of a native conversation and its explicit switch (`expected_selection`) |
| `edit_training` | `action=preview` (read-only consequences), `propose` (proposal for human review) or `save` (apply) a complete `document` against `expected_revision` |
| `manage_training_lifecycle` | `action=archive`, `restore`, `delete` or `withdraw`; `preview=true` reports the effects, applying needs `confirm=true` and `expected_revision` |
| `share_training` / `submit_training` / `decide_training` / `publish_training` | Contribution, review and publication of common programmes |
| `set_training_access` / `find_training_members` | Audience, invitations and editor rights |
| `get_training_policy` / `set_training_policy` | Personal-learning policy of the institution |
| `join_training` | Explicit enrollment respecting the access mode |
| `get_training_groups` | Real cohorts assigned to a trainer (`progress:read`) |

`edit_training` and `share_training` advertise only the parts of `document` the
server reads: `curriculum` for a personal training, `modules` and `concepts` for
an institutional one, and `training.description` for a new institutional
programme. Outcome and criterion shapes are shared through `$defs`.
Server-computed fields returned by `get_training` may be sent back and are
ignored.

## Alert engine

The scheduler uses learning and metacognitive alerts. `MASTERY_READY` means that attempt-linked retained, varied evidence is sufficient to *attempt* a challenge; it is not a demonstrated-mastery claim, and recent trusted transfer failure suppresses the alert. Every delivery rechecks consent, DND, local time windows and frequency caps. Unreviewed high-stakes domains cannot produce demonstrated claims or intrusive suggestions; only trusted `human_review` evidence opens that gate, and the runtime does not invent an external reviewer.
