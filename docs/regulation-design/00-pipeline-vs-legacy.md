# Pipeline vs Legacy Router — Migration Mapping

> **Historical design record — non-normative.** This document compares the
> current pipeline with a transitional router and feature flag that have since
> been removed. References to `engine/router.go`, `engine.Route`, and
> `REGULATION_PHASE` describe past migration state. The supported runtime is
> the always-on pipeline in `engine/orchestrator.go`.

> Audit doc requested by issue #15. For each of the 7 priorities encoded
> in the legacy `engine.Route` cascade (`engine/router.go`), this file
> traces *where* the equivalent behaviour lives in the orchestrator
> (`engine.Orchestrate` → `[2] PhaseFSM` → `[3] Gate` → `[4]
> ConceptSelector` → `[5] ActionSelector`), and emits a verdict per
> priority: **Preserved**, **Reformulated**, **Dropped (intentional)**,
> or **Lost (bug)**.
>
> No runtime change is made by this doc. The one verdict at risk of
> being a real gap (FORGETTING critical on a non-mastered concept in
> INSTRUCTION phase) is filed as a follow-up issue at the bottom.

---

## Reading guide

The legacy router is a single `for`-cascade scoped to one
`get_next_activity` call. It treats the full `[]models.Alert` slice as
the routing input and emits an `Activity` directly.

The orchestrator splits that monolith into pure stages:

- `[2] PhaseFSM` (`engine/phase_fsm.go:85`) — picks among
  `{INSTRUCTION, DIAGNOSTIC, MAINTENANCE}` from observables (mean
  binary entropy, mastery counts, retention floor). The FSM never
  reads alerts; it only reads `PhaseObservables`.
- `[3] Gate` (`engine/gate.go:191`) — produces an `EscapeAction`,
  `NoCandidate`, or an `AllowedConcepts` pool with optional
  per-concept `ActionRestriction`. Reads alerts only for OVERLOAD
  (escape) and FORGETTING (anti-rep bypass).
- `[4] ConceptSelector` (`engine/concept_selector.go:75`) — picks
  *which concept* to work on inside the gate's allowed pool. Phase-
  dispatch: INSTRUCTION → `argmax(rel × (1-mastery))` over the KST
  external fringe; MAINTENANCE → `argmax((1-retention) × rel)` over
  mastered concepts; DIAGNOSTIC → `argmax(BKT info-gain)`. Reads no
  alerts.
- `[5] ActionSelector` (`engine/action_selector.go:135`) — picks
  *what to do* on the chosen concept. Cascade: nil/NaN → REST;
  active misconception → DEBUG_MISCONCEPTION; retention<0.5 →
  RECALL_EXERCISE; mastery brackets → NEW_CONCEPT / PRACTICE /
  high-mastery rotation (MasteryChallenge → Feynman → Transfer).
  Reads no alerts.

Wiring entry: `tools/activity.go:107` selects `Orchestrate` when
`REGULATION_PHASE` is on (default-on per `engine/phase_config.go`),
falling back to `engine.Route` only on orchestrator error or when the
flag is explicitly set to `off`.

The `[]models.Alert` slice is still computed (`ComputeAlerts` at
`engine/orchestrator.go:210`) and surfaced to the cockpit and audit
log. **Inside the pipeline** the alerts are consumed at exactly two
points:

- `engine/gate.go:201` — OVERLOAD escape;
- `engine/gate.go:270` — FORGETTING anti-rep bypass.

Everything else the legacy cascade did with alerts has been
*reformulated* through observables (mastery, retention, theta,
misconception flag, action history) read directly by the relevant
selector.

---

## Priority 1 — FORGETTING critical (FSRS retention < 0.30)

**Legacy** (`engine/router.go:18-28`): the first alert with
`Type=AlertForgetting` and `Urgency=Critical` short-circuits to
`ActivityRecall` on that concept with `DifficultyTarget=0.65`,
`Format="code_completion"`, regardless of session-dedup. The alert is
emitted by `engine/alert.go:30-34` for any concept (mastered or not)
with retention < 0.30 and `CardState != "new"`.

**Orchestrator**: the FORGETTING signal is split across three
mechanisms, each anchored on retention rather than on the alert
itself:

