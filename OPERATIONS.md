# Operations runbook — tutor-mcp

Practical recipes for running the server. Companion to the README, which describes *what* the project does. This file describes *how* to keep it alive.

## Database backend

The supported local profile stores relational state in a single SQLite file at
`${DB_PATH:-./data/runtime.db}` and runs one server process. With the `local`
narrative backend, Markdown remains below
`${TUTOR_MCP_MEMORY_ROOT:-~/.tutor-mcp}` and both locations must be backed up.
The PostgreSQL `database` backend instead stores encrypted/versioned narrative
objects in PostgreSQL; retain the keyring with database backups. See
[`docs/narrative-memory-operations.md`](./docs/narrative-memory-operations.md).

PostgreSQL with `DB_DRIVER=postgres` is the supported horizontal SaaS backend;
production uses a `DATABASE_URL` with verified TLS as described below. SQLite
remains the simpler supported local profile. The original consolidated schema
is frozen behind its checksum; ordered, immutable incremental migrations
upgrade existing databases under the same advisory lock. Edit neither an
applied migration nor `schema_pg.sql`: checksum drift intentionally stops
startup for operator intervention. Tune the pool with `DB_MAX_CONNS` (default
10) and follow the [SaaS runtime runbook](./docs/saas-runtime-operations.md).

## Production security profile

Set `DEPLOYMENT_PROFILE=production` to turn deployment assumptions into boot
checks. Startup then requires an HTTPS `BASE_URL`, PostgreSQL, shared database
rate limits, a `DATABASE_URL` with `sslmode=verify-full` and explicit
`sslrootcert`, valid non-catch-all `TRUSTED_PROXY_CIDRS`, STARTTLS SMTP, and the
integration-secret keyring. A missing or downgraded control stops the process;
it is never reduced to a warning in this profile.

Account verification and recovery require `SMTP_ADDR` and `SMTP_FROM`.
`SMTP_SERVER_NAME` defaults to the host in `SMTP_ADDR`; optional credentials
are `SMTP_USERNAME` and `SMTP_PASSWORD`. The client refuses SMTP servers that
do not advertise STARTTLS and uses TLS 1.2 or newer. Verification/reset links
are bearer credentials: never paste them into tickets or logs.

Discord webhook URLs contain credentials. Configure a versioned keyring such
as `INTEGRATION_SECRET_KEYS=v1:<base64-key>` and
`INTEGRATION_SECRET_CURRENT_KEY_ID=v1`; source the values from the deployment
secret manager, not a checked-in environment file. The database stores only
AES-256-GCM envelopes bound to the learner row. At startup, plaintext legacy
rows and envelopes using a retained old key are re-encrypted with the current
key before traffic is accepted. General learner-profile reads and scheduler
audience pages never select or decrypt this credential; the scheduler resolves
it only inside the final durable-delivery boundary immediately before HTTP.

### Key rotation and rollback

1. Add the new key while retaining the old one, and select the new key ID.
2. Roll out the binary/configuration; readiness begins only after re-encryption.
3. Verify no row uses the old prefix (`webhook_url LIKE 'enc:v1:old-id:%'`).
4. Remove the old key only after every instance uses the new configuration and
   backups containing old envelopes remain covered by the documented key
   retention policy.

To roll back, restore both key IDs, select the former ID, restart and let the
same rotation pass re-encrypt the rows. Never restore an older database backup
without also restoring the keyring versions needed by that backup. The
database restore procedure below remains the data rollback path.

### Horizontal SaaS profile

For multi-node production, every instance must use:

- `JWT_ED25519_KEYS` identical on every issuer instance, with exactly one active
  Ed25519 private key and the previous public key retained through the maximum
  access-token TTL. Verification keys are published at `/.well-known/jwks.json`.
- `DB_DRIVER=postgres` and the same `DATABASE_URL`.
- `SCHEDULER_MODE=distributed` — each scheduled run slot has at most one lease winner across the fleet.
- `RATELIMIT_BACKEND=postgres` — rate-limit and login-failure counters live in the shared database.
- `TUTOR_MCP_MEMORY_BACKEND=database` (or
  `TUTOR_MCP_MEMORY_ENABLED=off`) — narrative state must be shared.

