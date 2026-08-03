# Changelog

All notable changes to Tutor MCP are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security

- Bind OAuth authorization codes to their redirect URI and PKCE challenge,
  consume them atomically, rotate refresh-token families transactionally, and
  revoke a family when an already-used member is replayed. Access tokens now
  expire after 30 minutes.
- Enforce bounded public request bodies, strict normalized emails, single-use
  CSRF state, namespaced race-safe rate limits, and per-account login limits.
- Add learner-scoped idempotency keys to every state-changing MCP tool, with
  canonical request hashing, exact successful-response replay, conflict
  rejection, and fail-closed handling after an ambiguous completion.
- Stop webhook transport failures and rejected delivery targets from leaking
  credential-bearing URLs into logs or tool responses; harden install and
  backup scripts against unsafe paths and permissions.
- Revoke plaintext/unbound refresh-token families, purge legacy authorization
  codes without exact redirect/S256 binding, pseudonymize structured learning
  identifiers in logs, and remove free-form pedagogical text from INFO logs.

### Added

- Add durable learning sessions and implementation-intention lifecycles so
  interactions, affect, assessments, transfer evidence and summaries share a
  canonical episode ID instead of a sliding two-hour approximation.
- Add versioned assessment attempts that freeze task and rubric before the
  learner responds, commit the response before evaluation, derive pass/fail
  server-side, and atomically consume an attempt with its interaction.
- Add the shared `estimated` → `retained` → `demonstrated` →
  `transferred` evidence ladder to mastery checks, the OLM and dashboards.
  Host-LLM evaluations remain explicitly untrusted; high-stakes domains accept
  only trusted human-review evidence.
- Add immutable, CAS-protected curriculum snapshots with stable competency,
  outcome and criterion identifiers; observable outcomes, level descriptors,
  provenance and review state; audited rename/split/merge/removal; and
  evidence-preserving domain tombstones.
- Add learner-owned IANA timezones, weekly local availability, DND,
  notification consent/frequency/caps and accessibility preferences. Delivery
  reservations are atomic and DST-aware.
- Add durable webhook retry state, bounded backoff, stale-claim recovery,
  expiration and dead-letter metadata.
- Add the dry-run-first `tutor-retention` maintenance command for terminal and
  orphaned live webhooks, abandoned assessment plaintext with verified hashes,
  completed consolidation markers, narrative Markdown, old pedagogical
  snapshots and notification event logs.

### Changed

- Replace one-step IRT updates with a finite, clamped, regularized cumulative
  MAP estimate and remove the unused Rasch/Elo path.
- Make a cold domain emit only `DIAGNOSTIC_ASSESSMENT` probes until bounded,
  distinct, attempt-linked concept coverage is satisfied; ordinary practice,
  hinted attempts and repeated items cannot exhaust the diagnostic budget.
- Define `MASTERY_READY` as readiness to attempt a challenge, requiring delayed
  successful recall, varied evidence and acceptable uncertainty rather than a
  BKT threshold alone.
- Require delayed-retention evidence to reference a submitted/evaluated
  assessment attempt; unlinked public interactions remain explicitly
  unverified routing observations even after 24 hours.
- Count diagnostic completion by hint-free, evaluated attempt coverage over
  distinct concepts (all concepts up to eight, otherwise eight), so repetitions
  and entropy changes cannot bypass cold measurement.
- Feed trusted transfer successes and failures into both `check_mastery` and
  `MASTERY_READY`; an unresolved recent trusted failure blocks readiness and the
  transferred stage, including under the human-only high-stakes gate.
- Separate model-estimated routing state from evidence-backed progress in the
  dashboard. The deprecated `mastered_count` alias now means demonstrated, not
  merely high estimated probability.
- Treat direct `record_transfer_result` calls as legacy unverified
  observations. Transfer claims require trusted passed transfer attempts in at
  least three independent dimensions with no blocking failure.
- Consolidate narrative memory for every learner, including those without a
  webhook, and claim scheduled work atomically across workers.
- Scope runtime narrative sessions and concept notes by domain; ambiguous
  learner-global legacy narratives are retained for export but not injected
  into activity generation.

### Fixed