1. **MAINTENANCE → INSTRUCTION FSM transition**
   (`engine/phase_fsm.go:136-142`). When *any* goal-relevant concept
   has retention strictly below `cfg.RetentionRecallThreshold` (0.5
   in the default profile, see `engine/phase_config.go`), the FSM
   pulls the domain back to INSTRUCTION on the next call. This
   handles "forgetting drift" at the whole-domain granularity, but
   it switches *out* of MAINTENANCE — it does not pick the forgotten
   concept itself.

2. **`[4] MAINTENANCE` selector**
   (`engine/concept_selector.go:247-320`). When phase is MAINTENANCE,
   the selector scores each *mastered* concept by
   `urgency × goal_relevance` with `urgency = 1 - retention`. The
   most-forgotten mastered concept wins. This subsumes the legacy
   priority 7 fallback ("default recall on lowest retention") and
   the typical FORGETTING case (concept is mastered, retention has
   decayed) in one step.

3. **`[5] ActionSelector` retention branch**
   (`engine/action_selector.go:171-182`). After [4] picks a concept,
   if `retention < 0.5` (and `CardState != "new"`), [5] emits
   `RECALL_EXERCISE` with `DifficultyTarget=0.65`,
   `Format="code_completion"` — bit-for-bit the legacy priority-1
   activity shape, gated on the [4] choice.

**Where the alert itself goes**: `engine/gate.go:270` collects
FORGETTING-tagged concepts into `forgettingSet` and uses it to
*bypass* the anti-repetition window — i.e. a forgetting concept that
would normally be filtered as "too recent" stays in the pool. This
is the only direct alert→pipeline coupling for FORGETTING.

**Verdict**: **Reformulated** for the common case (mastered concept
forgetting, MAINTENANCE active or fired by FSM), but with a
**potential gap** in INSTRUCTION phase on non-mastered concepts —
see "Follow-up gaps" below. The legacy cascade fired retention<0.30
unconditionally; the orchestrator only reaches the retention branch
of [5] *if* [4] picks the forgotten concept, which in INSTRUCTION it
won't (INSTRUCTION scores by `rel × (1-mastery)`, ignoring
retention).

---

## Priority 2 — ZPD_DRIFT (3+ consecutive failures, or IRT pCorrect<0.55)

**Legacy** (`engine/router.go:30-39`): any `AlertZPDDrift` short-
circuits to `ActivityRecall` on that concept with `DifficultyTarget=0.40`,
`Format="guided_exercise"`, rationale citing the error rate. Alert
emitted by `engine/alert.go:60-126` either on a 3-failure streak or
on IRT pCorrect<0.55.

**Orchestrator**: the ZPD_DRIFT *alert* is not consumed by `[3]`,
`[4]`, or `[5]`. The drift *condition* is reformulated through:

- **Mastery erosion in the BKT update** (off-pipeline, in
  `engine.RecordInteraction`). Repeated failures push `PMastery`
  down, which moves the concept from the "p ≥ MasteryBKT()" bracket
  into "p < 0.70" or "p < 0.30", changing what `[5]` emits
  (`engine/action_selector.go:186-202`).
- **IRT-based difficulty reduction**. `[5]`'s ZPD branch
  (`engine/action_selector.go:303-313`,
  `zpdDifficultyFromTheta`) recomputes the `DifficultyTarget` from
  the learner's current `Theta` to target pCorrect≈0.70 — the same
  mechanism that produces ZPD_DRIFT alerts also produces lower
  `DifficultyTarget` values via `[5]`. Net effect: the next
  activity on a drifting concept is automatically easier.

The "3 consecutive failures" alert specifically is *informational
only* in the orchestrator world — it surfaces in the cockpit but
does not redirect routing. The drift is regulated through the BKT/
IRT signal that *generates* the alert, not the alert itself.

**Verdict**: **Reformulated** (continuous regulation through Theta
and PMastery) rather than discrete (alert-triggered redirect). Trade-
off: the orchestrator never produces the legacy
`Format="guided_exercise"` shape; the closest analogue is
`Format="practice_zpd"` from `[5]`. Open question for a future
calibration pass: is `practice_zpd` enough scaffolding when the
learner has just chained 3 failures? Not in scope here.

---

## Priority 3 — PLATEAU (PFA stagnation, sessions stalled)

