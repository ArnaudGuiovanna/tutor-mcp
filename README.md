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

**Status — alpha v0.4.1.** The robust MVP target is a single-tenant, single-node deployment using SQLite and the in-process scheduler. The regulation pipeline (phase FSM + concept/action selectors + gate + threshold resolver) ships default-on; the fade controller is opt-in. PostgreSQL, distributed leases, and shared rate limits are available for testing, but multi-node operation remains **experimental**: narrative memory is local and job leases do not provide crash-safe exactly-once delivery.

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
| **Algorithmic state** | SQLite (recommended) or PostgreSQL (experimental) domains, concept states, interactions, affect, calibration, transfer, intentions | Domains, prerequisites, phase, mastery, retention, ability, review timing, transfer readiness, active misconceptions |
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

- **Learning loop** — Before and after every exchange, the LLM calls `get_next_activity` and `record_interaction`. The runtime updates BKT mastery, FSRS recall, IRT ability, Rasch/Elo exercise calibration, transfer evidence and misconception status — in real time, on every interaction. The LLM never picks scheduling itself.
- **Narrative memory loop** — `record_session_close` asks the LLM for a factual session trace; `update_learner_memory` stores stable memory, pending observations, concept notes, sessions and archives. The next `get_next_activity` call can use those traces to avoid a generic exercise.
- **Metacognitive loop** — Affect check-ins (`record_affect`), calibration tracking (`calibration_check` / `record_calibration_result`) and an autonomy score observe the learner's relationship to the system. A factual mirror surfaces consolidated dependency patterns — the system aims to make itself progressively unnecessary.
- **Motivation loop** — A brief engine selects one motivational angle per exercise (milestone, competence value, growth mindset, affect reframe, plateau recontext, utility value) and emits *signals + instruction* — never canned text. The LLM phrases it.

The pillars of an Intelligent Tutoring System map cleanly:

| ITS pillar | Owner |
|---|---|
| **Domain model** (concept graph, prerequisites) | Tutor MCP runtime — KST-validated |
| **Learner model** (mastery, ability, recall, transfer) | Tutor MCP runtime — BKT, IRT, Rasch/Elo, PFA |
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
export JWT_SECRET="$(openssl rand -base64 32)"   # required — must be base64
export BASE_URL=https://your.domain              # public origin, no trailing slash
./tutor-mcp                                       # listens on :3000 by default
```

Verify: `curl $BASE_URL/health` → `{"status":"ok"}`.

For real use, put the runtime behind a public reverse proxy with TLS — see [OPERATIONS.md](./OPERATIONS.md). Web clients (Claude.ai, ChatGPT, Le Chat) require a public HTTPS endpoint; `http://localhost` is rejected by their cloud connectors.

### 3. Connect a client

Add `https://your.domain/mcp` as a custom MCP connector. OAuth 2.1 + PKCE with dynamic client registration: no client ID or secret to copy by hand. On the first connection the client opens `/authorize` — register (email + password) or log in. Subsequent launches reuse refresh tokens silently.

<a id="setup"></a>