- Preserve successful write semantics when optional post-write enrichment
  fails, preventing transport retries from duplicating memory, calibration or
  transfer mutations.
- Validate affect input before opening/touching a session and make transfer
  interaction plus transfer-profile updates atomic.
- Ensure deleted domains disappear from runtime selection while retaining
  curriculum, completed intentions, terminal delivery history and learning
  evidence for audit.
- Make one queued metacognitive mirror map to one durable delivery and prevent
  webhook retries from being lost across process restarts or scheduler gaps.
- Normalize webhook domain ownership, terminalize queued work on archive or
  deletion, revalidate claims immediately before the HTTP boundary, and use
  one HTTP attempt per durable claim.

### Documentation and tests

- Add the normative learning-integrity contract and operator runbooks for
  retention and webhook dead-letter handling.
- Add multi-domain, domain-neutral diagnostic, assessment rollback,
  idempotency, DST/availability, curriculum-history, retention, retry and
  property/fuzz coverage across SQLite and PostgreSQL migrations.

## [0.4.1] — 2026-07-30

### Security

- Require Go 1.25.12 and `golang.org/x/text` 0.39.0, make `govulncheck`
  blocking in CI, and pin the scanner to v1.3.0.
- Consume authorization codes atomically, rotate refresh tokens in one
  transaction, store new refresh tokens as SHA-256 digests, and reject
  malformed OAuth response types, scopes, redirect URIs, and registration
  metadata.
- Scope cognitive state, interactions, misconceptions, calibration, and
  transfer evidence by learner and domain so identical concept labels in
  unrelated subjects cannot share or leak progress.
- Reject non-evidence activity types before `record_interaction` mutates
  BKT/FSRS/IRT state.

### Changed

- Add a real PostgreSQL 17 CI job for the DB suite.
- Add ordered, checksummed PostgreSQL upgrades after the frozen consolidated
  schema, including domain-scoping and legacy-evidence backfills.
- Fail startup on invalid public origins, unknown scheduler/rate-limit modes,
  PostgreSQL rate limits paired with SQLite, or incomplete distributed-mode
  prerequisites. SQLite now creates the configured `DB_PATH` parent rather
  than a hard-coded `./data` directory.
- Define SQLite single-node as the supported MVP profile and PostgreSQL
  multi-node as experimental pending shared narrative memory, crash-safe
  scheduler/outbox semantics, and failure/load validation.

### Fixed

- Compute FSRS retention from the current time and `LastReview` throughout the
  decision pipeline instead of reusing a stale persisted age.
- Make domain creation/concept extension atomic and add conservative legacy
  evidence backfills that leave ambiguous shared concept labels unassigned.
- Preserve streaming capabilities through HTTP middleware and clear the write
  deadline only for MCP responses, so long-lived SSE connections survive the
  ordinary endpoint timeout.
- Serialize concurrent narrative-memory updates per learner path and make
  writes durable with file and parent-directory syncs around atomic rename.
- Surface required storage failures and explicitly report optional degraded
  enrichments instead of silently returning partial pedagogical state.
- Remove the obsolete legacy activity router and unreachable helpers; archive
  its design documents as non-normative history.

### Tests

- Add a black-box official MCP client test over authenticated Streamable HTTP
  covering initialization, prompt/tool discovery, and a persisted
  `init_domain` → `get_next_activity` → `record_interaction` loop.
- Add shared-label domain-isolation, OAuth concurrency, FSRS time-progression,
  narrative-memory concurrency, startup configuration, and PostgreSQL
  migration regression coverage.

## [0.4.0] — 2026-06-06

### Added

- **PostgreSQL backend (opt-in) for horizontal scaling.** SQLite stays the
  default (`DB_DRIVER=sqlite`, embedded, no external service); set
  `DB_DRIVER=postgres` + `DATABASE_URL` to run on Postgres via the pure-Go pgx
  driver. The persistence layer was inverted behind a `store.Store` port
  (`engine`/`tools`/`auth` no longer import `db`) with a single conformance
  suite replayed against both backends so behaviour stays equivalent. Includes
  a dialect-aware schema (`schema_pg.sql`), `?`→`$N` rebind, RETURNING/ON
  CONFLICT handling, and `DB_MAX_CONNS` pool tuning.
