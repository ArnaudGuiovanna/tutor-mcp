# Learning integrity contract

Tutor MCP is an adaptive learning runtime. It may describe a learner as having
made progress only when the persisted evidence supports that statement. This
document defines the product invariants enforced by the runtime and its tests.

## Evidence stages

Progress is not a single boolean. Each concept can independently be:

- **estimated**: the learner model has enough scored observations to form a
  bounded estimate;
- **retained**: a delayed successful retrieval is linked to a submitted and
  evaluated attempt, occurs at least 24 hours after prior exposure, and meets
  the configured retention gate;
- **demonstrated**: a versioned assessment attempt passes a rubric that was
  fixed before grading;
- **transferred**: demonstrated evidence also passes trusted transfer probes
  across at least three of the five independent dimensions (near, far,
  debugging, teaching and creative), with no unresolved recent trusted
  failure. Trusted failures are read alongside successes; averaging them away
  cannot preserve a transferred/readiness claim.

`solid`, `mastered` and completion percentages must be derived from these
stages through one shared policy. A BKT threshold by itself is not proof of
retention, demonstration or transfer.

Learner-facing dashboards expose the evidence stage and the model's routing
estimate separately. Their progress percentage is the proportion of active
competencies with demonstrated evidence; a high estimate alone cannot increase
that percentage. The deprecated `mastered_count` compatibility field is an
alias of the demonstrated count.

## Assessment evidence

`PRACTICE` and `NEW_CONCEPT` may be recorded without an attempt to preserve a
low-friction learning loop. They are explicitly labelled
`unverified_observation` and may update routing estimates, but can never mint a
retained, demonstrated or transferred claim. Every interaction used for one of
those claims must be traceable to an assessment attempt. An attempt records:

- a stable activity identifier and version;
- the domain, concept and observable learning objective;
- the exact prompt/task and the learner response or an integrity-preserving
  content hash when the artefact cannot be stored;
- the rubric and passing rule as they existed before evaluation;
- per-criterion evidence, evaluator identity and evaluation method;
- the derived outcome, timestamps and optional session identifier.

The server derives the interaction outcome from the assessment record. A host
must not be able to set `success` independently of contradictory rubric data.
Legacy or public interactions without this evidence remain historical/routing
observations but cannot establish `retained`, `demonstrated` or `transferred`,
even when a successful retrieval is more than 24 hours after an earlier event.

The public MCP evaluator path is deliberately untrusted: it records
`host_llm` provenance but cannot set `trusted_evaluation`. Trusted
`deterministic`, `external_service`, or `human_review` evidence can only enter
through a server-controlled evaluator boundary. Until such a boundary is
configured, a deployment can report estimated and retained stages but cannot
manufacture a demonstrated claim.

Decision-bound attempts use the shared `assessment` package at both MCP and
storage boundaries. Rubric/scoring JSON must have unique object keys, canonical
criterion IDs and finite JSON numbers; aliases, numeric strings and unsupported
fields are rejected. Each criterion is scored exactly once with non-empty
`evidence`. Optional `total`, `max_total` and per-criterion `max_score` must agree
with the frozen rubric and scores. Confidence remains descriptive metadata.

Stored float64 values are summed using their canonical decimal representations;
passing compares that sum to the frozen threshold without a fixed epsilon.
This avoids order-dependent arithmetic and a zero passing a tiny positive
threshold, but does not imply arbitrary precision of incoming JSON numbers.
Each JSON document and its canonical form are limited to 16,384 bytes.

Storage verifies the frozen passing rule on creation and recomputes the bound
outcome before completing an evaluation, under the curriculum/attempt locks.
A rejected result leaves the attempt submitted and rolls back any composed
learning writes. Standalone/legacy normalization remains unchanged. No existing
evaluation is rewritten and structural validity never grants evaluator trust.
This shared contract prepares, but does not implement, an independent review
channel, reviewer identity or disagreement adjudication.

## Mutation delivery

Every state-changing MCP schema accepts an optional `idempotency_key`, also
accepted as `_meta.idempotency_key`. The durable identity is learner + tool +
key. An exact canonical retry replays the original successful response;
different arguments conflict. A processing reservation is never automatically
stolen after an ambiguous disconnect because duplicating a learning mutation
is less safe than requiring operator diagnosis. Expiring a cached response
retains the learner/tool/key tuple, request hash and completed status. An exact
retry then returns an explicit “mutation already completed; cached response
expired” error and never invokes the mutation handler again.

