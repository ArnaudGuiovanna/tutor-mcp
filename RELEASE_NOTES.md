# Tutor MCP v0.4.1

Tutor MCP v0.4.1 hardens the supported SQLite single-node profile into a
robust MVP candidate. It fixes cross-domain learner-state collisions, stale
retention decisions, OAuth replay races, fragile MCP streaming, concurrent
narrative-memory writes, and several silent partial-failure paths.

## Highlights

- **Domain-safe pedagogy.** Cognitive state, interactions, misconceptions,
  calibration, and transfer evidence are scoped by learner and domain. Common
  labels such as “functions” no longer share progress across unrelated topics.
- **Correct evidence and retention.** FSRS uses the current age since
  `LastReview`, and non-evidence activities cannot mutate BKT/FSRS/IRT state.
- **OAuth and transport hardening.** Authorization codes are consumed
  atomically, refresh tokens rotate transactionally and are stored hashed, and
  Streamable HTTP/SSE keeps its streaming capabilities past ordinary HTTP
  write deadlines.
- **Durable single-node operation.** Narrative-memory updates are serialized
  per learner path and synced around atomic rename. Invalid deployment modes
  now fail at startup instead of silently degrading.
- **Protocol-level acceptance coverage.** The official MCP Go client connects
  to the authenticated production handler, fetches the tutor prompt, discovers
  tools, and persists a full pedagogical evidence loop.

## Fixes

- Ordered, checksummed SQLite and PostgreSQL migrations add domain identity and
  conservatively backfill only unambiguous legacy evidence.
- Domain creation and concept extension are atomic.
- Required storage failures are returned explicitly; optional failed
  enrichments are exposed as `degraded_components`.
- The obsolete legacy activity router and unreachable helpers are removed.
- CI runs PostgreSQL 17, the exact Go 1.25.12 toolchain, and a blocking pinned
  vulnerability scan.

## Upgrading

- Back up the SQLite database and narrative-memory directory before upgrading.
  Forward migrations run automatically at startup.
- Existing plaintext refresh tokens remain usable until their next rotation or
  expiry; newly issued tokens are stored hashed.
- No configuration change is required for the supported SQLite single-node
  profile.
- PostgreSQL remains available for conformance and controlled experiments; see
  [`OPERATIONS.md`](OPERATIONS.md) and the configuration table in
  [`README.md`](README.md).

## Boundaries

- Multi-node operation remains experimental: narrative memory is local and
  scheduler leases do not provide crash-safe exactly-once delivery.
- Tutor MCP supplies deterministic state and routing; the connected LLM still
  needs to load `tutor_mcp` and close the
  `get_next_activity` → `record_interaction` loop.
- The pedagogical models are auditable heuristics, not a claim of psychometric
  or clinical validation.

## Operational Notes

- Install URL remains unchanged:

```bash
curl -fsSL https://tutor-mcp.dev/install.sh | sh
```

- `latest` release assets keep stable names such as
  `tutor-mcp_linux_amd64.tar.gz`, so installers do not need to know the tag.

## Validation

- `go build ./...`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- PostgreSQL 17 DB suite with the race detector
- `go vet`, `staticcheck`, `deadcode`, and `govulncheck`

Full changelog: [`CHANGELOG.md`](CHANGELOG.md).
