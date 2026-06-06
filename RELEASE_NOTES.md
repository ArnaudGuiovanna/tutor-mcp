# Tutor MCP v0.4.0

Tutor MCP v0.4.0 adds an **opt-in PostgreSQL backend** and **stateless
multi-node operation**, so the runtime can scale horizontally beyond the
single-node SQLite ceiling. SQLite stays the default — existing single-node
deployments are unchanged and require no configuration.

## Highlights

- **PostgreSQL backend (opt-in).** Set `DB_DRIVER=postgres` + `DATABASE_URL` to
  run on Postgres via the pure-Go pgx driver (no CGO). The persistence layer was
  inverted behind a `store.Store` port and a single conformance suite is
  replayed against both backends, so behaviour stays equivalent. Dialect-aware
  schema, `?`→`$N` rebind, RETURNING/ON CONFLICT, and `DB_MAX_CONNS` pool tuning.
- **Stateless multi-node.** Run N instances behind a load balancer against a
  shared Postgres:
  - `SCHEDULER_MODE=distributed` — each scheduled run is leased in the DB, so a
    job/nudge fires exactly once across the fleet.
  - `RATELIMIT_BACKEND=postgres` — shared rate-limit and login-failure stores
    for coherent fleet-wide throttling and brute-force lockout.
  - `JWT_SECRET` shared across instances; concurrent cold-start migrations are
    serialized with a Postgres advisory lock.
- **Row-level write concurrency on Postgres.** The `record_interaction` hot path
  uses `SELECT … FOR UPDATE` and the webhook queue uses `SKIP LOCKED` — the same
  lost-update protection Postgres-side that `BEGIN IMMEDIATE` gives on SQLite.

## Fixes

- First-touch lost update on `concept_states` under concurrency on Postgres
  (new `GetOrCreateConceptStateForUpdate` materializes the row before locking).
- nil-pointer panic when a self-transacting method runs inside a `WithTx`
  callback (new `inTx` composes onto the current transaction).
- Postgres migration anti-drift guard: `MigratePostgres` records the schema
  checksum and refuses to boot if `schema_pg.sql` drifted (SQLite parity).

All three are covered by regression tests, green on real PostgreSQL and SQLite.

## Upgrading

- **No action required for SQLite users** — `DB_DRIVER` defaults to `sqlite` and
  behaviour is unchanged.
- To adopt Postgres, see the multi-node runbook in
  [`OPERATIONS.md`](OPERATIONS.md) and the configuration table in
  [`README.md`](README.md).

## Operational Notes

- Install URL remains unchanged:

```bash
curl -fsSL https://tutor-mcp.dev/install.sh | sh
```

- `latest` release assets keep stable names such as
  `tutor-mcp_linux_amd64.tar.gz`, so installers do not need to know the tag.

## Validation

- `go build ./...`
- `go test ./...` (SQLite default and against a real PostgreSQL 17 via
  `TUTOR_TEST_PG_DSN`)

Full changelog: [`CHANGELOG.md`](CHANGELOG.md).