**Legacy** (`engine/router.go:40-70`): `AlertPlateau` short-circuits
to `ActivityDebuggingCase` with a rotating format
(`debugging` / `real_world_case` / `teaching_exercise` /
`creative_application`) chosen by the per-concept interaction count.
The alert is emitted by `engine/alert.go:128-151` when PFA scores
saturate (4-window plateau detection on the chronological score
sequence).

**Orchestrator**: PLATEAU alert is not consumed by `[3]`, `[4]`,
or `[5]`. The plateau *condition* is reformulated through the high-
mastery rotation in `[5]`:

- A learner stuck at "stable above BKT but no new growth" is exactly
  the population the rotation
  (`engine/action_selector.go:254-289`,
  `selectHighMasteryAction`) targets:
  `MasteryChallenge → Feynman → TransferChallenge → MasteryChallenge ↻`.
  This is the *constructive* analogue of the legacy escape into
  debugging cases — instead of switching the activity *format* in
  place, the orchestrator switches the *kind* of cognitive task.
- The legacy "creative_application" / "real_world_case" formats
  loosely correspond to `Transfer` and the explanation-focused
  `Feynman`; "teaching_exercise" maps to `Feynman` directly.
- The orchestrator never emits `ActivityDebuggingCase` (the type
  still exists in `models/activity.go` but no selector chooses it).

**Verdict**: **Reformulated** through the [5] high-mastery
rotation. The plateau detector itself remains useful as a *signal*
on the cockpit (the alert is still computed) but plays no routing
role. One observable lost: the legacy rotation was per-concept
based on raw interaction count; the orchestrator's rotation is
gated by `InteractionsAboveBKT` plus per-bucket counters
(`MasteryChallengeCount`, `FeynmanCount`, `TransferCount`), which
means the orchestrator only rotates *after* mastery is consolidated,
not at the first sign of stagnation. This is intentional (OQ-5.5
stability window).

---

## Priority 4 — OVERLOAD (session > 45 min)

**Legacy** (`engine/router.go:71-77`): `AlertOverload` short-circuits
to `ActivityRest` with rationale "session > 45 minutes".

**Orchestrator** (`engine/gate.go:200-209`): `[3] Gate` checks
`AlertOverload` *first*, before any prereq/anti-rep filtering, and
returns an `EscapeAction{Type: ActivityCloseSession,
Format: "session_overload"}`. The orchestrator's `runPipeline`
(`engine/orchestrator.go:326-328`) emits this directly via
`composeEscapeActivity`, bypassing `[4]` and `[5]`.

Note the `Type` change: legacy was `ActivityRest`; orchestrator is
`ActivityCloseSession`. This is a *semantic upgrade* — the
orchestrator pairs the escape with the `record_session_close` tool
flow (see `composeEscapeActivity` PromptForLLM) instead of just
suggesting a pause. Behaviour-equivalent at the user-facing level
(both stop the session).

**Verdict**: **Preserved** (with a deliberate Type rename).

---

## Priority 5 — MASTERY_READY (BKT ≥ 0.85)

**Legacy** (`engine/router.go:78-91`): `AlertMasteryReady` short-
circuits to `ActivityMasteryChallenge` on that concept with
`DifficultyTarget=0.75`, `Format="build_challenge"`, skipping if the
concept was already practiced this session. Alert emitted by
`engine/alert.go:50-57` for every concept where `PMastery >=
MasteryBKT()`.

**Orchestrator**: the MASTERY_READY *alert* is not consumed by `[3]`,
`[4]`, or `[5]`. The condition is reformulated as a *consequence* of
selection rather than a router:

- **`[4] INSTRUCTION` will not pick a mastered concept**
  (`engine/concept_selector.go:128-130`: `mastery >= bktThreshold`
  excludes from the external fringe). So in INSTRUCTION phase, a
  MASTERY_READY concept simply waits.
- **`[2] FSM` transitions to MAINTENANCE** when *all* goal-relevant
  concepts cross `MasteryBKT()` (`engine/phase_fsm.go:121-128`).
  Once in MAINTENANCE, `[4]` scores mastered concepts by
  retention urgency (`engine/concept_selector.go:247-320`), which
  is the moment a recently-mastered concept becomes selectable.
- **`[5] ActionSelector` emits MasteryChallenge** only when the
  *chosen* concept has `mastery >= MasteryBKT()` AND has accumulated
  `InteractionsAboveBKT >= HighMasteryStabilityWindow` (=3) since
  crossing the threshold (`engine/action_selector.go:212-229`,
  `selectHighMasteryAction`). The first MasteryChallenge fires only
  after this stability window — explicitly to avoid the ping-pong
  pathology described in OQ-5.5.

