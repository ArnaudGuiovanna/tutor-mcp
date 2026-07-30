# GOAP — Phase 0 · Reconnaissance

> **Archive historique — non normative.** Cette reconnaissance décrit le
> routeur qui existait au moment de l'étude. `engine/router.go` et
> `engine.Route` ont depuis été supprimés ; le runtime supporté utilise le
> pipeline permanent de `engine/orchestrator.go`. Le code, les schémas des
> outils MCP, `README.md` et `OPERATIONS.md` font foi.

> Read-only audit of the existing routing layer in `tutor-mcp`, performed
> before any GOAP design or implementation work. Scope: map what exists,
> identify boundaries, surface frictions with the GOAP-réactif idea.
> No code written outside this file.

## 1. Routing function — exact signature

**File:** `engine/router.go:17`

```go
func Route(
    alerts []models.Alert,
    frontier []string,
    states []*models.ConceptState,
    recentInteractions []*models.Interaction,
    sessionConcepts map[string]int,
) models.Activity
```

- **Pure function.** No store, no logger, no clock. Time-derived data
  (retention, session length) is precomputed by the caller.
- **Output:** a single `models.Activity` (one shot, no plan, no rationale
  beyond the `Rationale` string and `PromptForLLM`).
- **Algorithm:** linear priority scan with 7 hard-coded buckets:
  1. `FORGETTING` critical
  2. `ZPD_DRIFT`
  3. `PLATEAU` (with format rotation, skip if concept practiced ≥2× this session)
  4. `OVERLOAD` → REST
  5. `MASTERY_READY` (skip if practiced ≥1× this session)
  6. New concept from KST `frontier` (skip if already introduced this session)
  7. Default recall on lowest FSRS retention; falls back to REST when nothing eligible
- **Session dedup** is already partially in place via `sessionConcepts` —
  but only steps 3, 5 and 6 honour it; critical (1) and ZPD (2) bypass it
  by design.

### Non-test callers

```
tools/activity.go:101     activity := engine.Route(alerts, frontier, domainStates, domainInteractions, sessionConcepts)
tools/negotiation.go:92   systemActivity := engine.Route(alerts, frontier, domainStates, domainInteractions, sessionConcepts)
```

Two MCP tools depend on `Route`: `get_next_activity` and `learning_negotiation`.
Both build the same context (alerts, frontier, domain-filtered states and
interactions, session concept counts) before calling.

### Tests touching `Route`

12 tests across `engine/router_test.go` (4) and `engine/router_more_test.go` (8).
They exercise each priority bucket plus dedup edge cases. **All must pass on
the legacy path with the feature flag default (see §6 / migration).**

## 2. Implicit `WorldState` schema

These are the fields the runtime inspects today to decide "what next".
They are the candidate state variables for a GOAP `WorldState`.

### Per-concept (`models.ConceptState`)

From `models/learner.go:20`:

| Field | Algo origin | Used by |
|---|---|---|
| `PMastery`, `PLearn`, `PForget`, `PSlip`, `PGuess` | BKT | mastery gating, mastery_ready alert |
| `Stability`, `Difficulty`, `ElapsedDays`, `ScheduledDays`, `Reps`, `Lapses`, `LastReview`, `NextReview`, `CardState` | FSRS | retention computation, forgetting alert |
| `Theta` | IRT | predictive ZPD alert |
| `PFASuccesses`, `PFAFailures` | PFA | plateau detection |

### Per-domain (`models.Domain`)

From `models/domain.go:69`:

| Field | Role for routing |
|---|---|
| `Graph` (concepts + prerequisites) | KST frontier computation |
| `PersonalGoal` | Used by motivation (`why_this_exercise`), **not by the router**. Today it is a free-text string; no decomposition into goal vertices. |
| `PinnedConcept` | Learner-set focus override (used by OLM, `engine/olm.go:207` — not by `Route`) |
| `Archived` | Filter (router ignores archived domains via caller-side filter) |
| `ValueFramingsJSON`, `LastValueAxis` | Motivation only |

### Per-learner / metacognitive (`models.AffectState`, `AutonomyMetrics`)

`Route` itself does **not** read these. They are read by `tools/activity.go`
*after* `Route` returns to (a) compute `tutorMode`, which adjusts
`DifficultyTarget` and `EstimatedMinutes` post-hoc, and (b) compose the
`MotivationBrief`. So today they live in the wrapper layer, not the
routing layer.

| Field | Currently consumed by |
|---|---|
| `Energy`, `SubjectConfidence`, `Satisfaction`, `PerceivedDifficulty` | `engine.ComputeTutorMode`, `MotivationEngine.Build` |
| `AutonomyScore`, autonomy components | `metacognitive_mirror`, `DEPENDENCY_INCREASING` alert |
| `CalibrationBias` | Difficulty post-adjustment in `tools/activity.go:147`; `CALIBRATION_DIVERGING` alert |

