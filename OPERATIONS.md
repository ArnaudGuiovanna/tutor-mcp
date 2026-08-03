# Operations runbook — tutor-mcp

Practical recipes for running the server. Companion to the README, which describes *what* the project does. This file describes *how* to keep it alive.

## Database backend

The supported MVP profile stores the relational state (interactions, OLM, BKT/FSRS/IRT state, refresh tokens, calibration history) in a single SQLite file at `${DB_PATH:-./data/runtime.db}` and runs one server process. Narrative memory is stored separately under `${TUTOR_MCP_MEMORY_ROOT:-~/.tutor-mcp}`. Back up both locations if narrative memory is enabled.

An **experimental PostgreSQL** backend is available with `DB_DRIVER=postgres` and `DATABASE_URL=postgres://user:pass@host:5432/db?sslmode=require`. It is exercised in CI for backend conformance. The original consolidated schema is frozen behind its checksum; ordered, immutable incremental migrations upgrade existing databases under the same advisory lock. Edit neither an applied migration nor `schema_pg.sql`: checksum drift intentionally stops startup for operator intervention. Tune the pool with `DB_MAX_CONNS` (default 10).

### Experimental multi-node profile

Multi-node operation is not part of the robust MVP support target. For controlled testing, every instance must use:

- `JWT_SECRET` **identical** on every instance (tokens are verified anywhere).
- `DB_DRIVER=postgres` and the same `DATABASE_URL`.
- `SCHEDULER_MODE=distributed` — each scheduled run slot has at most one lease winner across the fleet.
- `RATELIMIT_BACKEND=postgres` — rate-limit and login-failure counters live in the shared database.
- `TUTOR_MCP_MEMORY_ENABLED=off` — Markdown memory is node-local and would otherwise diverge.

Startup rejects incomplete or unknown distributed settings. This makes configuration errors visible; it does not make the profile production-ready. The scheduler lease has no running/done state, expiry, heartbeat, or crash recovery, and webhook delivery is not exactly-once. Do not describe these instances as stateless while any node-local feature is enabled.

## Database backup

The SQLite profile keeps all state in `${DB_PATH:-./data/runtime.db}`. Loss of that file resets every learner to `PMastery=0.1` and erases trend windows. Backup posture is part of the product premise, not an afterthought. (On Postgres, use your standard `pg_dump`/PITR posture instead of the SQLite recipes below.)

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

The JSON report separates `eligible` rows from `applied` mutations. In a
dry-run every `applied` value is zero. Review the report, take a fresh backup,
then repeat the exact command with `--apply`. Apply mode is one database
transaction: any category failure rolls the whole run back.

```bash
systemctl --user start tutor-mcp-backup.service
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
  --apply
```

The command refuses to create a missing SQLite file and does not run schema
migrations. Point it only at a database already migrated by the matching server
binary. For PostgreSQL, set `DB_DRIVER=postgres` and `DATABASE_URL`; do not put
the DSN on the command line where process listings can expose it.

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
subsequent apply exactly. Also align backup-object retention with the same
privacy policy: deleting live rows does not erase older backup copies.

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

## Operational constraints

The recommended profile is a **single process on a single host**:

- With `RATELIMIT_BACKEND=memory`, token buckets and login-failure counters reset on restart.
- With `SCHEDULER_MODE=inprocess`, a second process would run the same scheduled jobs.
- With narrative memory enabled (the default), Markdown files are local to one host and concurrent read-modify-write operations are not coordinated across processes.

PostgreSQL-backed rate limits and scheduler leases address only the first two points. They do not provide a shared narrative-memory backend, crash-safe job recovery, or an exactly-once webhook outbox. Until those gaps are closed and load/failure tests exist, treat multi-node as experimental and use single-node SQLite for the robust MVP.

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