Startup rejects incomplete or unknown distributed settings. Scheduler runs
use durable states, fenced leases, heartbeat, bounded retry
and DLQ. Direct Discord delivery remains explicitly at-least-once: ambiguous
transport outcomes are quarantined as `delivery_unknown` for operator
reconciliation because Discord cannot deduplicate the internal `event_id`.
Do not describe these instances as stateless while narrative memory or another
node-local feature is enabled.

## Database backup

The SQLite profile keeps relational state in `${DB_PATH:-./data/runtime.db}`;
the local narrative backend remains a separate tree. Loss of either loses part
of learner state. With PostgreSQL/database narrative memory, use a coordinated
`pg_dump`/PITR posture and retain the encryption key versions required by the
backup instead of the SQLite recipes below.

For SaaS, use the encrypted streaming backup, full restore, real WAL replay and
logical tenant procedures in
[`tenant-restore-runbook.md`](./docs/tenant-restore-runbook.md). The scripts are
`deploy/postgres-backup.sh`, `deploy/postgres-full-restore-exercise.sh`,
`deploy/pitr-restore-exercise.sh`, `deploy/tenant-logical-backup.sh` and
`deploy/tenant-logical-restore-exercise.sh`.

### What ships in the repo

- `scripts/backup.sh` — online backup using `sqlite3 .backup`. Safe to run while the server is writing (the SQLite engine acquires the appropriate locks). Writes a date-stamped file then prunes anything older than `BACKUP_RETENTION_DAYS` (default 14).

### Recommended setup — user-level systemd timer

The unit files in this repo ship as inline examples. On a single-user VPS, install them under `~/.config/systemd/user/` so they run without root.

`~/.config/systemd/user/tutor-mcp-backup.service`:

```ini
[Unit]
Description=tutor-mcp — daily SQLite online backup
After=tutor-mcp.service

[Service]
Type=oneshot
ExecStart=/home/ubuntu/mcp/scripts/backup.sh
Environment=DB_PATH=/home/ubuntu/mcp/data/runtime.db
Environment=BACKUP_DIR=/home/ubuntu/backups/tutor-mcp
Environment=BACKUP_RETENTION_DAYS=14
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
```

`~/.config/systemd/user/tutor-mcp-backup.timer`:

```ini
[Unit]
Description=tutor-mcp — schedule daily SQLite backup at 03:30 UTC

[Timer]
OnCalendar=*-*-* 03:30:00 UTC
Persistent=true
RandomizedDelaySec=300

[Install]
WantedBy=timers.target
```

Enable:

```bash
systemctl --user daemon-reload
systemctl --user enable --now tutor-mcp-backup.timer
```

Verify the next run:

```bash
systemctl --user list-timers tutor-mcp-backup.timer
```

Force a backup now (useful before risky operations):

```bash
systemctl --user start tutor-mcp-backup.service
journalctl --user -u tutor-mcp-backup.service --since "1 minute ago"
```

### Off-host copy

The setup above keeps backups on the same VPS. A disk failure loses both the live DB and the backups. Pick at least one of:

- **Tailscale + rsync** to a second machine you control:
  ```bash
  rsync -a --delete /home/ubuntu/backups/tutor-mcp/ user@other-host:/var/backups/tutor-mcp/
  ```
  Add this as a second `ExecStartPost` line in the backup service, or as a separate timer that runs 5 min after the local backup.

- **Object storage** (S3-compatible). Add `aws s3 sync` or `rclone sync` after the local write. Document the bucket and IAM scope in your private notes.

- **A nightly tarball pulled by another machine over SSH**. Lowest friction if you have a homelab.

Whichever you pick, verify the off-host copy *every quarter* by performing a test restore from it (see below).

### Restore procedure

Given a backup file `runtime-2026-05-05T03-30-00Z.db`:

1. Stop the service:
   ```bash
   systemctl --user stop tutor-mcp
   ```