### Session-scoped

| Field | Source |
|---|---|
| `sessionConcepts` (`map[string]int`) | `db.Store.GetSessionInteractions` filtered by domain |
| `sessionStart` | `db.Store.GetSessionStart` (time of first interaction in current session window) |
| `recentInteractions` (last 20) | feeds ZPD streaks, plateau detection |

## 3. Catalogue of `ActivityType` produced today

From `models/domain.go:42`:

```
RECALL_EXERCISE
NEW_CONCEPT
INTERLEAVING        // declared, NEVER produced by Route
MASTERY_CHALLENGE
DEBUGGING_CASE
REST
SETUP_DOMAIN        // produced by tools/activity.go when no domain exists, not by Route
```

`Activity` parameters set by `Route`:

| Field | Set how |
|---|---|
| `Type` | enum above |
| `Concept` | the chosen concept (empty for REST) |
| `DifficultyTarget` | hard-coded per bucket: 0.40 (ZPD), 0.55 (new), 0.60 (plateau), 0.65 (forgetting/recall), 0.75 (mastery) |
| `Format` | string label (`code_completion`, `guided_exercise`, `debugging`, `real_world_case`, `teaching_exercise`, `creative_application`, `introduction`, `build_challenge`, `mixed`) — purely informational, consumed by the LLM prompt |
| `EstimatedMinutes` | hard-coded per bucket (5–45) |
| `Rationale` | French string, surfaced to learner |
| `PromptForLLM` | French instruction telling the LLM **what to generate**, never the exercise itself |

**Important for GOAP design:** `Activity` is the *terminal* output — it is
what we hand to the LLM. The planner's leaf actions can map 1:1 to the
existing `ActivityType` set, *or* the planner could emit higher-level
"steps" that get translated to `Activity` at the boundary. Either way,
`PromptForLLM` content stays an LLM-facing instruction; the planner does
not write exercises.

`INTERLEAVING` is declared but never emitted — opportunity, but also a
signal that the current router doesn't really do "plans" (sequencing).

## 4. Alerts inventory (9 types)

From `models/domain.go:11` and `engine/alert.go`:

### Learning alerts (computed in `ComputeAlerts`)

| Alert | Trigger | `Route` reaction |
|---|---|---|
| `FORGETTING` | FSRS retention `<0.40` (warning) or `<0.30` (critical) | Critical → priority 1, RECALL @ 0.65, bypasses session dedup. Warning → falls through to default recall |
| `MASTERY_READY` | BKT `PMastery >= 0.85` | Priority 5, MASTERY_CHALLENGE @ 0.75 |
| `ZPD_DRIFT` (failures) | 3+ consecutive failures on same concept | Priority 2, RECALL @ 0.40 with guidance |
| `ZPD_DRIFT` (predictive) | IRT `pCorrect < 0.55` and `Reps > 0` and no failure-streak alert already | Same priority 2 bucket — emits info alert |
| `PLATEAU` | PFA score saturation over ≥4 interactions | Priority 3, DEBUGGING_CASE with rotating format; skipped if concept practiced ≥2× this session |
| `OVERLOAD` | session > 45 min | Priority 4, REST |

### Metacognitive alerts (computed in `ComputeMetacognitiveAlerts`, **separate function**)

| Alert | Trigger | `Route` reaction |
|---|---|---|
| `DEPENDENCY_INCREASING` | autonomy score declining over 3 sessions | **Not consumed by `Route`** — consumed by `mirror` and Discord webhooks |
| `CALIBRATION_DIVERGING` | `\|bias\| > 1.5` | **Not consumed by `Route`** — surfaces in `get_pending_alerts` and adjusts difficulty post-hoc in `tools/activity.go` |
| `AFFECT_NEGATIVE` | satisfaction ≤2 or perceivedDifficulty=1 over 2 sessions | **Not consumed by `Route`** — drives `tutor_mode` post-hoc |
| `TRANSFER_BLOCKED` | mastered (BKT≥0.85) but transfer score <0.50 on 2+ contexts | **Not consumed by `Route`** — emits `feynman challenge recommande` recommendation only |

This is a major point for GOAP design (see §6). The four metacognitive
alerts currently influence post-hoc parameters and the motivation layer,
but they never *change which activity gets picked*. A GOAP planner
could naturally consume them as preconditions/penalties — but it will
also need to coexist with the tutor_mode post-adjustment in `activity.go:131`.

## 5. Boundaries — what NOT to touch and what calls into the router