**This is the routing change that motivated issue #15**: the
observed behaviour where 4 active `MASTERY_READY` alerts did not
surface through `get_next_activity` is not a bug. It is the
intentional consequence of:

1. The pipeline routes through *phase + concept score*, not through
   alerts.
2. INSTRUCTION won't pick mastered concepts (the legacy router
   would have, via the alert short-circuit).
3. The transition to MAINTENANCE requires *all* goal-relevant
   concepts mastered — a partial mastery picture keeps the domain
   in INSTRUCTION, where the mastered concepts wait.
4. Even after MAINTENANCE engages, `[5]` requires the
   `HighMasteryStabilityWindow` of 3 interactions before emitting
   the challenge.

**Verdict**: **Reformulated** (alert is now a consequence, not a
router). The change is intentional per the design docs of `[2]`,
`[4]`, and `[5]`. The trade-off documented in
`docs/regulation-design/05-action-selector.md` §OQ-5.5: avoid
ping-pong oscillation around the BKT threshold at the cost of a
slower MasteryChallenge cadence.

---

## Priority 6 — NEW_CONCEPT from KST fringe

**Legacy** (`engine/router.go:92-103`): if no alert fires, iterate
the `frontier` (KST external fringe, computed in `tools/activity.go`)
and pick the first concept not yet introduced this session. Emits
`ActivityNewConcept` with `DifficultyTarget=0.55`,
`Format="introduction"`.

**Orchestrator**:

- `[4] INSTRUCTION` (`engine/concept_selector.go:181-233`) is
  exactly this priority's home. It computes the same KST external
  fringe (`externalFringe` at `engine/concept_selector.go:109-158`)
  with the same `MasteryBKT()` / `MasteryKST()` thresholds and
  picks `argmax(rel × (1-mastery))` instead of "first
  alphabetical". Improvement: weighted by goal_relevance, ignores
  in-session dedup (the orchestrator handles dedup via `[3] Gate`'s
  anti-repetition window instead — `engine/gate.go:254-282`).
- `[5] ActionSelector` then emits `ActivityNewConcept` for the
  bracket `mastery < 0.30` (`engine/action_selector.go:186-194`)
  with the exact same `DifficultyTarget=0.55`, `Format="introduction"`
  shape.

Concepts at `0.30 ≤ mastery < 0.70` get `ActivityPractice` standard
instead of `ActivityNewConcept` — a finer split than the legacy
binary "new vs not".

**Verdict**: **Reformulated** with strict improvement (goal_relevance
weighting, explicit mastery brackets, anti-rep at the gate). The
legacy "first concept in frontier order" is gone; replaced by
deterministic `argmax` with alphabetical tie-break.

---

## Priority 7 — RECALL by default (lowest FSRS retrievability)

**Legacy** (`engine/router.go:104-137`): if nothing else fires, scan
all non-"new" `ConceptState`s, pick the one with lowest
`Retrievability(elapsed, stability)`, prefer one not practiced this
session, emit `ActivityRecall` with `Format="mixed"`.

**Orchestrator**:

- `[4] MAINTENANCE` selector (`engine/concept_selector.go:247-320`)
  is the home. Same retention scoring (`urgency = 1 - retention`),
  scoped to mastered concepts and weighted by goal_relevance.
- `[5] ActionSelector` emits `ActivityRecall`
  (`engine/action_selector.go:171-182`) for any selected concept
  with `retention < 0.5`. Format is `code_completion` instead of
  `mixed` — minor cosmetic difference.

The "default" nature of the legacy priority is preserved by the
phase machinery: once goal-relevant concepts are mastered the FSM
moves to MAINTENANCE, where this priority dominates by construction.
What is *not* preserved: the legacy router would emit recall on
non-mastered concepts as a last resort fallback. The orchestrator
won't — non-mastered concepts route through INSTRUCTION
`(rel × (1-mastery))`, never through retention scoring.

**Verdict**: **Reformulated**, with the same observable trade-off
flagged at priority 1 (FORGETTING on a non-mastered concept in
INSTRUCTION).

---

## Verdict table

