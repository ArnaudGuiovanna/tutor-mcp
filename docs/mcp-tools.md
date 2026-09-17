# MCP tools

[Back to README](../README.md#documentation) · [Architecture](architecture.md) · [Algorithms](algorithms.md)

Tutor MCP exposes 46 learning tools with the default feature flags.
The two goal-relevance tools are hidden when `REGULATION_GOAL=off`.
The connected server's `tools/list` response supplies the current schemas.

## Contents

- [Core learning loop](#core-learning-loop-12)
- [Domain management](#domain-management-12)
- [Metacognition](#metacognition-6)
- [Audit and replay](#audit--replay-3)
- [Transfer and negotiation](#transfer--negotiation-4)
- [Memory and session](#memory--session-7)
- [Availability](#availability-2)
- [Alert engine](#alert-engine)

## Calling conventions

Domain-scoped learning tools accept an optional `domain_id`; where documented, omitting it selects the most recently active non-archived domain. Learner-global and lifecycle tools intentionally have different contracts—use each tool's schema as the source of truth.

Every mutation schema exposes an optional `idempotency_key` (the equivalent
`_meta.idempotency_key` is also accepted). Reusing the same learner/tool/key
with canonically equivalent arguments replays the first successful response; reusing
it with different arguments is rejected. Hosts should generate a fresh key for
each logical mutation and retain it across transport retries. If an operator
opts into cached-response retention, an expired response produces an explicit
already-completed error; the durable key and request hash remain, so the tool
handler is never executed again for that key.

## Core learning loop (12)

| Tool | Purpose |
|---|---|
| `start_learning_session` | Idempotently open/resume the durable session ID shared by interactions, affect, transfer, intentions, assessments and summaries |
| `get_learner_context` | Session-start context: active domain, concept states, recent history, active misconceptions |
| `get_pending_alerts` | Learning + metacognitive alerts requiring action |
| `get_next_activity` | Next recommended activity + episodic context + reasoning request + tutor mode + motivation brief + mastery uncertainty + transfer profile |
| `prepare_assessment_attempt` / `submit_assessment_attempt` / `cancel_assessment_attempt` | Freeze task/rubric before the response, commit the response before evaluation, or explicitly cancel the attempt |
| `record_interaction` | Persist an observation and update BKT/FSRS; unlinked practice stays explicitly unverified, while retention/demonstration/transfer evidence references a submitted/evaluated attempt |
| `record_learning_event` | Record delivered feedback/instruction separately from responses; server time and idempotent event key, without awarding model progress |
| `check_mastery` | Mastery-challenge readiness: BKT + evidence diversity + uncertainty + transfer status |
| `get_olm_snapshot` | Open Learner Model: estimates and evidence per concept — retained, demonstrated and transferred |
| `get_dashboard_state` | Evidence-backed progress (estimated/retained/demonstrated/transferred), routing state, retention, autonomy, calibration bias and affect history |

## Domain management (12)

| Tool | Purpose |
|---|---|
| `init_domain` | Create domain with concept graph, prerequisites, personal goal, and immutable curriculum version 1 |
| `add_concepts` | Append concepts with `expected_version` CAS; optional outcomes/level/criteria metadata; progress is not reset |
| `get_curriculum_snapshot` | Read latest/historical immutable versions, stable concept IDs, outcomes, criteria, provenance and review state |
| `publish_curriculum_revision` | CAS-protected rename, definition update, split, merge, safe removal or explicit prerequisite repair. Changed definitions reset estimates and supersede old evidence for routing; historical records remain auditable. |
| `validate_domain_graph` | Audit graph: cycles, orphans, depth, disconnections |
| `archive_domain` / `unarchive_domain` / `delete_domain` | Lifecycle; deletion is a runtime-hidden tombstone that preserves curriculum and learning evidence |
| `set_domain_priority` | Re-rank domains for scheduling weight |
| `mark_domain_high_stakes` | One-way safety classification; demonstrated claims and intrusive suggestions then require trusted human-reviewed evaluation |
| `set_goal_relevance` / `get_goal_relevance` | LLM-decomposed relevance vector over the concept graph (biases the concept selector) — gated by `REGULATION_GOAL` |

## Metacognition (6)

| Tool | Purpose |
|---|---|
| `record_affect` | Energy + confidence (start), satisfaction + difficulty + intent (end) |
| `calibration_check` / `record_calibration_result` | Self-prediction (1–5) + bias update |
| `get_autonomy_metrics` | Descriptive learning rates, prediction accuracy and observation counts; the legacy summary score is not a validated autonomy measure |
| `get_metacognitive_mirror` | Observations and a reflection intent from recorded behavior over 3+ sessions, with evidence coverage for the LLM to explain |
| `update_learner_profile` | Persist learner metadata (objective, language, calibration bias, …) |

## Audit & replay (3)

| Tool | Purpose |
|---|---|
| `get_pedagogical_snapshots` | Before / observation / after / decision trace |
| `get_decision_replay_summary` | Offline audit: replay coverage, missing rubrics, transfer gaps, JSON issues |
| `get_misconceptions` | Per-concept misconceptions with status (active / resolved) and frequency |

## Transfer & negotiation (4)

| Tool | Purpose |
|---|---|
| `feynman_challenge` | Learner explains a high-estimate concept to deepen evidence; confirmed prerequisite gaps become versioned curriculum revisions |
| `transfer_challenge` / `record_transfer_result` | Generate structured probes across `near`/`far`/`debugging`/`teaching`/`creative`; the direct recorder is legacy/unverified, while evidence-bearing probes use assessment attempts |
| `learning_negotiation` | Expose system plan + tradeoffs; learner can propose alternatives |

## Memory & session (7)

| Tool | Purpose |
|---|---|
| `update_learner_memory` / `read_raw_session` / `get_memory_state` | Narrative memory: sessions, concepts, stable memory and archives; encrypted database storage in the new profiles |
| `record_session_close` | Idempotently close the durable session + recap brief + optional Gollwitzer if-then implementation intention |
| `list_implementation_intentions` / `update_implementation_intention` | Inspect and resolve commitments through pending/honored/missed/cancelled states |
| `queue_webhook_message` | Queue a structured Discord nudge (`why_now`, `learning_gain`, `open_loop`, `next_action`) |

## Availability (2)

| Tool | Purpose |
|---|---|
| `get_availability_model` | Learner-owned IANA timezone, weekly local windows, DND, consent/frequency/cap, accessibility preferences and policy version |
| `update_availability_model` | Optimistic, ownership-scoped replacement of availability/accessibility policy; concurrent stale writes are rejected |

## Alert engine

The scheduler uses learning and metacognitive alerts. PFA-based `PLATEAU` and composite-score `DEPENDENCY_INCREASING` producers have been removed; legacy records remain readable. `MASTERY_READY` means that attempt-linked retained, varied evidence is sufficient to *attempt* a challenge; it is not a demonstrated-mastery claim, and recent trusted transfer failure suppresses the alert. Every delivery rechecks consent, DND, local time windows and frequency caps. Unreviewed high-stakes domains cannot produce demonstrated claims or intrusive suggestions; only trusted `human_review` evidence opens that gate, and the runtime does not invent an external reviewer.