| Client | Path | Notes |
|---|---|---|
| **Claude.ai** | Settings → Connectors → + → URL `https://your.domain/mcp` | Pro, Max, Team, Enterprise |
| **ChatGPT** | Settings → Connectors → Advanced → Developer Mode → Create | Plus, Pro, Team, Enterprise, Edu |
| **Le Chat** | Connectors → + Add Connector → Custom MCP | Auto-detects OAuth |
| **Gemini Enterprise** | GCP Console → Custom MCP server data store | StreamableHTTP transport |
| **Gemini CLI** | [`geminicli.com/docs/tools/mcp-server/`](https://geminicli.com/docs/tools/mcp-server/) | Local CLI |
| **Claude Code** (CLI, local) | `.mcp.json` with `"url": "http://localhost:3000/mcp"` | No HTTPS needed |

## MCP tools (35)

Domain-scoped learning tools accept an optional `domain_id`; where documented, omitting it selects the most recently active non-archived domain. Learner-global and lifecycle tools intentionally have different contracts—use each tool's schema as the source of truth.

### Core learning loop (7)

| Tool | Purpose |
|---|---|
| `get_learner_context` | Session-start context: active domain, concept states, recent history, active misconceptions |
| `get_pending_alerts` | Learning + metacognitive alerts requiring action |
| `get_next_activity` | Next optimal activity + episodic context + reasoning request + tutor mode + motivation brief + mastery uncertainty + transfer profile + Rasch/Elo calibration |
| `record_interaction` | Persist outcome, update BKT/FSRS/IRT/Rasch-Elo; tracks hints, initiative, error type, misconception, rubric evidence, interpretation brief |
| `check_mastery` | Mastery-challenge readiness: BKT + evidence diversity + uncertainty + transfer status |
| `get_olm_snapshot` | Open Learner Model: per-concept mastery, retention, fringe membership |
| `get_dashboard_state` | Full dashboard: progress, retention, autonomy, calibration bias, affect history |

### Domain management (9)

| Tool | Purpose |
|---|---|
| `init_domain` | Create domain with concept graph, prerequisites, personal goal |
| `add_concepts` | Append concepts without resetting progress |
| `validate_domain_graph` | Audit graph: cycles, orphans, depth, disconnections |
| `archive_domain` / `unarchive_domain` / `delete_domain` | Lifecycle |
| `set_domain_priority` | Re-rank domains for scheduling weight |
| `set_goal_relevance` / `get_goal_relevance` | LLM-decomposed relevance vector over the concept graph (biases the concept selector) — gated by `REGULATION_GOAL` |

### Metacognition (5)

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
| `feynman_challenge` | Learner explains a mastered concept; LLM detects gaps for BKT injection |
| `transfer_challenge` / `record_transfer_result` | Structured probe in `near`/`far`/`debugging`/`teaching`/`creative` |
| `learning_negotiation` | Expose system plan + tradeoffs; learner can propose alternatives |

### Memory & session (5)

| Tool | Purpose |
|---|---|
| `update_learner_memory` / `read_raw_session` / `get_memory_state` | Markdown memory: sessions, concepts, stable memory, archives |
| `record_session_close` | Recap brief + optional Gollwitzer if-then implementation intention |
| `queue_webhook_message` | Queue a structured Discord nudge (`why_now`, `learning_gain`, `open_loop`, `next_action`) |

### Availability (1)

| Tool | Purpose |
|---|---|
| `get_availability_model` | Learner's time windows and session frequency |

### Alert engine

The scheduler detects nine alert types — learning (`FORGETTING`, `PLATEAU`, `ZPD_DRIFT`, `OVERLOAD`, `MASTERY_READY`) and metacognitive (`DEPENDENCY_INCREASING`, `CALIBRATION_DIVERGING`, `AFFECT_NEGATIVE`, `TRANSFER_BLOCKED`). Daily dedup, per-day frequency cap; archived/deleted domains are filtered out of reads and webhooks.

## Cognitive science engine

Pure-function algorithms running on every interaction, composed by the regulation orchestrator (`engine/orchestrator.go`; design notes in [`docs/regulation-design/`](./docs/regulation-design/)).

| Algorithm | Role |
|---|---|
| **BKT** + individualized BKT | Estimates mastery confidence per concept, not just whether the learner answered right today; recent-history profile individualizes `P(Learn)`, `P(Slip)`, `P(Guess)` — never tuned by the LLM |
| **FSRS** | Decides when to bring a concept back, using stability and difficulty curves |
| **IRT** | Tracks learner ability θ from response patterns so activity difficulty is calibrated to the current learner |
| **Rasch / Elo** | Keeps a deterministic learner-ability vs exercise-difficulty signal, exposed to the LLM and stored in snapshots |
| **PFA** | Weighs wins and losses on each concept to predict how the next attempt is likely to go |
| **KST** | Validates prerequisite graph; gates new concepts on mastery of ancestors |
| **Structured transfer** | Checks whether knowledge moves beyond the training pattern across `near`/`far`/`debugging`/`teaching`/`creative` probes |

The **regulation pipeline** runs as a 7-stage chain inside `get_next_activity`: threshold resolver → goal decomposer → phase FSM (`DIAGNOSTIC ↔ INSTRUCTION ↔ MAINTENANCE`) → concept selector → gate (anti-repeat / session-budget / no-fringe escape) → action selector → fade controller. Pure functions are unit-tested (~90 tests); the orchestrator integration is covered by SQLite in-memory + migration tests. Full design rationale in [`docs/regulation-design/`](./docs/regulation-design/).

## Configuration

Environment variables read at boot:

| Variable | Default | Effect |
|---|---|---|
| `JWT_SECRET` | — *(required)* | HS256 secret. Must be valid base64 (plain strings rejected at boot). Use `openssl rand -base64 32` — 32+ decoded bytes recommended for HS256. |
| `PORT` | `3000` | HTTP listen port |
| `DB_DRIVER` | `sqlite` | `sqlite` (recommended MVP profile, embedded) or `postgres` (experimental). |
| `DB_PATH` | `./data/runtime.db` | SQLite path (ignored when `DB_DRIVER=postgres`) |
| `DATABASE_URL` | — | Postgres DSN, **required** when `DB_DRIVER=postgres` (e.g. `postgres://user:pass@host:5432/db?sslmode=require`). |
| `DB_MAX_CONNS` | `10` | Postgres connection-pool size per instance (ignored on SQLite). Keep `DB_MAX_CONNS × instances < Postgres max_connections`. |
| `SCHEDULER_MODE` | `inprocess` | `inprocess` (recommended) or experimental `distributed` (one lease winner per run slot; not crash-safe exactly-once). Distributed mode requires PostgreSQL, PostgreSQL rate limits, and local narrative memory disabled. Unknown values fail at boot. |
| `RATELIMIT_BACKEND` | `memory` | `memory` (per-instance, default) or `postgres`/`db` (shared rate-limit + login-failure store). The PostgreSQL backend requires `DB_DRIVER=postgres`; unknown values fail at boot. |
| `BASE_URL` | `http://localhost:$PORT` | Public HTTP(S) origin. A trailing `/` is normalized; paths, credentials, query strings, fragments, and non-HTTP schemes fail at boot. Triggers HSTS when `https://`. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `TRUSTED_PROXY_CIDRS` | — | Comma-separated CIDRs of trusted reverse-proxies. **Required behind a public proxy** — without it every IP-rate-limit collapses under the proxy's loopback bucket. |
| `MCP_RATE_LIMIT_PER_MIN` | `60` | Per-IP and per-learner cap on `/mcp` |
| `MCP_RATE_LIMIT_BURST` | `60` | Burst allowance |
| `TUTOR_MCP_MEMORY_ENABLED` | `on` | Node-local Markdown learner memory. Experimental distributed mode requires `off` until a shared memory backend exists. |
| `TUTOR_MCP_MEMORY_ROOT` | `~/.tutor-mcp/` | Memory FS root |
| `REGULATION_THRESHOLD` | `on` | `off` reverts to legacy split thresholds (BKT 0.85 / KST 0.70 / Mid 0.80) |
| `REGULATION_GOAL` | `on` | `off` hides `set_goal_relevance` / `get_goal_relevance` and drops the goal-aware prompt section |
| `REGULATION_ACTION` / `_CONCEPT` / `_GATE` | `on` | `off` drops the system-prompt appendix only — the selector / gate logic always runs |
| `REGULATION_FADE` | **`off`** *(opt-in)* | Strict literal `on` enables the fade controller (verbosity reduction + webhook frequency + ZPD aggressiveness + proactive review). Any other value keeps it off. |

Auth endpoints are rate-limited at 10/min (`/authorize`, `/token`), 5/min (`/register`); the MCP endpoint applies the per-IP and per-learner caps configured above.

## Architecture

```
main.go              HTTP + MCP handler + OAuth + scheduler
auth/                OAuth 2.1 + JWT + PKCE + rate limiter
algorithms/          BKT / FSRS / IRT / Rasch-Elo / PFA / KST + thresholds
engine/              Orchestrator + phase FSM + selectors + gate + fade
                     + alert / motivation / mirror / replay / OLM
models/              Typed structs (learner, domain, interactions, regulation, …)
db/                  Store (SQLite default + Postgres) + dialect-aware schema + checksummed migrations
memory/              Markdown learner memory (stable / pending / sessions / concepts / archives)
tools/               MCP tool handlers + system prompt + rubrics
```

The regulation engine is layered: **pure** decision components (`phase_fsm.go`, `concept_selector.go`, `action_selector.go`, `gate.go`) composed by an **impure** orchestrator (`orchestrator.go`). The same separation applies to the metacognition (autonomy, mirror, tutor mode) and motivation (brief selection) modules.

## Capacity & sizing

The supported MVP profile is intentionally **single-tenant, single-node**: SQLite + in-process scheduler, with no broker or external database required.

| Profile | Active / day | Registered | Use case |
|---|---|---|---|
| **Personal** | 1 | 1–5 | Solo learning |
| **Small group** | 1–10 | up to 30 | Family / team |
| **Classroom** | 10–50 | up to 150 | Facilitated sessions |
| **Small org** | 50–200 | up to 600 | Sustained load |

Treat ~200 active learners as a planning ceiling for the default SQLite profile, not a service-level guarantee; measure against your workload. PostgreSQL can be exercised for conformance and multi-node experiments, but it is not yet the production scaling recommendation. See the explicit limitations and fail-fast configuration in [OPERATIONS.md](./OPERATIONS.md).

**Idle footprint**: ~30 MB RSS, ~15 MB binary, ~10 MB initial DB (+50 KB/active learner/month). Tested on Raspberry Pi 4 and €5/mo VPS for personal use.

## Tech stack

Go 1.25.12+ · [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) · [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) (pure-Go, no CGO, default) · [jackc/pgx](https://github.com/jackc/pgx) (Postgres, pure-Go, experimental) · [robfig/cron](https://github.com/robfig/cron) · [golang-jwt/jwt](https://github.com/golang-jwt/jwt) · bcrypt.

## Pedagogical reliability

The runtime deliberately separates deterministic decisions from LLM coaching freedom: the runtime owns state transitions, thresholds, graph validation, evidence gates, scheduling and audit snapshots; the LLM owns examples, hints, feedback, tone and explanations. `record_interaction` accepts structured `rubric_json` / `rubric_score_json` and persists them on interactions + pedagogical snapshots. `get_decision_replay_summary` surfaces audit quality (missing rubrics, transfer gaps, JSON issues). A static goldset covers known failure modes (false-positive high BKT, missing rubrics, missing transfer, clean replay).

## Acknowledgments

Stands on the shoulders of: Corbett & Anderson (BKT, 1995), Open-Spaced-Repetition (FSRS), Lord & Novick (IRT, 1968), Pavlik et al. (PFA, 2009), Falmagne & Doignon (KST, 2011), Hidi & Renninger (interest phases, 2006), McClelland / McNaughton / O'Reilly (CLS-inspired memory layering, 1995).

## Operations · Security · Contributing · Roadmap

- **Operations** — backup, restore, off-host copy, systemd-user setup: [OPERATIONS.md](./OPERATIONS.md).
- **Security** — private disclosure channels and operator hardening checklist: [SECURITY.md](./SECURITY.md). Do not open public issues for vulnerabilities.
- **Contributing** — fork, branch from `staging`, conventional commits, test plan in the PR: [CONTRIBUTING.md](./CONTRIBUTING.md). Single-author maintained; small focused changes land fastest.
- **Roadmap** — tracked on the [issue tracker](https://github.com/ArnaudGuiovanna/tutor-mcp/issues) (`p0` urgent, `p1` sprint, `p2` when convenient). Deferred statistical refinements: [#48](https://github.com/ArnaudGuiovanna/tutor-mcp/issues/48) PFA fidelity, [#49](https://github.com/ArnaudGuiovanna/tutor-mcp/issues/49) IRT EAP/MAP prior, [#52](https://github.com/ArnaudGuiovanna/tutor-mcp/issues/52) FSRS sub-day intervals. Shipped log in [CHANGELOG.md](./CHANGELOG.md).

## License

[MIT](./LICENSE) — free for personal and commercial use, copyright + license text preserved.

## Author

**Arnaud Guiovanna** — [aguiovanna.fr](https://www.aguiovanna.fr) · [@ArnaudGuiovanna](https://github.com/ArnaudGuiovanna)
