# Operations runbook — tutor-mcp

Practical recipes for running the server. Companion to the README, which describes *what* the project does. This file describes *how* to keep it alive.

## Database backend

By default the server stores everything (interactions, OLM, BKT/FSRS/IRT state, refresh tokens, calibration history) in a single SQLite file at `${DB_PATH:-./data/runtime.db}` — the recommended profile for single-node deployments (see *Database backup* below).

For horizontal scaling it can instead run on **PostgreSQL**: set `DB_DRIVER=postgres` and `DATABASE_URL=postgres://user:pass@host:5432/db?sslmode=require`. The schema migrates automatically on boot (advisory-lock serialized, so concurrent cold-starting instances are safe), and is checksum-guarded — a changed `schema_pg.sql` against an already-migrated database fails fast rather than silently diverging. Tune the pool with `DB_MAX_CONNS` (default 10), keeping `DB_MAX_CONNS × instances < Postgres max_connections`.

### Multi-node (stateless) deployment

Run N instances behind a load balancer, all pointed at the same Postgres, with:

- `JWT_SECRET` **identical** on every instance (tokens are verified anywhere).
- `SCHEDULER_MODE=distributed` — each scheduled run is leased in the DB so a job/nudge fires exactly once across the fleet, not once per instance.
- `RATELIMIT_BACKEND=postgres` — rate-limit and login-failure counters live in the shared DB, so throttling and brute-force lockout stay coherent across instances (otherwise each instance counts only its own traffic).

With those set, instances hold no per-learner state between requests; the next ceiling is LLM throughput/cost and Postgres `max_connections` (front with PgBouncer if you run many instances).

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

(currently the `session_id` is not in the pipeline-decision log — add it if you need cross-event correlation; see `tools/activity.go` and the `record_interaction` params).

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

## Operational constraints (single-node / in-memory state)

This server is designed to run as a **single process on a single host**. Several pieces of state live only in process memory, with no shared store behind them:

- **Rate limiter** (`auth/ratelimit.go`) — per-client token buckets are held in memory.
- **Login-failure tracker** (`auth/login_failures.go`) — the brute-force lockout counters are held in memory.
- **Scheduler** (background jobs in `engine/`) — runs in-process, single-node.

Two consequences follow, and both are deliberate trade-offs for this deployment shape:

- **A restart clears throttle and lockout state.** Every process restart or crash-loop resets the rate-limiter buckets and the login-failure counters to zero. An account locked out by repeated bad passwords becomes reachable again the moment the server is restarted (e.g. after a binary deploy, see *Pre-migration safety* above). Avoid restart-storming the service if a lockout is actively protecting you.
- **State is not shared across instances.** Each process keeps its own buckets and counters. **Do not run multiple instances behind a load balancer expecting shared throttle or lockout state** — doing so multiplies the effective rate limit by the instance count and lets brute-force attempts spread across instances, weakening both protections. The in-process scheduler has the same constraint: running two instances would double-fire scheduled jobs.

If you ever need to scale beyond one node, the throttle/lockout state must move to a shared store (e.g. Redis) and the scheduler must gain leader election first. Until then, treat single-node as a hard requirement.

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
