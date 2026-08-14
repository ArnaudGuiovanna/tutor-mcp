<p align="center">
  <img src="docs/banner.svg" alt="tutor/mcp — Self-learning is a superpower." width="100%" />
</p>

<p align="center">
  <a href="./LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License: MIT" /></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/go-1.25+-00ADD8.svg?logo=go&logoColor=white" alt="Go 1.25+" /></a>
  <a href="https://modelcontextprotocol.io/"><img src="https://img.shields.io/badge/MCP-server-7c3aed.svg" alt="MCP server" /></a>
  <a href="https://github.com/ArnaudGuiovanna/tutor-mcp/releases"><img src="https://img.shields.io/badge/release-v0.4.1-orange.svg" alt="Release v0.4.1" /></a>
  <a href="https://github.com/ArnaudGuiovanna/tutor-mcp/issues"><img src="https://img.shields.io/badge/status-alpha-yellow.svg" alt="Status: alpha" /></a>
</p>

# Tutor MCP — Adaptive learning runtime for LLMs

> Give any MCP-capable LLM the state and tools to act as an **adaptive tutor**. Tutor MCP is an open-source [MCP](https://modelcontextprotocol.io/) server that adds durable learner state, review scheduling, session memory, misconceptions, metacognition, and auditable pedagogical decisions. No item bank — the LLM generates content, Tutor MCP remembers and recommends.

Tell the LLM what you want to learn — *Spanish for travel*, *Go for backend*, *medieval history* — and the runtime orchestrates the journey: what to study next, when to review, when you've mastered a concept, when you need a nudge. The next conversation starts from what the learner has mastered, forgotten, misunderstood, felt, and explicitly committed to do next.

**Status — alpha v0.4.1.** Two profiles are supported: local SQLite with one
process, and horizontal multi-tenant SaaS on PostgreSQL with separate
API/worker/migrator roles, forced RLS, durable outbox/jobs, encrypted shared
narrative memory, quotas, audit and recovery runbooks. The regulation pipeline
(phase FSM + concept/action selectors + gate + threshold resolver) ships
default-on; the fade controller is opt-in. Direct Discord remains at-least-once
with quarantine of ambiguous outcomes; signed SaaS webhooks expose a stable
deduplication contract.

## Compatible clients

<p align="left">
  <a href="#setup"><img src="docs/assets/logos/claude.svg" width="32" height="32" alt="Claude" title="Claude" /></a>
  &nbsp;&nbsp;
  <a href="#setup"><img src="docs/assets/logos/openai.svg" width="32" height="32" alt="ChatGPT" title="ChatGPT" /></a>
  &nbsp;&nbsp;
  <a href="#setup"><img src="docs/assets/logos/mistral.svg" width="32" height="32" alt="Le Chat" title="Le Chat (Mistral)" /></a>
  &nbsp;&nbsp;
  <a href="#setup"><img src="docs/assets/logos/gemini.svg" width="32" height="32" alt="Gemini" title="Gemini" /></a>
</p>

Claude (web + Desktop + Code), ChatGPT (Developer Mode), Le Chat, Gemini Enterprise / CLI. See the [client setup guide](#setup) below.

The protocol-level target is any MCP client that supports remote Streamable HTTP plus the OAuth dynamic-registration/PKCE flow used here. Pedagogical continuity also depends on the host LLM following the supplied prompt and consistently calling `get_next_activity` and `record_interaction`; the server cannot force a client to make those calls.

## Continuity model

The missing layer is not content. It is continuity.

LLMs can explain. Tutor MCP remembers and decides. The runtime owns the durable learner state and the pedagogical decisions; the LLM stays free to explain, reframe, question, generate exercises, and consolidate narrative memory from the traces it receives.

| Layer | Stored as | What it gives the tutor |
|---|---|---|
| **Algorithmic state** | SQLite local or PostgreSQL SaaS: tenant/enrollment-scoped domains, concept states, interactions, affect, calibration, transfer, intentions | Domains, prerequisites, phase, mastery, retention, ability, review timing, transfer readiness, active misconceptions |
| **Episodic memory** | Markdown `sessions/*.md` with YAML frontmatter | Affect, concepts touched, salient exchanges, mental-model observations, implementation intentions |
| **Narrative state** | Markdown `MEMORY.md`, `MEMORY_pending.md`, `concepts/*.md`, `archives/*.md` | Stable learner facts, pending observations, concept notes, medium-term trajectory, contradictions to verify |
| **Operator view** | Pedagogical snapshots + decision replay | Why an activity was selected, why a concept was held back, whether evidence was missing or noisy |

`get_next_activity` merges the algorithmic signals with `episodic_context`: stable memory, pending observations, recent sessions, archives, concept notes, and detected OLM inconsistencies. The LLM receives enough context to form a brief hypothesis about the learner's current cognitive state, but it does not own the schedule.

## How it works

The server sits between a learner and an LLM. It splits the job cleanly:

| Component | Owns | Does not own |
|---|---|---|
| **Deterministic engine — Tutor MCP** | Cognitive signals, phase control, evidence gates, session history, Markdown learner memory, audit trail | Learner-facing prose, examples, Socratic phrasing |
| **Generative coach — your LLM** | Content generation, natural language coaching, interpretation briefs, session summaries, memory consolidation | Durable mastery state, review timing, prerequisite gates |

Four loops run from the first session:

- **Learning loop** — Before and after every exchange, the LLM calls `get_next_activity` and `record_interaction`. The runtime updates BKT mastery, FSRS recall, regularized cumulative IRT ability, transfer evidence and misconception status — in real time, on every interaction. The LLM never picks scheduling itself.
- **Narrative memory loop** — `record_session_close` asks the LLM for a factual session trace; `update_learner_memory` stores stable memory, pending observations, concept notes, sessions and archives. The next `get_next_activity` call can use those traces to avoid a generic exercise.
- **Metacognitive loop** — Affect check-ins (`record_affect`), calibration tracking (`calibration_check` / `record_calibration_result`) and an autonomy score observe the learner's relationship to the system. A factual mirror surfaces consolidated dependency patterns — the system aims to make itself progressively unnecessary.
- **Motivation loop** — A brief engine selects one motivational angle per exercise (milestone, competence value, growth mindset, affect reframe, plateau recontext, utility value) and emits *signals + instruction* — never canned text. The LLM phrases it.

The pillars of an Intelligent Tutoring System map cleanly:

| ITS pillar | Owner |
|---|---|
| **Domain model** (concept graph, prerequisites) | Tutor MCP runtime — KST-validated |
| **Learner model** (mastery, ability, recall, transfer) | Tutor MCP runtime — BKT, regularized online IRT, PFA |
| **Pedagogical model** (scheduling, regulation, alerts) | Tutor MCP runtime — FSRS, evidence gates, orchestrator |
| **Interface + content** | The LLM — Claude / ChatGPT / Le Chat / Gemini |

These models are deterministic and auditable routing heuristics, not a claim of psychometric or clinical validation. Their observations still depend on the LLM calling the tools and scoring the learner faithfully. This separation makes Tutor MCP useful on a new topic without requiring a pre-authored item bank, while keeping the limits of that generality explicit.

## Quick start

### 1. Install or build

```bash
# Latest Linux release (no sudo: set TUTOR_MCP_INSTALL_DIR)
curl -fsSL https://tutor-mcp.dev/install.sh | sh

# Or build from source
go build -o tutor-mcp
```

### 2. Run

```bash
export JWT_SECRET="$(openssl rand -base64 32)"   # development compatibility only
export BASE_URL=https://your.domain              # public origin, no trailing slash
export SMTP_ADDR=smtp.example.com:587            # STARTTLS is mandatory
export SMTP_FROM=tutor@your.domain
export INTEGRATION_SECRET_KEYS="v1:$(openssl rand -base64 32)"
export INTEGRATION_SECRET_CURRENT_KEY_ID=v1
./tutor-mcp                                       # listens on :3000 by default
```

Verify liveness with `curl $BASE_URL/live`; use `$BASE_URL/ready` for the
load-balancer readiness probe (it includes a bounded database check).

For real use, put the runtime behind a public reverse proxy with TLS — see [OPERATIONS.md](./OPERATIONS.md). Web clients (Claude.ai, ChatGPT, Le Chat) require a public HTTPS endpoint; `http://localhost` is rejected by their cloud connectors.

### 3. Connect a client

Add `https://your.domain/mcp` as a custom MCP connector. OAuth 2.1 + PKCE with
CIMD first and bounded dynamic registration as a compatibility fallback means
there is no client ID or secret to copy by hand. On first connection, the
client opens `/authorize`: register, verify the short-lived email link, then
return automatically to the client. Subsequent launches rotate refresh tokens
silently.

<a id="setup"></a>

| Client | Path | Notes |
|---|---|---|
| **Claude.ai** | Settings → Connectors → + → URL `https://your.domain/mcp` | Pro, Max, Team, Enterprise |
| **ChatGPT** | Settings → Connectors → Advanced → Developer Mode → Create | Plus, Pro, Team, Enterprise, Edu |
| **Le Chat** | Connectors → + Add Connector → Custom MCP | Auto-detects OAuth |
| **Gemini Enterprise** | GCP Console → Custom MCP server data store | StreamableHTTP transport |
| **Gemini CLI** | [`geminicli.com/docs/tools/mcp-server/`](https://geminicli.com/docs/tools/mcp-server/) | Local CLI |
| **Claude Code** (CLI, local) | `.mcp.json` with `"url": "http://localhost:3000/mcp"` | No HTTPS needed |

## MCP tools (45)

Domain-scoped learning tools accept an optional `domain_id`; where documented, omitting it selects the most recently active non-archived domain. Learner-global and lifecycle tools intentionally have different contracts—use each tool's schema as the source of truth.

Every mutation schema exposes an optional `idempotency_key` (the equivalent
`_meta.idempotency_key` is also accepted). Reusing the same learner/tool/key
with canonically equivalent arguments replays the first successful response; reusing
it with different arguments is rejected. Hosts should generate a fresh key for
each logical mutation and retain it across transport retries. If an operator
opts into cached-response retention, an expired response produces an explicit
already-completed error; the durable key and request hash remain, so the tool
handler is never executed again for that key.

### Core learning loop (11)

| Tool | Purpose |
|---|---|
| `start_learning_session` | Idempotently open/resume the durable session ID shared by interactions, affect, transfer, intentions, assessments and summaries |
| `get_learner_context` | Session-start context: active domain, concept states, recent history, active misconceptions |
| `get_pending_alerts` | Learning + metacognitive alerts requiring action |
| `get_next_activity` | Next optimal activity + episodic context + reasoning request + tutor mode + motivation brief + mastery uncertainty + transfer profile |
| `prepare_assessment_attempt` / `submit_assessment_attempt` / `cancel_assessment_attempt` | Freeze task/rubric before the response, commit the response before evaluation, or explicitly cancel the attempt |
| `record_interaction` | Persist an observation and update BKT/FSRS/IRT; unlinked practice stays explicitly unverified, while retention/demonstration/transfer evidence references a submitted/evaluated attempt |
| `check_mastery` | Mastery-challenge readiness: BKT + evidence diversity + uncertainty + transfer status |
| `get_olm_snapshot` | Open Learner Model: evidence-backed stages per concept — estimated, retained, demonstrated and transferred |
| `get_dashboard_state` | Evidence-backed progress (estimated/retained/demonstrated/transferred), routing state, retention, autonomy, calibration bias and affect history |

### Domain management (12)

| Tool | Purpose |
|---|---|
| `init_domain` | Create domain with concept graph, prerequisites, personal goal, and immutable curriculum version 1 |
| `add_concepts` | Append concepts with `expected_version` CAS; optional outcomes/level/criteria metadata; progress is not reset |
| `get_curriculum_snapshot` | Read latest/historical immutable versions, stable concept IDs, outcomes, criteria, provenance and review state |
| `publish_curriculum_revision` | CAS-protected rename, metadata update, split, merge, or safe removal with a complete audit envelope |
| `validate_domain_graph` | Audit graph: cycles, orphans, depth, disconnections |
| `archive_domain` / `unarchive_domain` / `delete_domain` | Lifecycle; deletion is a runtime-hidden tombstone that preserves curriculum and learning evidence |
| `set_domain_priority` | Re-rank domains for scheduling weight |
| `mark_domain_high_stakes` | One-way safety classification; demonstrated claims and intrusive suggestions then require trusted human-reviewed evaluation |
| `set_goal_relevance` / `get_goal_relevance` | LLM-decomposed relevance vector over the concept graph (biases the concept selector) — gated by `REGULATION_GOAL` |

### Metacognition (6)

| Tool | Purpose |
|---|---|
| `record_affect` | Energy + confidence (start), satisfaction + difficulty + intent (end) |
| `calibration_check` / `record_calibration_result` | Self-prediction (1–5) + bias update |
| `get_autonomy_metrics` | Autonomy score 0–1 with 4 components (initiative, calibration, hint independence, proactive review) |
| `get_metacognitive_mirror` | Factual mirror message when a dependency pattern is consolidated over 3+ sessions |
| `update_learner_profile` | Persist learner metadata (objective, language, calibration bias, …) |

### Audit & replay (3)

| Tool | Purpose |
|---|---|
| `get_pedagogical_snapshots` | Before / observation / after / decision trace |
| `get_decision_replay_summary` | Offline audit: replay coverage, missing rubrics, transfer gaps, JSON issues |
| `get_misconceptions` | Per-concept misconceptions with status (active / resolved) and frequency |

### Transfer & negotiation (4)

| Tool | Purpose |
|---|---|
| `feynman_challenge` | Learner explains a high-estimate concept to deepen evidence; confirmed prerequisite gaps become versioned curriculum revisions |
| `transfer_challenge` / `record_transfer_result` | Generate structured probes across `near`/`far`/`debugging`/`teaching`/`creative`; the direct recorder is legacy/unverified, while evidence-bearing probes use assessment attempts |
| `learning_negotiation` | Expose system plan + tradeoffs; learner can propose alternatives |

### Memory & session (7)

| Tool | Purpose |
|---|---|
| `update_learner_memory` / `read_raw_session` / `get_memory_state` | Markdown memory: sessions, concepts, stable memory, archives |
| `record_session_close` | Idempotently close the durable session + recap brief + optional Gollwitzer if-then implementation intention |
| `list_implementation_intentions` / `update_implementation_intention` | Inspect and resolve commitments through pending/honored/missed/cancelled states |
| `queue_webhook_message` | Queue a structured Discord nudge (`why_now`, `learning_gain`, `open_loop`, `next_action`) |

### Availability (2)

| Tool | Purpose |
|---|---|
| `get_availability_model` | Learner-owned IANA timezone, weekly local windows, DND, consent/frequency/cap, accessibility preferences and policy version |
| `update_availability_model` | Optimistic, ownership-scoped replacement of availability/accessibility policy; concurrent stale writes are rejected |

### Alert engine

The scheduler detects nine alert types — learning (`FORGETTING`, `PLATEAU`, `ZPD_DRIFT`, `OVERLOAD`, `MASTERY_READY`) and metacognitive (`DEPENDENCY_INCREASING`, `CALIBRATION_DIVERGING`, `AFFECT_NEGATIVE`, `TRANSFER_BLOCKED`). `MASTERY_READY` means that attempt-linked retained, varied evidence is sufficient to *attempt* a challenge; it is not a demonstrated-mastery claim, and the same recent trusted transfer failure that blocks `check_mastery` suppresses the alert. Every delivery rechecks explicit consent, DND, the learner's DST-aware local window, frequency and local-day cap through an atomic reservation. Archived/deleted domains are filtered out. Unreviewed high-stakes domains cannot produce demonstrated claims or intrusive suggestions; only trusted `human_review` assessment evidence opens that gate, and the runtime does not invent an external reviewer.

## Cognitive science engine

Pure-function algorithms running on every interaction, composed by the regulation orchestrator (`engine/orchestrator.go`; design notes in [`docs/regulation-design/`](./docs/regulation-design/)).

| Algorithm | Role |
|---|---|
| **BKT** + individualized BKT | Estimates mastery confidence per concept, not just whether the learner answered right today; recent-history profile individualizes `P(Learn)`, `P(Slip)`, `P(Guess)` — never tuned by the LLM |
| **FSRS** | Decides when to bring a concept back, using stability and difficulty curves |
| **IRT** | Tracks learner ability θ with a regularized cumulative online update so one binary response cannot saturate the estimate |
| **PFA** | Weighs wins and losses on each concept to predict how the next attempt is likely to go |
| **KST** | Validates prerequisite graph; gates new concepts on mastery of ancestors |
| **Structured transfer** | Checks whether knowledge moves beyond the training pattern across `near`/`far`/`debugging`/`teaching`/`creative` probes |

The **regulation pipeline** runs as a 7-stage chain inside `get_next_activity`: threshold resolver → goal decomposer → phase FSM (`DIAGNOSTIC ↔ INSTRUCTION ↔ MAINTENANCE`) → concept selector → gate (anti-repeat / session-budget / no-fringe escape) → action selector → fade controller. Pure functions are unit-tested (~90 tests); the orchestrator integration is covered by SQLite in-memory + migration tests. Full design rationale in [`docs/regulation-design/`](./docs/regulation-design/).

## Configuration

Environment variables read at boot:

| Variable | Default | Effect |
|---|---|---|
| `JWT_ED25519_KEYS` | — *(required in production)* | JSON keyring with one active Ed25519 private key and current/previous public keys (`kid`, base64 standard encoding). Enables overlap rotation and JWKS publication. |
| `JWT_SECRET` | — *(development fallback)* | Legacy HS256 compatibility secret. Base64, 32+ decoded bytes. Rejected as the sole production signing configuration. |
| `DEPLOYMENT_PROFILE` | `development` | `production` fails closed unless the public origin is HTTPS, PostgreSQL uses verified TLS with an explicit CA, shared rate limits, SMTP, integration-secret encryption and trusted proxy CIDRs are configured. |
| `PROCESS_ROLE` | `all` in development | `api`, `worker`, or `migrator` is mandatory in production. Only the migrator applies DDL. |
| `PORT` | `3000` | HTTP listen port |
| `DB_DRIVER` | `sqlite` | `sqlite` for the local profile or `postgres` for horizontal SaaS. |
| `DB_PATH` | `./data/runtime.db` | SQLite path (ignored when `DB_DRIVER=postgres`) |
| `DATABASE_URL` | — | Postgres DSN, **required** when `DB_DRIVER=postgres`. Production requires `sslmode=verify-full` and an explicit `sslrootcert` CA. |
| `DB_MAX_CONNS` | `10` | Postgres connection-pool size per instance (ignored on SQLite). Keep `DB_MAX_CONNS × instances < Postgres max_connections`. |
| `SCHEDULER_MODE` | `inprocess` | `inprocess` (recommended) or `distributed` (fenced recoverable leases, heartbeat, retry and DLQ). Distributed mode requires PostgreSQL, PostgreSQL rate limits, and shared database narrative memory or disabled memory. Unknown values fail at boot. |
| `RATELIMIT_BACKEND` | `memory` | `memory` (per-instance, default) or `postgres`/`db` (shared rate-limit + login-failure store). The PostgreSQL backend requires `DB_DRIVER=postgres`; unknown values fail at boot. |
| `BASE_URL` | `http://localhost:$PORT` | Public HTTP(S) origin. A trailing `/` is normalized; paths, credentials, query strings, fragments, and non-HTTP schemes fail at boot. Triggers HSTS when `https://`. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | Enables OTLP traces and metrics; standard trace/metric endpoints, resource attributes, headers and TLS variables are also accepted. |
| `TRUSTED_PROXY_CIDRS` | — | Comma-separated CIDRs of trusted reverse-proxies. **Required behind a public proxy** — without it every IP-rate-limit collapses under the proxy's loopback bucket. |
| `AUTH_BCRYPT_MAX_CONCURRENT` | `4` | Process-wide CPU budget shared by password and OAuth client-secret bcrypt work (1–128). Admission is non-blocking; saturation receives HTTP 503 with `Retry-After` instead of an unbounded goroutine queue. |
| `MCP_RATE_LIMIT_PER_MIN` | `60` | Per-IP and per-learner cap on `/mcp` |
| `MCP_RATE_LIMIT_BURST` | `60` | Burst allowance |
| `MCP_MAX_REQUEST_BODY_BYTES` | `1048576` (1 MiB) | Maximum POST body accepted by `/mcp`; values from 1 byte through 64 MiB are accepted. Oversized declared or chunked bodies receive HTTP 413. |
| `MCP_MAX_CONCURRENT` | `128` | Maximum in-flight authenticated MCP calls per process; overflow is rejected immediately. |
| `MCP_MAX_CONCURRENT_PER_LEARNER` | `8` | Maximum in-flight MCP calls for one learner; cannot exceed the global limit. |
| `MCP_TOOL_CALL_TIMEOUT_SECONDS` | `30` | Cooperative server deadline applied only to `tools/call` (1–600 seconds); discovery and long-lived transport responses are unaffected. |
| `OAUTH_GRANULAR_SCOPES` | **`off`** *(opt-in)* | `on` publishes and issues per-tool `learner:read` / `learner:write` grants; `off` keeps the bounded legacy `learner` compatibility mode. Only `on` and `off` are accepted. Use the [two-phase rollout and rollback runbook](./docs/oauth-granular-scopes-rollout.md). |
| `OAUTH_DCR_MODE` | `open` in development | `open`, `token`, or `disabled`. Production requires an explicit `token` or `disabled`; `disabled` removes `/register` from discovery and routing. See the [DCR production runbook](./docs/oauth-dcr-production.md). |
| `OAUTH_DCR_INITIAL_ACCESS_TOKEN` | — | Required only with `OAUTH_DCR_MODE=token`. Must be unpadded base64url encoding at least 32 bytes (for example, 64 hex characters from `openssl rand -hex 32`). Startup registers only its SHA-256 digest in the shared capability registry; changed values overlap until explicit audited revocation with `tutor-dcr-admin`. |
| `SMTP_ADDR` / `SMTP_FROM` | — | SMTP endpoint and sender for verification/recovery. Delivery requires STARTTLS (TLS 1.2+); public account flows cannot complete when absent. Optional `SMTP_SERVER_NAME`, `SMTP_USERNAME`, `SMTP_PASSWORD`. |
| `INTEGRATION_SECRET_KEYS` | — | Comma-separated `key_id:base64-32-byte-key` keyring used to encrypt Discord webhook credentials at rest. Supply old and new keys during rotation. |
| `INTEGRATION_SECRET_CURRENT_KEY_ID` | — | Key ID used for new envelopes; startup atomically re-encrypts legacy/old-key records. Required with `INTEGRATION_SECRET_KEYS`. |
| `TENANT_INTEGRATION_ALLOWED_HOSTS` | — | Comma-separated HTTPS host allowlist for signed tenant webhooks; mandatory in production. IP literals and non-443 ports are refused. |
| `TUTOR_MCP_MEMORY_ENABLED` | `on` | Enables narrative learner memory. Runtime concept notes and sessions are domain-scoped; ambiguous legacy/global narratives are excluded from activity generation. |
| `TUTOR_MCP_MEMORY_BACKEND` | `local` on SQLite, `database` on PostgreSQL | `local` Markdown or encrypted/versioned relational objects. Active distributed and production profiles require `database`. See the [migration and rotation runbook](./docs/narrative-memory-operations.md). |
| `TUTOR_MCP_MEMORY_ROOT` | `~/.tutor-mcp/` | Local backend root and create-only backfill source when switching to `database`. |
| `TUTOR_MCP_MEMORY_MAX_WRITE_BYTES` | `262144` | Maximum content supplied to one narrative-memory write. |
| `TUTOR_MCP_MEMORY_MAX_FILE_BYTES` | `1048576` | Maximum size of one narrative Markdown file, on reads and writes. |
| `TUTOR_MCP_MEMORY_MAX_LEARNER_BYTES` | `16777216` | Cumulative narrative-memory quota per learner. |
| `TUTOR_MCP_MEMORY_MAX_FILES_PER_LEARNER` | `2048` | Maximum Markdown files per learner. |
| `TUTOR_MCP_MEMORY_MAX_CONCURRENT_WRITES` | `32` | Maximum concurrent narrative writes per process; overflow receives backpressure. |
| `REGULATION_THRESHOLD` | `on` | `off` reverts to legacy split thresholds (BKT 0.85 / KST 0.70 / Mid 0.80) |
| `REGULATION_GOAL` | `on` | `off` hides `set_goal_relevance` / `get_goal_relevance` and drops the goal-aware prompt section |
| `REGULATION_ACTION` / `_CONCEPT` / `_GATE` | `on` | `off` drops the system-prompt appendix only — the selector / gate logic always runs |
| `REGULATION_FADE` | **`off`** *(opt-in)* | Strict literal `on` reduces motivational/hint verbosity and exposes advisory fade parameters. Scheduler frequency, ZPD target and proactive-review cadence are not yet wired to those advisory fields. |

Credential checks, registration, verification and recovery have distinct
rate-limit buckets. The MCP endpoint applies both per-IP/per-learner rates and
the in-flight concurrency ceilings configured above.
OAuth access tokens expire after 30 minutes. Refresh tokens rotate as a family;
replay of an already-used member revokes the family instead of issuing another
access token. Granular OAuth scopes are an opt-in fleet-wide change; follow the
[scope rollout runbook](./docs/oauth-granular-scopes-rollout.md) before enabling
them.

Data lifecycle cleanup is opt-in and runs through a separate dry-run-first
maintenance command; it is never triggered by server startup. See
[Data retention maintenance](./OPERATIONS.md#data-retention-maintenance) for
safe defaults, durable apply jobs, legal holds, backup proof, crash recovery
and restoration reconciliation.
Ambiguous outbound webhook attempts are quarantined and require the separate,
explicit `tutor-webhook-admin` command; they are never retried automatically.
See the [webhook delivery runbook](./docs/webhook-delivery-operations.md).
The product-level evidence, curriculum, session and learner-control invariants
are specified in the [learning integrity contract](./docs/learning-integrity.md).

## Architecture

```
main.go              HTTP + MCP handler + OAuth + scheduler
auth/                OAuth 2.1 + JWT + PKCE + rate limiter
algorithms/          BKT / FSRS / regularized IRT / PFA / KST + thresholds
engine/              Orchestrator + phase FSM + selectors + gate + fade
                     + alert / motivation / mirror / replay / OLM
models/              Typed structs (learner, domain, interactions, regulation, …)
db/                  Store (SQLite default + Postgres) + dialect-aware schema + checksummed migrations
memory/              Markdown learner memory (stable / pending / sessions / concepts / archives)
tools/               MCP tool handlers + system prompt + rubrics
```

The regulation engine is layered: **pure** decision components (`phase_fsm.go`, `concept_selector.go`, `action_selector.go`, `gate.go`) composed by an **impure** orchestrator (`orchestrator.go`). The same separation applies to the metacognition (autonomy, mirror, tutor mode) and motivation (brief selection) modules.

## Capacity & sizing

The local profile remains intentionally **single-node**: SQLite + in-process
scheduler, with no broker or external database. The SaaS profile uses
PostgreSQL, stateless API replicas and distributed workers.

| Profile | Active / day | Registered | Use case |
|---|---|---|---|
| **Personal** | 1 | 1–5 | Solo learning |
| **Small group** | 1–10 | up to 30 | Family / team |
| **Classroom** | 10–50 | up to 150 | Facilitated sessions |
| **Small org** | 50–200 | up to 600 | Sustained load |

Treat ~200 active learners as a planning ceiling for the default SQLite
profile, not a service-level guarantee. PostgreSQL capacity is quota- and
workload-dependent; validate it with the noisy-neighbour gate and the
[SaaS SLO](./docs/saas-slo.md). Deployment, rollback and recovery are in
[OPERATIONS.md](./OPERATIONS.md).

**Idle footprint**: ~30 MB RSS, ~15 MB binary, ~10 MB initial DB (+50 KB/active learner/month). Tested on Raspberry Pi 4 and €5/mo VPS for personal use.

## Tech stack

Go 1.25.13+ · [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) · [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) (pure-Go, no CGO, local default) · [jackc/pgx](https://github.com/jackc/pgx) (PostgreSQL SaaS, pure-Go) · OpenTelemetry · [robfig/cron](https://github.com/robfig/cron) · [golang-jwt/jwt](https://github.com/golang-jwt/jwt) · bcrypt.

## Pedagogical reliability

The runtime deliberately separates deterministic decisions from LLM coaching freedom: the runtime owns state transitions, thresholds, graph validation, evidence gates, scheduling and audit snapshots; the LLM owns examples, hints, feedback, tone and explanations. `record_interaction` accepts structured `rubric_json` / `rubric_score_json` and persists them on interactions + pedagogical snapshots. `get_decision_replay_summary` surfaces audit quality (missing rubrics, transfer gaps, JSON issues). A static goldset covers known failure modes (false-positive high BKT, missing rubrics, missing transfer, clean replay).

## Acknowledgments

Stands on the shoulders of: Corbett & Anderson (BKT, 1995), Open-Spaced-Repetition (FSRS), Lord & Novick (IRT, 1968), Pavlik et al. (PFA, 2009), Falmagne & Doignon (KST, 2011), Hidi & Renninger (interest phases, 2006), McClelland / McNaughton / O'Reilly (CLS-inspired memory layering, 1995).

## Operations · Security · Contributing · Roadmap

- **Operations** — local and SaaS deployment: [OPERATIONS.md](./OPERATIONS.md),
  [runtime horizontal](./docs/saas-runtime-operations.md),
  [SLO/alerting](./docs/saas-slo.md),
  [PITR and tenant restore](./docs/tenant-restore-runbook.md).
- **Security** — private disclosure channels and operator hardening checklist: [SECURITY.md](./SECURITY.md). Do not open public issues for vulnerabilities.
- **Contributing** — fork, branch from `staging`, conventional commits, test plan in the PR: [CONTRIBUTING.md](./CONTRIBUTING.md). Single-author maintained; small focused changes land fastest.
- **Roadmap** — tracked on the [issue tracker](https://github.com/ArnaudGuiovanna/tutor-mcp/issues) (`p0` urgent, `p1` sprint, `p2` when convenient). Deferred statistical refinements: [#48](https://github.com/ArnaudGuiovanna/tutor-mcp/issues/48) PFA fidelity and [#52](https://github.com/ArnaudGuiovanna/tutor-mcp/issues/52) FSRS sub-day intervals. Shipped log in [CHANGELOG.md](./CHANGELOG.md).

## License

[MIT](./LICENSE) — free for personal and commercial use, copyright + license text preserved.

## Author

**Arnaud Guiovanna** — [aguiovanna.fr](https://www.aguiovanna.fr) · [@ArnaudGuiovanna](https://github.com/ArnaudGuiovanna)