## Diagnostic protocol

A diagnostic activity is an assessment, not instruction. It must:

- be presented before explanation or hints;
- use the dedicated `DIAGNOSTIC_ASSESSMENT` activity type;
- be linked to a submitted and evaluated attempt; interactions without an
  attempt or with `hints_requested > 0` do not count;
- sample the domain rather than repeatedly teaching the lowest-prior concept;
- cover every distinct concept when the domain has at most eight concepts, or
  at least eight distinct concepts in a larger domain; repeated attempts on one
  concept do not consume that target, and entropy cannot bypass it;
- never count an introduction as prior-knowledge evidence.

`MASTERY_READY` and `check_mastery` consume the same evaluated attempts and
trusted transfer facts. A recent trusted transfer failure therefore suppresses
both surfaces until a later passing observation repairs that dimension.

## Sessions

A session is a durable entity with a unique ID, learner, optional active
domain, `started_at`, `last_active_at` and `closed_at`. Interactions, affect,
transfer observations, intentions and summaries reference that ID. Closing a
session changes its state; a two-hour sliding window is not a substitute.

## Curriculum

A curriculum version contains stable competency identifiers, observable
outcomes, level descriptors, prerequisite edges, assessment criteria,
provenance and review status. Structural graph validity is necessary but not
sufficient. Mutations are versioned and concurrency-safe; rename, split,
merge and removal preserve an audit trail.

`get_curriculum_snapshot` exposes the current version and stable concept IDs.
Every `add_concepts` or `publish_curriculum_revision` write must supply that
version as `expected_version`; a stale writer is rejected and must rebase.
Rename changes only the display label. Split and merge retire their sources in
the new snapshot, while removal is refused if an active competency still
depends on the target. Public callers may mark a proposal unreviewed or
in-review, but cannot self-assert trusted approval.

## Learner control and safety

Learners control timezone, availability windows, do-not-disturb state,
notification consent/frequency and accessibility accommodations. Derived
calibration and autonomy values are computed from evidence and are not writable
profile fields. High-stakes domains must be identifiable so a deployment can
require human-reviewed curriculum and evaluation.

Notification consent is opt-in: an existing webhook URL is not consent. Weekly
windows use local civil time in a validated IANA timezone and are evaluated with
the timezone database, including 23-hour and 25-hour DST days. The scheduler
atomically reserves a local-day slot before delivery so concurrent workers
cannot exceed the learner's frequency or daily cap. DND always wins. Screen
reader and decorative-emoji preferences are applied to generated webhook
embeds; the remaining accessibility preferences are surfaced to the tutor.

High-stakes classification is one-way through the learner-facing MCP boundary.
In those domains, a trusted passed assessment counts as `demonstrated` only when
its persisted evaluation method is `human_review`. Intrusive proactive
suggestions are also blocked until a trusted human-reviewed evaluation exists.
`host_llm`, `deterministic`, and `external_service` labels do not satisfy this
gate, and the runtime does not claim that any external review service exists.

## Release gates

The learning-integrity suite must cover, at minimum:

1. one answer cannot saturate ability at either bound;
2. alternating outcomes produce a stable cumulative trajectory;
3. a cold diagnostic never returns an instructional activity;
4. Spanish, history, mathematics and programming receive domain-neutral task
   contracts unless their curriculum explicitly requests code;
5. a completed transfer probe exits the unobserved state atomically;
6. an objective/language update round-trips through learner context;
7. learners without webhooks still receive memory consolidation;
8. one queued mirror produces exactly one delivery;
9. concurrent graph and relevance mutations preserve both writers or return a
   version conflict;
10. delayed-recall fixtures prove that unlinked observations remain
    unverified, while linked attempts distinguish retained and demonstrated;
11. diagnostic fixtures reject hints, missing attempts and repeated-concept
    padding, and enforce the bounded distinct-concept target;
12. trusted transfer fixtures retain both successes and failures and keep
    `check_mastery` coherent with `MASTERY_READY`;
13. public HTTP bodies, emails and token rotation satisfy the security limits;
14. SQLite and PostgreSQL pass the same persistence conformance tests.

Product claims about demonstrated learning additionally require external
pre-test/post-test, delayed-retention and novel-transfer evaluation; unit test
coverage alone is not evidence of educational effectiveness.