2. Move the current DB aside (don't delete — you may want it for forensic comparison):
   ```bash
   mv /home/ubuntu/mcp/data/runtime.db /home/ubuntu/mcp/data/runtime.db.broken-$(date -u +%FT%TZ)
   rm -f /home/ubuntu/mcp/data/runtime.db-shm /home/ubuntu/mcp/data/runtime.db-wal
   ```
3. Copy the backup into place:
   ```bash
   cp /home/ubuntu/backups/tutor-mcp/runtime-2026-05-05T03-30-00Z.db /home/ubuntu/mcp/data/runtime.db
   ```
4. Restart:
   ```bash
   systemctl --user start tutor-mcp
   journalctl --user -u tutor-mcp --since "30 seconds ago"
   ```

Expect the migration runner to log *"database ready"* with no migration applied (the backup is post-migration).

### Pre-migration safety

The migration runner in `db/migrations.go` is forward-only and does not snapshot before applying. Until that gets a built-in pre-migration backup, follow this manual recipe before deploying a binary that ships new migrations:

```bash
systemctl --user start tutor-mcp-backup.service           # snapshot now
systemctl --user stop tutor-mcp                           # stop server
go build -o /home/ubuntu/mcp/tutor-mcp                    # build new binary
systemctl --user start tutor-mcp                          # restart, migrations run on boot
```

If the migration corrupts something, the on-demand backup taken in step 1 is your rollback target via the restore procedure above.

### Verification — what to check periodically

- **Size sanity**: a daily backup that suddenly drops below 50% of the previous size suggests data loss. Add a log-watch alert if you care.
- **Open the backup**: every quarter, run `sqlite3 <backup> 'SELECT COUNT(*) FROM interactions;'` against the latest off-host copy. Compare to live. Mismatch = the off-host pipeline is broken.
- **Practice restore**: every six months, run the restore procedure into a scratch directory (`DB_PATH=/tmp/test-restore.db`) and boot a second instance on a different port. If it doesn't come up clean, your backups are theatre.

## Data retention maintenance

Retention is **disabled by default**. The server never deletes learning history
merely because it starts, and zero days always means “preserve”, not “delete
immediately”. The separate `tutor-retention` maintenance command is read-only
unless `--apply` is supplied.

Build it alongside the server:

```bash
go build -o tutor-retention ./cmd/tutor-retention
```

Preview a policy against the existing SQLite database:

```bash
DB_PATH=/home/ubuntu/mcp/data/runtime.db ./tutor-retention \
  --webhook-days=30 \
  --webhook-live-days=14 \
  --assessment-plaintext-days=90 \
  --assessment-abandoned-days=30 \
  --idempotency-response-days=30 \
  --snapshot-days=180 \
  --event-days=90 \
  --consolidation-days=180 \
  --memory-days=180
```

The JSON report separates policy-matching `eligible` objects, actual `applied`
mutations, and objects preserved by an active legal hold in `held`. In a
dry-run every `applied` value is zero. Preserve the exact command and report,
review them, then take a fresh backup. A new apply job is rejected unless its
operator-supplied backup timestamp is at most 24 hours older than the immutable
job cutoff.

```bash
systemctl --user start tutor-mcp-backup.service

# Replace these examples with the identifier, reference and completion time
# recorded by the backup system. The job ID must be unique to this policy run.
RETENTION_JOB_ID=retention-20260811-1200
RETENTION_BACKUP_REFERENCE=/home/ubuntu/backups/tutor-mcp/runtime-2026-08-11T11-58-00Z.db
RETENTION_BACKUP_CREATED_AT=2026-08-11T11:58:00Z

DB_PATH=/home/ubuntu/mcp/data/runtime.db ./tutor-retention \
  --webhook-days=30 \
  --webhook-live-days=14 \
  --assessment-plaintext-days=90 \
  --assessment-abandoned-days=30 \
  --idempotency-response-days=30 \
  --snapshot-days=180 \
  --event-days=90 \
  --consolidation-days=180 \
  --memory-days=180 \
  --job-id="$RETENTION_JOB_ID" \
  --actor=operator@example.com \
  --backup-reference="$RETENTION_BACKUP_REFERENCE" \
  --backup-created-at="$RETENTION_BACKUP_CREATED_AT" \
  --apply
```

Apply mode persists the policy, cutoff, backup proof and phase reports under
`job-id`. The relational phase and its checkpoint commit in one transaction;
the narrative phase follows and is idempotent. A category failure rolls the
relational transaction back. A process crash, expired 30-minute lease, or
narrative failure leaves a durable partial status. Inspect it with:

```bash
DB_PATH=/home/ubuntu/mcp/data/runtime.db ./tutor-retention \
  --action=job-status --job-id="$RETENTION_JOB_ID"
```

Resume by repeating the apply command with the same job ID and exact policy,
backup reference and backup timestamp. Completed phases are not replayed. A
different manifest is rejected; create a new job ID only for a deliberately
new policy run. Keep the final JSON report with the change record: its category
and phase counters prove which objects were eligible, mutated or held, while
`last_error` preserves a partial failure. Do not run two operators against the
same job; the durable lease admits only one owner.

### Retention legal holds

Create a hold before the dry-run. Omit `--apply` to preview the record, then
repeat with `--apply` after review:

```bash
DB_PATH=/home/ubuntu/mcp/data/runtime.db ./tutor-retention \
  --action=hold-create \
  --hold-id=case-2026-0042 \
  --learner=L123 \
  --reason='litigation preservation request 2026-0042' \
  --actor=legal@example.com \
  --apply

DB_PATH=/home/ubuntu/mcp/data/runtime.db ./tutor-retention \
  --action=hold-list --limit=100
```

An active hold preserves that learner's objects in every supported relational
category and in either the local or database narrative backend; they appear in
`held`, never `eligible` or `applied`. Releasing a hold is a separate audited,
one-way action. Preview first by omitting `--apply`:

```bash
DB_PATH=/home/ubuntu/mcp/data/runtime.db ./tutor-retention \
  --action=hold-release \
  --hold-id=case-2026-0042 \
  --reason='written release received 2026-09-01' \
  --actor=legal@example.com \
  --apply
```

Never reuse a released hold ID. Create a new hold record if preservation is
required again. Retention after release requires a new dry-run, fresh backup
and new job.

The command refuses to create a missing SQLite file and does not run schema
migrations. Point it only at a database already migrated by the matching server
binary. For PostgreSQL, set `DB_DRIVER=postgres` and `DATABASE_URL`; do not put
the DSN on the command line where process listings can expose it. Set
`TUTOR_MCP_MEMORY_BACKEND=database` (the PostgreSQL default), or pass the same
`--memory-backend=local|database` value used by the server, so the narrative
phase targets the correct storage system.

SQLite dry-run opens the main database with `mode=ro` and `query_only=ON`; it
does not run a write transaction or change journal mode. SQLite may still
create or update `-wal`/`-shm` coordination sidecars when inspecting a live WAL
database. Those files are part of SQLite locking/visibility, not retention
mutations. The main database file remains read-only. Stop the service first if
your filesystem policy forbids even coordination sidecars.

| Setting / flag | Default | Lifecycle action |
|---|---:|---|
| `TUTOR_MCP_RETENTION_WEBHOOK_DAYS` / `--webhook-days` | `0` | Delete only terminal (`sent`, `failed`, `expired`) queue rows. `pending` and `processing` are never selected. Retained push logs have their logical `queue_id` cleared first. |
| `TUTOR_MCP_RETENTION_WEBHOOK_LIVE_DAYS` / `--webhook-live-days` | `0` | Terminalize stale `pending`/`processing` rows as `expired`, including work made undispatchable by consent or configuration changes. |
| `TUTOR_MCP_RETENTION_ASSESSMENT_PLAINTEXT_DAYS` / `--assessment-plaintext-days` | `0` | Set task/response plaintext to `NULL` only on evaluated or cancelled attempts and only when the corresponding content hash exists. The evidence envelope, rubric, score, provenance and hashes remain. |
| `TUTOR_MCP_RETENTION_ASSESSMENT_ABANDONED_DAYS` / `--assessment-abandoned-days` | `0` | Cancel stale prepared/submitted attempts and immediately clear task/response plaintext wherever a content hash exists. |
| `TUTOR_MCP_RETENTION_IDEMPOTENCY_RESPONSE_DAYS` / `--idempotency-response-days` | `0` | Clear only the cached `response_text` payload of old completed mutations and set `response_expired_at`. The learner/tool/key tuple, request hash, `completed` status and timestamps remain, so the mutation can never be claimed again. |
| `TUTOR_MCP_RETENTION_SNAPSHOT_DAYS` / `--snapshot-days` | `0` | Delete old pedagogical decision snapshots; parent interactions remain. |
| `TUTOR_MCP_RETENTION_EVENT_DAYS` / `--event-days` | `0` | Delete old notification event logs (`webhook_push_log`, `scheduled_alerts`). A future-dated push or scheduled alert is preserved even if its row was created before the cutoff. |
| `TUTOR_MCP_RETENTION_CONSOLIDATION_DAYS` / `--consolidation-days` | `0` | Delete completed consolidation scheduling markers; narrative archive files are governed separately. |
| `TUTOR_MCP_RETENTION_MEMORY_DAYS` / `--memory-days` | `0` | Delete regular Markdown files older than the cutoff below the configured narrative-memory root; symlinks are never followed. |

`assessment_plaintext_blocked` is deliberately conservative: it counts old
rows whose plaintext is their only representation. Those fields are retained
even during `--apply`; backfill a verified hash or choose explicit legal/manual
handling rather than silently destroying the evidence.

`idempotency_response_plaintext` reports cached response payloads selected and
cleared. After expiry, an exact retry returns “mutation already completed;
cached response expired” and does not call the mutation handler. Never delete
the corresponding idempotency row to reclaim space: doing so would release the
key and could duplicate a learning-state write after an ambiguous client retry.

Core learning evidence — interactions, affect, calibration, transfer,
implementation intentions and concept state — is outside this generic policy.
Deleting it changes the learner model and requires a separate product/legal
decision. Expired OAuth codes and refresh-token families continue to use their
existing security-specific cleanup and are intentionally not duplicated here.
Run maintenance during a low-write window if the dry-run counts must match the
subsequent apply exactly. Align backup-object retention, off-host copies and
encryption-key destruction with the same privacy policy: deleting live rows
does not erase older backup copies. Restoring a backup taken before a retention
job can reintroduce deleted data. Record completed job reports outside the live
database, reconcile holds after restore, run a new dry-run, take a new backup,
and apply a new retention job before returning the restored service to normal
traffic.

## Webhook retry and dead-letter lifecycle

Webhook delivery state is durable in `webhook_message_queue`; a process restart
does not reset its retry budget. A claim increments `attempt_count`. A transport
failure returns the row to `pending` with `next_attempt_at` set using the fixed
backoff sequence 1 minute, 5 minutes, 30 minutes, 2 hours, then 12 hours. The
last delay is reused if a caller explicitly configures more than the default
five attempts. A policy/consent denial releases the claim and restores the
counter because no delivery was attempted.

Once `max_attempts` is reached, the existing terminal `failed` state is the
dead-letter queue; `dead_lettered_at` and a bounded, sanitized `last_error` code
are retained for diagnosis. Expiration wins over retry/DLQ: an expired message
becomes `expired` and is never dispatched again. The hourly cleanup also
recovers `processing` claims older than 15 minutes, applying the same
expiry/backoff/DLQ rules. Do not persist webhook URLs, credentials, or raw
response bodies in `last_error`.

For a metadata-only DLQ check (intentionally omit the sensitive `content`
column):

```sql
SELECT id, learner_id, kind, attempt_count, max_attempts,
       last_error, dead_lettered_at
FROM webhook_message_queue
WHERE status = 'failed'
ORDER BY dead_lettered_at DESC;
```

Retries are picked up when the dispatcher for that message kind runs and the
stored `next_attempt_at` is due. Row-level claims prevent two workers from
dispatching the same attempt concurrently, but delivery remains at-least-once:
a crash after the remote service accepts a request and before `sent` is stored
can cause a later retry. Terminal queue retention also applies to DLQ rows and
uses `dead_lettered_at`, so investigate/export required metadata before the
configured `--webhook-days` cutoff.

## Pipeline observability

The regulation pipeline emits structured `level=INFO` log lines so each `get_next_activity` call leaves a trace. The four key event types:

| Event | Source | Fields |
|-------|--------|--------|
| `pipeline decision` | `tools/activity.go` | `route` (`orchestrator` \| `review_override`), `phase`, `activity_type`, `concept`, `rationale`, `learner`, `domain` |
| `phase transition (FSM)` | `engine/orchestrator.go` | `from`, `to`, `entry_entropy`, `rationale`, `domain` |
| `phase fallback (NoFringe)` | `engine/orchestrator.go` | `from`, `to`, `retry`, `domain` — FSM-disjoint phase override when no candidate is eligible |
| `goal_relevance updated` | `tools/goal_relevance.go` | `concepts_updated`, `covered_total`, `all_concepts`, `uncovered`, `version`, `stale_after_set` |
| `interaction recorded` | `tools/interaction.go` | `concept`, `activity_type`, `success`, `hints_requested`, `self_initiated`, `new_mastery`, `new_theta`, `reps` |

### Live tail — full pipeline narrative

```bash
journalctl --user -u tutor-mcp -f \
  | grep -E "pipeline decision|phase transition|phase fallback|goal_relevance updated|interaction recorded|gate:"
```

### Single-session forensic — by `session_id`

```bash
journalctl --user -u tutor-mcp --since "1 hour ago" \
  | grep -E "pipeline decision|interaction recorded" \
  | grep "session=sess_<your_session_id>"
```

Both pipeline decisions and recorded interactions include the canonical durable
`session_id`, so the filter above follows one episode even when it lasts longer
than the former sliding activity window.

### Aggregations (last hour)

```bash
# Count decisions by route
journalctl --user -u tutor-mcp --since "1 hour ago" \
  | grep "pipeline decision" \
  | grep -oE 'route=[a-z_]+' | sort | uniq -c

# Count phase transitions
journalctl --user -u tutor-mcp --since "1 hour ago" \
  | grep "phase transition (FSM)" | wc -l

# Activity-type distribution
journalctl --user -u tutor-mcp --since "1 hour ago" \
  | grep "pipeline decision" \
  | grep -oE 'activity_type=[A-Z_]+' | sort | uniq -c
```

### Health signals to watch

- **`route=review_override` spike** — learners are explicitly asking for review. If unexpected, inspect the surrounding chat/tool calls that set `intent=review`.
- **`orchestrator failed` errors** — the regulation pipeline could not compute an activity. Look for the same `learner` and `domain` on the preceding `level=ERROR` line.
- **No `pipeline decision` logs after a session starts** — the LLM isn't calling `get_next_activity`. Drift in the system prompt likely.
- **No `interaction recorded` logs while exercises are happening** — the LLM is generating activities but not closing the loop with `record_interaction`. Cohérence-of-rule-3 problem in the system prompt.
- **Repeated `phase fallback (NoFringe)` for the same domain** — the candidate pool is empty. Likely cause: missing `goal_relevance` on a domain where the strict contract is enforced (partial vector). Run `set_goal_relevance` to repair.

## Operational profiles

The local profile is a **single process on a single host**:

- With `RATELIMIT_BACKEND=memory`, token buckets and login-failure counters reset on restart.
- With `SCHEDULER_MODE=inprocess`, a second process would run the same scheduled jobs.
- With `TUTOR_MCP_MEMORY_BACKEND=local`, Markdown files are local to one host and concurrent read-modify-write operations are not coordinated across processes.

The supported SaaS profile uses PostgreSQL, shared rate limits, recoverable
fenced job leases, durable outbox/webhook state and the encrypted/versioned
`database` narrative backend. Direct Discord delivery cannot be exactly-once:
an ambiguous outcome remains quarantined for operator reconciliation. Tenant SaaS
webhooks use their separate signed, deduplicable contract. See the
[runtime](./docs/saas-runtime-operations.md), [SLO](./docs/saas-slo.md) and
[restore](./docs/tenant-restore-runbook.md) runbooks.

For the single-node profile, avoid restart storms while a login lockout is actively protecting an account and include `${TUTOR_MCP_MEMORY_ROOT:-~/.tutor-mcp}` in the off-host backup plan.

## Service control quick reference

```bash
# State
systemctl --user status tutor-mcp
systemctl --user list-timers

# Logs
journalctl --user -u tutor-mcp -f                 # live tail
journalctl --user -u tutor-mcp --since "1 hour ago"
journalctl --user -u tutor-mcp-backup --since "today"

# Restart after binary change
go build -o tutor-mcp .
systemctl --user restart tutor-mcp
```