### Boundaries confirmed by inspection

- **`engine/motivation.go`** — `BriefInput`, `MotivationEngine.Build`,
  6 brief kinds, Hidi-Renninger phase inference. Reads `Activity.Type`
  and `Activity.Concept` after the router has chosen, never decides
  *what* to do. ✅ Do not touch.
- **`engine/metacognition.go`** — `ComputeAutonomyMetrics`,
  `ComputeTutorMode`, `DetectMirrorPattern`. All post-hoc to `Route`.
  ✅ Do not touch.
- **`algorithms/`** — BKT, FSRS, IRT, PFA, KST. The "physical laws"
  of the world. ✅ Do not touch.
- **LLM-generated content** — `PromptForLLM` strings, exercise content,
  hints, feedback. ✅ Stays LLM-side.

### MCP tools that indirectly invoke the router

Direct callers of `engine.Route`:

- `get_next_activity` (`tools/activity.go:101`) — primary consumer.
  Wraps the call with: alerts compute, frontier compute, session-concept
  build, then post-router enrichments (tutor_mode, calibration bias
  difficulty bump, misconception injection, motivation brief).
- `learning_negotiation` (`tools/negotiation.go:92`) — exposes the
  router's choice as `system_plan`, then computes `tradeoffs` for the
  learner's counter-proposal. **Today it can only show the next *single*
  activity** — exposing a real plan would be a behaviour upgrade for
  this tool (matches the brief's mandate that negotiation must benefit
  from the planner).

Indirect / orthogonal:

- `get_pending_alerts` reads alerts but doesn't route.
- `get_cockpit_state` reads `engine.OLMSnapshot` (FocusConcept, etc.)
  which uses KST frontier and `PinnedConcept` — separate from `Route`,
  but **the same "what's next" question** is answered there in a
  cockpit-shaped way. Worth keeping in mind: there are effectively
  *two* choosing mechanisms today (Route for activities, OLM for
  the dashboard's focus concept), and they can disagree.

## 6. Surprises, frictions, things that nuance the GOAP-réactif idea

This is the section the brief asked for explicitly. I'm being honest;
some of these may turn out to be non-issues but I want them on the
table before design.

### 6.1 The current "router" is closer to a reflex agent than a plan-needing system

Each `Route` call is **stateless** (modulo `sessionConcepts` passed in).
There is no concept of *"the plan I committed to last turn"*. The brief
asks for replanification after each interaction, plans ≤5 steps, and
exposing the plan in `learning_negotiation`. That's a real architectural
addition: we need a **plan store** (in-memory? persisted? scoped to
session?) and we need to decide whether the second step of a plan is
ever actually executed without replanning. If we replan after every
single interaction (the F.E.A.R. spirit), the persisted plan is mostly
a *justification artifact* and an *anticipation surface for negotiation*,
not a commitment. That should be made explicit in design.

### 6.2 `personal_goal` is currently free text with **no structured decomposition**

`grep -rn PersonalGoal` shows it is: (a) stored on `Domain`, (b) read by
`MotivationEngine` to build `goal_link` strings, (c) mentioned in
`olm_global` for global tutor frame. **There is no mapping today from
`personal_goal` → `(concept, mastery_target)` set.**

The brief asks the planner's goals to be A*-cibles in `(concept,
mastery_target)` form. So we need a **goal compiler** that turns the
free-text `personal_goal` into a target state. Two paths:

- **Naive:** target = `{ concept ∈ domain.Graph.Concepts : PMastery >= 0.85 }`.
  Domain-wide mastery. Loses the "personal" of personal_goal, but
  trivially deterministic.
- **Goal-aware:** ask the LLM at `init_domain` time to mark a subset
  of concepts as goal-critical (or weighted). This requires a schema
  change to `Domain` or to `value_framings_json`. It's the right
  long-term answer but introduces a co-authoring step.

This is a phase-1 decision-point.

### 6.3 The four metacognitive alerts don't gate activity choice today

Section 4 already noted this. For GOAP, treating them as preconditions
is *appealing* (no `MASTERY_CHALLENGE` action when `AFFECT_NEGATIVE` is
fresh, etc.) but risks **double-counting** because `tutor_mode` already
softens difficulty downstream. Either:

- The planner consumes metacognitive alerts and `activity.go` stops
  doing the post-hoc tutor-mode adjustment; or
- The planner ignores them and `activity.go` keeps doing the post-hoc
  pass.

Mixing both is the worst of both worlds (planner avoids hard challenges
*and* the survivor gets softened again). Phase-1 decision-point.

### 6.4 `Activity` carries `EstimatedMinutes` and `Format` — these are essentially **action parameters**, not effects

In a clean GOAP, an action's *effects* update `WorldState`. Currently
`Route` produces `Activity` with `Format: "code_completion"`,
`EstimatedMinutes: 8`, etc. These are downstream LLM hints, not state
changes. We need to decide whether action effects model the *expected*
state change (e.g., `expected_PMastery += δ` predicted by BKT under a
success assumption) or just the parameters. Real GOAP needs the former
to drive A* heuristics; the latter is "still a router with a slightly
fancier shell". Phase-1 decision-point.

### 6.5 PFA, IRT, BKT updates are all **stateful, real-valued, stochastic** — A* over them needs care

Predicting an action's effect on `WorldState` means simulating, e.g.,
`BKTUpdate(state, success=true)` to anticipate a `PMastery` gain. That's
fine — these functions are deterministic and exposed
(`algorithms/bkt.go:17`, `algorithms/fsrs.go:113`, etc.). But A* over
real-valued state needs **discretisation** or a **distance function**
to compare states. The brief says "heuristic, admissible, depth ≤ N";
admissibility on continuous state is non-trivial. Phase-1: define
the discretisation.

### 6.6 `OVERLOAD` and `REST` don't fit cleanly into A* with a "make progress on goal" cost

`OVERLOAD` ⇒ `REST` is a hard interrupt that ignores goal distance.
In GOAP terms it would be a high-priority precondition that overrides
goal pursuit. Easy to model with an *escape* action or a
*soft-constraint heuristic*; just calling it out — F.E.A.R.-style GOAP
with goal-priority works for this.

### 6.7 The 70 tests claim and reality

The README says "70 tests". `find . -name "*_test.go" | wc -l` reports
**67 test files**, and `grep -c "func Test"` totals **289** test
functions across packages. The "70" is likely stale count of test
*functions* — but the order of magnitude is right: there's substantial
coverage to preserve. No regression target needs adjustment.

### 6.8 OLM's `FocusConcept` is a parallel "what's next" answer

`engine/olm.go:182-218` already chooses a focus concept for the cockpit
using a hierarchy: critical alert > frontier first > pinned. This is
similar in spirit to `Route` but distinct in mechanism. **If the
planner becomes the source of truth, OLM's focus computation should
ideally be derived from `Plan[0].Concept`** — otherwise the cockpit
and the activity recommendation can drift. Worth flagging as
design-time scope: do we touch OLM in phase 1 or leave it as a
follow-up?

### 6.9 `INTERLEAVING` is declared but never emitted

**Resolved (sub-issue #64):** `ActivityInterleaving` was removed from
`models/domain.go` because no production path ever emitted it. A
future planner that thinks in sequences could naturally re-introduce
it ("alternate concept A and concept B over 3 steps to combat
illusion of competence") — free expressivity gain, but also a risk
of inflating action count beyond the 8–12 budget the brief asks for.
Re-introducing it must come with a Rohrer-2012-style emitter, not
just a constant.

### 6.10 `learning_negotiation` returns a **single tradeoff per factor**, not a plan diff

Today the tool emits `tradeoffs []tradeoff` (retention, prerequisites)
and an `accepted_plan` that is one activity. The brief mandates the
planner upgrade benefit the tool — concretely, it should expose
`current_plan: [step1, step2, step3...]` and
`alternative_plan: [step1', step2'...]` with cost/distance comparisons.
That is a **shape change** to the tool's output schema, not just an
internal swap. Worth signalling in the migration plan.

---

## Summary

The existing routing layer is a 7-bucket priority reflex on alerts +
KST frontier + retention fallback. It is small, well-tested, pure, and
called from exactly two MCP tools. The cognitive algorithms expose
deterministic update functions suitable for action-effect simulation,
but the world state is real-valued and there is no plan store today.
`personal_goal` is free text and has no structured decomposition; four
of nine alerts don't currently influence choice; `tutor_mode` and
calibration-bias adjustments happen post-hoc in `tools/activity.go`
and would either need to be folded into the planner or kept as a
separate post-pass.

The GOAP-réactif design is feasible but has six real decision points
that should be resolved before any code is written:

- D1 — plan persistence and replanning frequency (§6.1)
- D2 — `personal_goal` decomposition strategy (§6.2)
- D3 — metacognitive-alert ownership: planner vs. post-hoc (§6.3)
- D4 — action-effect modelling: parameters vs. predicted state (§6.4)
- D5 — `WorldState` discretisation for A* admissibility (§6.5)
- D6 — OLM `FocusConcept` alignment with planner (§6.8)

Plus three smaller scope questions (`learning_negotiation` schema
change §6.10, INTERLEAVING reintroduction §6.9, OLM scope §6.8).

**STOP.** Awaiting validation before phase 1 (design).