- **Experimental multi-node building blocks.** `SCHEDULER_MODE=distributed`
  leases each scheduled run slot in the DB (`ClaimJobRun`) so at most one fleet
  instance wins it; `RATELIMIT_BACKEND=postgres` (alias `db`) moves rate-limit
  and login-failure counters into a shared store. This release did not provide
  shared narrative memory or crash-safe exactly-once delivery. Concurrent
  cold-start migrations are serialized with a Postgres advisory lock.
- **Row-level write concurrency on Postgres.** The `record_interaction` hot path
  uses `SELECT … FOR UPDATE` and the webhook queue uses `SKIP LOCKED`, giving
  the same lost-update protection on Postgres that `BEGIN IMMEDIATE` provides on
  SQLite.

### Fixed

- **(postgres)** Fix a first-touch lost update on `concept_states` under
  concurrency on Postgres: the very first interaction on a brand-new
  (learner, concept) found no row, so `SELECT … FOR UPDATE` locked nothing and
  two concurrent transactions both bootstrapped and one overwrote the other.
  New `GetOrCreateConceptStateForUpdate` materializes the row
  (`INSERT … ON CONFLICT DO NOTHING`) before locking; `applyInteraction` uses
  it. Guarded by `TestFirstTouchNoLostUpdate` on both backends.
- **(postgres)** Prevent a nil-pointer panic when a method that opens its own
  transaction (`DeleteDomain`, `ConsumeAuthCode`, `MergeDomainGoalRelevance`,
  the learning-negotiation overrides, the shared rate limiter) is invoked from
  inside a `WithTx` callback. A new `inTx` helper composes onto the current
  transaction instead of dereferencing the nil root pool. Guarded by
  `TestNestedTxComposition`.