| #  | Legacy priority      | Verdict                | Where in orchestrator                                       |
|----|----------------------|------------------------|-------------------------------------------------------------|
| 1  | FORGETTING critical  | **Reformulated** *    | `[2]` MAINT→INSTR transition + `[4]` MAINTENANCE + `[5]` retention branch + `[3]` anti-rep bypass |
| 2  | ZPD_DRIFT            | **Reformulated**       | BKT/IRT loop + `[5]` ZPD via `zpdDifficultyFromTheta`       |
| 3  | PLATEAU              | **Reformulated**       | `[5]` high-mastery rotation (`selectHighMasteryAction`)     |
| 4  | OVERLOAD             | **Preserved**          | `[3]` `EscapeAction{ActivityCloseSession}` (was Rest)       |
| 5  | MASTERY_READY        | **Reformulated**       | `[2]` INSTR→MAINT transition + `[5]` high-mastery rotation; alert is now a consequence, not a router |
| 6  | NEW_CONCEPT          | **Reformulated**       | `[4]` INSTRUCTION `argmax(rel × (1-mastery))` + `[5]` `mastery<0.30` bracket |
| 7  | RECALL default       | **Reformulated**       | `[4]` MAINTENANCE `argmax((1-retention) × rel)` + `[5]` retention branch |

\* Priority 1 has a residual gap when phase=INSTRUCTION and the
forgetting concept is non-mastered + goal-relevant. Tracked in the
follow-up issue below.

---

## Follow-up gaps

### Gap A — FORGETTING critical on a non-mastered concept in INSTRUCTION

**Filed as follow-up**: see issue #16 — "Routing gap: FORGETTING
critical lost in orchestrator migration (INSTRUCTION phase, non-
mastered concept)".

**Reproduction sketch**: a learner with phase=INSTRUCTION, a concept
`X` at `PMastery=0.55` (above the new-concept floor, below BKT) and
`retention<0.30` (FORGETTING critical alert active). The legacy
router would short-circuit to RECALL on X. The orchestrator picks
*not* X but `argmax(rel × (1-mastery))` on the external fringe — X
is in the fringe (mastery < BKT) and is competing on `(1-mastery)`,
where its 0.55 score is *worse* than any concept with mastery <
0.55. So a low-mastery concept wins, and X (forgetting critical)
waits for its mastery to either drop to <0.30 (NEW_CONCEPT bracket
in [5]) or rise above BKT (then MAINTENANCE picks it). Meanwhile
the FSRS retention keeps decaying.

**Proposed fix sketch** (do not implement here): two reasonable
shapes — pick whichever the maintainer prefers in the follow-up:

- *Option 1 (gate-side):* in `[3] Gate` add a "FORGETTING priority"
  rule that, when an `AlertForgetting` with
  `Urgency=Critical` exists for a concept that survived prereq+
  anti-rep, restricts the allowed pool to that concept only (so [4]
  has no choice). Mirrors the OVERLOAD escape but at the
  AllowedConcepts level instead of the EscapeAction level.
- *Option 2 (selector-side):* in `[4] selectInstruction` boost the
  score of forgetting-critical concepts by a multiplicative factor
  (or replace the `(1-mastery)` term with `max(1-mastery,
  1-retention)` for non-"new" cards). Keeps the score-based
  architecture; preserves alphabetical tie-break.

The choice is non-trivial — option 1 is more legacy-faithful
(forced redirect) but breaks the "alerts don't router" invariant
introduced in v0.3; option 2 keeps the architecture clean but
requires re-tuning the score formula. Hence: follow-up issue, not
a same-PR patch.

---

## Cross-references

- Architecture doc that motivated the migration:
  `docs/regulation-architecture.md` §3 (the 7-component split),
  §dette différée §C (legacy migration plan).
- `[2]` design and FSM contract:
  `docs/regulation-design/02-phase-controller.md`.
- `[3]` Gate including OVERLOAD escape and FORGETTING anti-rep
  bypass: `docs/regulation-design/03-gate-controller.md`.
- `[4]` ConceptSelector phase branches and OQ-4.x decisions:
  `docs/regulation-design/04-concept-selector.md`.
- `[5]` ActionSelector cascade and OQ-5.x decisions:
  `docs/regulation-design/05-action-selector.md`.
- Legacy router implementation: `engine/router.go` (still wired in
  `tools/activity.go:118` and `:125` as fallback).
