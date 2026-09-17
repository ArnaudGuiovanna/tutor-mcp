# Configuration reference

[Back to README](../README.md#documentation) · [Installation](installation.md) · [Profiles](profiles.md)

## Start with your profile

| Profile | What you configure | Reference |
|---|---|---|
| Local stdio | `--local`, optional `--data-dir`; identity, SQLite and memory keys are created automatically. | [Local mode](profiles.md#local-mode) |
| Hobby VPS | `init --profile hobby --public-url https://your.domain`, service data directory and HTTPS proxy. Signing and memory keys are generated locally; accounts use SSH invitations. | [Hobby installation](installation.md#native-hobby-vps-binary-systemd-and-caddy) |
| Institution | Separate API/worker/migrator environments, PostgreSQL, SMTP and operator-managed keys. | [Institutional deployment](installation.md#institutional-deployment) |

The variables below describe the HTTP runtime and optional features. They are
not a prerequisite checklist for local stdio. `--local` starts before HTTP/OAuth
configuration is loaded. `--profile hobby` sets its own database, signing keys,
memory backend and single-server scheduler. `--profile institution` selects
PostgreSQL and applies the production validation gates. Starting without a profile
retains the legacy environment-based behavior.

For hobby and institution, `LISTEN_ADDR` overrides the default loopback HTTP
bind address (for example `0.0.0.0:3000` inside a private container network).
Expose the service through an HTTPS proxy; see [VPS prerequisites](installation.md#vps-prerequisites).

## Environment variables

Environment variables read at boot:

| Variable | Default | Effect |
|---|---|---|
| `JWT_ED25519_KEYS` | — *(required in production)* | JSON keyring with one active Ed25519 private key and current/previous public keys (`kid`, base64 standard encoding). Enables overlap rotation and JWKS publication. |
| `JWT_SECRET` | — *(development fallback)* | Legacy HS256 compatibility secret. Base64, 32+ decoded bytes. Rejected as the sole production signing configuration. |
| `DEPLOYMENT_PROFILE` | `development` | `production` fails closed unless the public origin is HTTPS, PostgreSQL uses verified TLS with an explicit CA, shared rate limits, SMTP, integration-secret encryption and trusted proxy CIDRs are configured. |
| `PROCESS_ROLE` | `all` in development | `api`, `worker`, or `migrator` is mandatory in production. Only the migrator applies DDL. |
| `PORT` | `3000` | HTTP listen port |
| `DB_DRIVER` | `sqlite` | `sqlite` for legacy development or `postgres` for institutional deployments; explicit profiles choose the driver. |
| `DB_PATH` | `./data/runtime.db` | SQLite path (ignored when `DB_DRIVER=postgres`) |
| `DATABASE_URL` | — | Postgres DSN, **required** when `DB_DRIVER=postgres`. Production requires `sslmode=verify-full` and an explicit `sslrootcert` CA. |
| `DB_MAX_CONNS` | `10` | Postgres connection-pool size per instance (ignored on SQLite). Keep `DB_MAX_CONNS × instances < Postgres max_connections`. |
| `SCHEDULER_MODE` | `inprocess` | `inprocess` (recommended) or `distributed` (fenced recoverable leases, heartbeat, retry and DLQ). For HTTP deployments, distributed mode requires PostgreSQL, PostgreSQL rate limits, and shared database narrative memory or disabled memory. Local stdio uses the durable lease implementation directly on SQLite. Unknown values fail at boot. |
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
| `OAUTH_GRANULAR_SCOPES` | **`off`** *(opt-in)* | `on` publishes and issues per-tool `learner:read` / `learner:write` grants; `off` keeps the bounded legacy `learner` compatibility mode. Only `on` and `off` are accepted. Use the [two-phase rollout and rollback runbook](oauth-granular-scopes-rollout.md). |
| `OAUTH_DCR_MODE` | `open` in development | `open`, `token`, or `disabled`. Production requires an explicit `token` or `disabled`; `disabled` removes `/register` from discovery and routing. See the [DCR production runbook](oauth-dcr-production.md). |
| `OAUTH_DCR_INITIAL_ACCESS_TOKEN` | — | Required only with `OAUTH_DCR_MODE=token`. Must be unpadded base64url encoding at least 32 bytes (for example, 64 hex characters from `openssl rand -hex 32`). Startup registers only its SHA-256 digest in the shared capability registry; changed values overlap until explicit audited revocation with `tutor-dcr-admin`. |
| `SMTP_ADDR` / `SMTP_FROM` | — | SMTP endpoint and sender for verification/recovery. Delivery requires STARTTLS (TLS 1.2+); public account flows cannot complete when absent. Optional `SMTP_SERVER_NAME`, `SMTP_USERNAME`, `SMTP_PASSWORD`. |
| `INTEGRATION_SECRET_KEYS` | — | Comma-separated `key_id:base64-32-byte-key` keyring used to encrypt webhook credentials and shared narrative memory at rest. Supply old and new keys during rotation. |
| `INTEGRATION_SECRET_CURRENT_KEY_ID` | — | Key ID used for new envelopes; startup atomically re-encrypts legacy/old-key records. Required with `INTEGRATION_SECRET_KEYS`. |
| `TENANT_INTEGRATION_ALLOWED_HOSTS` | — | Comma-separated HTTPS host allowlist for signed tenant webhooks; mandatory in production. IP literals and non-443 ports are refused. |
| `TUTOR_MCP_MEMORY_ENABLED` | `on` | Enables narrative learner memory. Runtime concept notes and sessions are domain-scoped; ambiguous legacy/global narratives are excluded from activity generation. |
| `TUTOR_MCP_MEMORY_BACKEND` | `local` on SQLite, `database` on PostgreSQL | `local` Markdown or encrypted/versioned relational objects. Active distributed and production profiles require `database`. See the [migration and rotation runbook](narrative-memory-operations.md). |
| `TUTOR_MCP_MEMORY_ROOT` | `~/.tutor-mcp/` | Local backend root and create-only backfill source when switching to `database`. |
| `TUTOR_MCP_MEMORY_MAX_WRITE_BYTES` | `262144` | Maximum content supplied to one narrative-memory write. |
| `TUTOR_MCP_MEMORY_MAX_FILE_BYTES` | `1048576` | Maximum size of one narrative Markdown file, on reads and writes. |
| `TUTOR_MCP_MEMORY_MAX_LEARNER_BYTES` | `16777216` | Cumulative narrative-memory quota per learner. |
| `TUTOR_MCP_MEMORY_MAX_FILES_PER_LEARNER` | `2048` | Maximum Markdown files per learner. |
| `TUTOR_MCP_MEMORY_MAX_CONCURRENT_WRITES` | `32` | Maximum concurrent narrative writes per process; overflow receives backpressure. |
| `REGULATION_THRESHOLD` | `on` | `off` reverts to legacy split thresholds (BKT 0.85 / KST 0.70 / Mid 0.80) |
| `REGULATION_GOAL` | `on` | `off` hides `set_goal_relevance` / `get_goal_relevance` and drops the goal-aware prompt section |
| `REGULATION_ACTION` / `_CONCEPT` / `_GATE` | `on` | `off` drops the system-prompt appendix only — the selector / gate logic always runs |
| `REGULATION_FADE` | **`off`** *(compatibility flag)* | Strict literal `on` exposes descriptive autonomy metrics and `fade_status`; it does not withdraw help or change scheduling from an unvalidated composite score. |

Credential checks, registration, verification and recovery have distinct
rate-limit buckets. The MCP endpoint applies both per-IP/per-learner rates and
the in-flight concurrency ceilings configured above.
OAuth access tokens expire after 30 minutes. Refresh tokens rotate as a family;
replay of an already-used member revokes the family instead of issuing another
access token. Granular OAuth scopes are an opt-in fleet-wide change; follow the
[scope rollout runbook](oauth-granular-scopes-rollout.md) before enabling
them.

Data lifecycle cleanup is opt-in and runs through a separate dry-run-first
maintenance command; it is never triggered by server startup. See
[Data retention maintenance](../OPERATIONS.md#data-retention-maintenance) for
safe defaults, durable apply jobs, legal holds, backup proof, crash recovery
and restoration reconciliation.
Ambiguous outbound webhook attempts are quarantined and require the separate,
explicit `tutor-webhook-admin` command; they are never retried automatically.
See the [webhook delivery runbook](webhook-delivery-operations.md).
The product-level evidence, curriculum, session and learner-control invariants
are specified in the [learning integrity contract](learning-integrity.md).