- **(postgres)** Give the Postgres migration path the same anti-drift guard as
  SQLite: `MigratePostgres` records the schema checksum in `schema_migrations`
  and refuses to boot if `schema_pg.sql` changed since it was applied (rather
  than silently no-op'ing under `CREATE … IF NOT EXISTS`). Guarded by
  `TestMigratePostgresDetectsChecksumDrift`.

- **(security)** Transactionalize `record_interaction` to prevent lost updates
  on `concept_states` under concurrent calls on the same (learner, concept)
  pair. New `Store.BeginTx` + Tx-variant helpers; DSN now carries
  `_txlock=immediate` so every `database/sql` `Begin` maps to `BEGIN IMMEDIATE`
  on SQLite (R008).
- **(security)** Persist OAuth client approvals in
  `learner_approved_clients(learner_id, client_id, redirect_uri)` so a
  returning login no longer re-prompts and the consent screen retains its
  trust-on-first-use guarantee. Cap `client_name` at 120 bytes server-side
  (R001).
- **(security)** Fold learner emails to lowercase + trim at the handler so the
  per-account login-failure tracker cannot be bypassed by rotating case
  permutations, and so register-mode rejects case-variant duplicates. Adds
  migrations `0009_lowercase_learner_emails` and
  `0010_index_learners_email_lower` (functional UNIQUE index on `lower(email)`
  as defence-in-depth) (R002).
- **(security)** Bump the Go toolchain from 1.25.6 to 1.25.10. Closes 12
  upstream stdlib advisories, including four `html/template` XSS variants that
  touched `auth/pages.go`, the TLS 1.3 KeyUpdate DoS, and the `net/url` IPv6
  host misparse that backstopped `validateRegistrationRedirectURIs` (R035).
- **(security)** Reject `JWT_SECRET` values that decode to fewer than 32 bytes
  (256 bits) in `auth.LoadJWTSecret`, so a short/low-entropy secret can no
  longer silently weaken HS256 token signing.
- **(security)** Stop leaking raw internal/SQLite error strings into LLM-facing
  tool responses. New `tools.safeErrorResult` logs the underlying error
  server-side and returns a clean public message; all 31 `errorResult(fmt.
  Sprintf("…: %v", err))` sites were converted.
- **(security)** Validate the target concept against the resolved domain in
  `feynman_challenge` and `transfer_challenge` (`resolveDomain` +
  `validateConceptInDomain`), matching `record_interaction` and preventing
  operations on concepts from archived/other domains of the same learner.
- Bound webhook retry time in the in-process scheduler: cap an honored 429
  `Retry-After` at 5s (was up to 60s) and interrupt in-flight backoff sleeps on
  shutdown via a `stopCh`, so a few dead webhook URLs can no longer stall a cron
  tick for minutes or exhaust the `Stop()` drain budget.
- Guard `algorithms.InitialStability` / `InitialDifficulty` against an
  out-of-range FSRS `Rating` (clamp to `[Again..Easy]`), removing an
  `index -1` panic risk on `Rating(0)`.
- Cap the SQLite connection pool (`SetMaxOpenConns(1)` + idle/lifetime tuning in
  `db.OpenDB`) so concurrent writers queue on the single writer rather than
  surfacing `SQLITE_BUSY` past the busy-timeout.
- Make `engine.ComputeAlerts` testable/deterministic via a new
  `ComputeAlertsAt(…, now)` seam; `ComputeAlerts` is now a thin `time.Now()`
  wrapper, leaving all existing call sites untouched.
- Give the learner-memory context-budget eviction a one-session floor so the
  graduated frontmatter/body trimming stages actually fire instead of the loop
  dropping every session whole (previously the later stages were dead code).
- Remove N+1 query patterns in `db.GetMisconceptionGroups` (set-based
  window-function fetch) and `db.ConceptMasteryDelta` / `MilestonesInWindow`
  (single grouped/`IN` queries).

### Changed

- Extract the review-mode concept selector (`SelectReviewConcept` and helpers)
  out of `engine/orchestrator.go` into `engine/concept_selector.go`; pure code
  move, behavior unchanged.
- Replace the undocumented BKT tuning magic numbers in
  `algorithms/individual_bkt.go` with named, commented constants; collapse the
  vestigial `MasteryBKT()` branch.

### Tests

- Add direct coverage for previously thin/zero-covered paths: `applyInteraction`
  (BKT→FSRS→IRT read-modify-write), OAuth client-consent persistence and the
  learning-negotiation override pair, `engine/nudge_planner`, the memory
  markdown section replacer, and context-budget eviction.

### Documentation

- Document the single-node / in-memory state operational constraints (rate
  limiter, login-failure tracker, in-process scheduler reset on restart and do
  not scale horizontally with shared throttle expectations) in `OPERATIONS.md`.
- Condense `README.md` from 603 lines / 45 KB to 259 lines / 17 KB. Collapse
  the three overlapping narrative pillars into one, replace per-tool walls of
  text with grouped compact tables, and align with the live code: add
  `set_domain_priority` to the tools list, document `TRUSTED_PROXY_CIDRS` /
  `MCP_RATE_LIMIT_PER_MIN` / `MCP_RATE_LIMIT_BURST`, soften the `JWT_SECRET`
  enforcement claim to match `auth/jwt.go`, fix the `#setup` anchor.

## [0.3.1] — 2026-05-14

### Added

- Learner memory package with markdown-backed episodic sessions, stable memory,
  pending observations, concept notes, archives, atomic writes, YAML session
  frontmatter parsing, and `TUTOR_MCP_MEMORY_ROOT` / `TUTOR_MCP_MEMORY_ENABLED`.
- Memory MCP tools: `update_learner_memory`, `read_raw_session`, and
  `get_memory_state`.
- `episodic_context` and `reasoning_request` in `get_next_activity` so the
  client LLM can produce an interpretation brief before generating the activity.
- `interpretation_brief` storage on pedagogical snapshots and replay summaries.
- Client-initiated consolidation: the server enqueues due monthly, quarterly,
  and annual jobs in `pending_consolidations`, attaches `consolidation_request`
  to `get_next_activity`, and marks jobs completed when the client writes the
  archive through `update_learner_memory`.
- `--version`, `-version`, and `version` CLI output for release binaries.

### Changed

- Consolidation no longer performs any server-side LLM or archive generation.
  The connected MCP client authors the archive using its own LLM session.
- OLM fallback focus reason and webhook/nudge runtime copy are normalized to
  English.
- Discord OLM push filtering now suppresses plain KST fallback pushes unless
  recent narrative memory gives the message real learning value.

## [0.3.0-alpha.1] — 2026-05-08

### Refreshed 2026-05-09 — QA hardening pass

Binaries re-cut from `0149a74` to ship 17 post-tag fixes from a focused QA
review. No API breakage, no migration required, drop-in replacement for the
2026-05-08 refresh.

#### Cross-domain leak fixes (the headline)

The orchestrator's concept-state list is learner-wide; two of the three phase
selectors and two metacognitive read tools were not re-filtering by the active
domain, so a concept mastered/in-progress in domain A could surface as the
suggested activity (or as input to the autonomy/mirror computation) for
domain B. Closed in [#131](https://github.com/ArnaudGuiovanna/tutor-mcp/pull/131):

- `selectDiagnostic` and `selectMaintenance` now restrict candidates to the
  active domain's `graph.Concepts` (closes #93, #130).
- `get_metacognitive_mirror` and `get_autonomy_metrics` now honor `domain_id`
  by filtering interactions and concept states (closes #95).
- Side benefit: `selectDiagnostic` now also respects the gate's anti-repeat
  window, which it was silently bypassing.
- `selectInstruction` was already structurally safe via `externalFringe`.

#### Auth, scheduler, migration, perf

- **OAuth confidential clients can complete `/token` without PKCE**
  ([#128](https://github.com/ArnaudGuiovanna/tutor-mcp/pull/128), closes #114).
  PKCE check at `/token` is now conditional on the auth code having been stored
  with a non-empty `code_challenge` — public clients still require PKCE
  (regression-tested), confidential clients authenticate via `client_secret`
  (bcrypt-verified, unchanged).
- **Scheduler shutdown drains cron jobs**
  ([#129](https://github.com/ArnaudGuiovanna/tutor-mcp/pull/129), closes #123,
  #124). `Scheduler.Stop()` now blocks on `cron.Stop()`'s drain context with a
  25 s deadline, so in-flight webhook retries finish before `database.Close()`
  on SIGTERM.
- **Migration runner is now atomic per migration**
  ([#127](https://github.com/ArnaudGuiovanna/tutor-mcp/pull/127), closes #118).
  `applyMigration` wraps the DDL body and the `schema_migrations` insert in a
  single transaction so a partial failure can no longer leave the DB and the
  bookkeeping table in disagreement. The `IgnoreExecErrors` legacy path still
  records its row.
- **`get_next_activity` drops a redundant `GetDomainByID` re-read**
  ([#113](https://github.com/ArnaudGuiovanna/tutor-mcp/pull/113), closes #91).
  New `engine.OrchestrateWithPhase` surfaces the resolved phase from
  in-memory; the legacy `engine.Orchestrate` is a thin wrapper so all existing
  callers are byte-identical.

#### Validation hardening

Twelve PRs landed earlier in the day to close the input-validation gaps QA had
flagged on chat-side LLM-driven tools:

- `actual_score`, `predicted_mastery`, `calibration_bias`, `autonomy_score`
  now reject NaN, Inf, and out-of-range values (closes #83, #85).
- `update_learner_profile` distinguishes "not provided" from "explicit 0"
  via `*float64`, so `calibration_bias=0` (perfect calibration) no longer
  vanishes through `omitempty` (closes #89).
- `activity_type` and `error_type` enforced as enums in `record_interaction`
  (closes #88).
- `learner_concept`, `concept_id`, and `context_type` validated against the
  active domain's graph in `learning_negotiation` and `record_transfer_result`
  (closes #92, #96).
- Unbounded string fields capped across remaining handlers (closes #82).
- `resolveDomain` now rejects archived domains (closes #94).
- `get_dashboard_state` aligned to the codebase's English error-string
  convention (closes #90).
- DB-layer ownership filter on calibration record helpers (closes #87).

#### Tests, docs, observability

- New end-to-end coherence regression suite covering scenarios 5-9 of #97.
- New `docs/mvp-checklist.md` MVP exit-criteria tracker.
- Disambiguated routing descriptions on `domain_id`-aware tools.
- 22 open issues bootstrapped with p0/p1/p2 priority labels (#81).

### Initial public alpha

Tutor MCP exposes a deterministic Intelligent Tutoring System runtime over the
Model Context Protocol so any compatible LLM (Claude, ChatGPT, Le Chat, Gemini)
can drive an adaptive learning loop without an editorial team.

#### Highlights

- **Cognitive engine** — BKT (mastery), FSRS (spaced repetition), IRT (ability),
  PFA (plateau detection), KST (prerequisite gating), plus a separate
  Rasch/Elo calibration signal for learner ability vs. exercise difficulty.
  The learner model updates on every interaction. The BKT → FSRS → IRT chain
  runs against a single read-only snapshot so order-of-evaluation no longer
  leaks cross-step state.
- **Regulation pipeline (v0.3)** — seven-stage pipeline: Threshold Resolver,
  Goal Decomposer, Action Selector, Concept Selector, Gate Controller, Phase
  Controller (DIAGNOSTIC ↔ INSTRUCTION ↔ MAINTENANCE FSM), Fade Controller.
  All shipped except FadeController which is opt-in via `REGULATION_FADE=on`.
- **Surface** — chat-only. 28 MCP tools across cognitive state, domain
  management, metacognition (mirror, calibration, autonomy), motivation
  (utility-value, growth-mindset, OLM narrative), and webhook nudges
  (Discord-targeted).
- **OAuth 2.1 + PKCE** — confidential and public client support, refresh-token
  rotation with client binding (per RFC 6749 §10.4 / RFC 9700 §2.2), per-IP
  and per-account rate limiting, bcrypt cost 12, password floor 12 chars.
- **Storage** — SQLite via modernc.org/sqlite (CGo-free), versioned migrations
  with SHA-256 checksum drift detection, idempotent CREATE TABLE / ALTER
  TABLE pipeline.
- **Observability** — structured slog, decision-trace logs through the
  regulation pipeline, scheduler with 6 cron jobs (OLM, motivation, recap,
  mirror, cleanup, metacognitive alerts).

#### Known limitations

Three algorithmic refinements deferred for a later release:

- **PFA fidelity** ([#48](https://github.com/ArnaudGuiovanna/tutor-mcp/issues/48))
  — the plateau detector follows a project-specific convention rather than
  Pavlik (2009) verbatim (sign of ρ, β intercept, decay term).
- **IRT statistical robustness** ([#49](https://github.com/ArnaudGuiovanna/tutor-mcp/issues/49))
  — pure MLE saturates θ on extreme response strings; no EAP/MAP prior.
- **FSRS sub-day intervals** ([#52](https://github.com/ArnaudGuiovanna/tutor-mcp/issues/52))
  — Learning/Relearning steps are day-granular; hours not yet supported.

#### Operational notes

- Forward-only migrations. A schema body change after apply surfaces as
  `checksum mismatch` at startup; manual operator action is required to reset.
  This is intentional — drift requires intervention, not a silent retry.
- The per-IP rate limiter assumes `TRUSTED_PROXY_CIDRS` is set in any public
  deployment behind a reverse proxy. Tailscale Funnel users can ignore the
  startup warning — the funnel terminates TLS locally and the rate-limiter
  collapses to a single global bucket by design.
- GitHub Actions runs the SQLite suite, PostgreSQL DB suite, cross-platform
  builds, vet, and a blocking vulnerability scan. Contributors should still run
  `go build ./... && go test ./...` before opening a PR.

#### Compatibility

- Tested with Claude Desktop and Claude.ai (custom connectors).
- Go 1.25+ required.
- SQLite >= 3.35 (DROP COLUMN support).

[Unreleased]: https://github.com/ArnaudGuiovanna/tutor-mcp/compare/v0.4.1...HEAD
[0.4.1]: https://github.com/ArnaudGuiovanna/tutor-mcp/releases/tag/v0.4.1
[0.4.0]: https://github.com/ArnaudGuiovanna/tutor-mcp/releases/tag/v0.4.0
[0.3.1]: https://github.com/ArnaudGuiovanna/tutor-mcp/releases/tag/v0.3.1
[0.3.0-alpha.1]: https://github.com/ArnaudGuiovanna/tutor-mcp/releases/tag/v0.3.0-alpha.1
