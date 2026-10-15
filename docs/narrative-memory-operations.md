# Shared narrative memory operations

Tutor MCP supports two narrative-memory backends:

- `local`: Markdown below `TUTOR_MCP_MEMORY_ROOT`; intended for the default
  single-process SQLite profile;
- `database`: encrypted, versioned objects in the relational database;
  mandatory when active memory is used in a distributed or production profile.

PostgreSQL selects `database` by default. Set
`TUTOR_MCP_MEMORY_BACKEND=database` explicitly in deployment manifests so a
driver change cannot silently change the memory authority.

## Storage guarantees

`narrative_objects` stores a canonical tenant/enrollment/scope/domain/key identity,
AES-256-GCM ciphertext, key version, optimistic version, plaintext SHA-256,
plaintext size and timestamps. AAD v2 binds tenant and enrollment alongside
every key field, so ciphertext transplanted into another tenant, enrollment or
concept is rejected. Rotation authenticates v1 rows with their original AAD
before rewriting them with v2.

Session lookup has separate versioned projections: a fixed-size SHA-256 domain
digest and a UTC timestamp sort key. Neither changes the original object key,
tenant/enrollment, PK or AAD. The domain comes from the final plaintext after
append/section/file operations; chronological ordering accepts both historical
timestamp encodings while retaining each original key. Reads select at most
three canonical keys across the learner's enrollments, including the recovery
enrollment, and authenticate/check the selected plaintexts.

Writes lock the tenant/enrollment quota row, enforce object/count/byte limits
in the same transaction and use compare-and-swap. `narrative_mutations` makes a supplied
mutation ID replayable without duplicating an append. A reused ID with different
arguments and a stale version are explicit conflicts.

## Local-to-database rollout

1. Back up the database, `TUTOR_MCP_MEMORY_ROOT` and every retained key version.
2. Deploy the additive narrative tables while the backend remains `local`.
3. Configure the current and retained encryption keys, then set the backend to
   `database` on one canary instance.
4. Before accepting traffic, startup authenticates/rotates every existing
   database object and scans the local Markdown tree. Absent objects are
   imported create-only into the canonical legacy enrollment; equal checksums are reconciled; divergent checksums
   stop startup without overwriting either side.
5. Check the structured startup counts `backfill_scanned`,
   `backfill_imported`, `backfill_reconciled`, and zero conflicts. Start the
   remaining instances. Concurrent imports converge through CAS and the stable
   backfill mutation ID.
6. Retain the local tree as a read-only rollback snapshot until the backup
   policy covers the post-cutover database window. Startup never deletes it.

For a conflict, first preserve both sources. If the database copy is
authoritative, move the conflicting local file to a quarantine directory
outside the configured root and retry. If the local copy is authoritative,
take an encrypted database backup, remove only the exact conflicting database
object in a maintenance window, and retry the create-only import. Never bulk
overwrite conflicts.

## Session index repair

After migrations and keyring setup, API/all processes and local stdio perform a
bounded startup repair and continue pending pages outside requests. Pages contain
at most 100 identities; one envelope is decrypted at a time, outside SQL
transactions. Each successful metadata-only CAS is a durable checkpoint.
Shutdown cancels and joins maintenance before closing the database. The restricted
institutional worker does not receive narrative UPDATE privileges or run repairs.

Until repair completes, `get_next_activity` reports
`session_narrative_index` in `degraded_components`; `get_memory_state` reports
`narrative_context`. An incomplete index never silently means an empty history.
Teaching can continue with the explicit degradation. Old application writers
make projections stale by advancing object versions; maintenance repairs them
again, including after the older writers have stopped.

For operator repair or a large restore, use the existing installation and keys:

```sh
tutor-mcp memory-index --local --data-dir /existing/local-profile
tutor-mcp memory-index --profile hobby --data-dir /existing/hobby-profile
# Existing DB_DRIVER / DB_PATH or DATABASE_URL and INTEGRATION_SECRET_KEYS:
tutor-mcp memory-index --tenant tenant_legacy --batch-size 100 --timeout 2m
tutor-mcp memory-index --profile institution --tenant tenant_example
```

Missing installations, databases or keys fail without creating a profile or
regenerating keys. PostgreSQL requires current migrations and a non-owner runtime
role without SUPERUSER, BYPASSRLS or schema CREATE privileges. The command starts
no HTTP listener or scheduler and does not rotate keys/envelopes. Repairs preserve
ciphertext, key ID, AAD version, object version, checksum and timestamps.

The JSON result includes `scanned`, `indexed`, `unresolved`, `remaining`,
`remaining_known` and `complete`. The exact remaining count is computed only on
the terminal page; intermediate/interrupted progress uses `remaining: -1` and
`remaining_known: false`, rather than rescanning all pending objects per page.
An interrupted or unresolved repair exits unsuccessfully; unresolved
objects leave `complete: false`. Fix missing retained keys/corruption, then rerun
the same command. Completed objects are skipped. Canonical pagination advances
past corrupt objects so they cannot starve healthy objects or other tenants.

The local filesystem backend keeps a private `.session-index.json` manifest
with fixed-size domain digests. Requests validate file metadata and read at most
three selected bodies; they never rebuild a missing/stale index. Startup and
maintenance rebuild outside requests. Internal writes update the projection
under the learner lock; a durable dirty marker precedes the content rename, so
a crash or `CommittedWriteError` leaves an explicit incomplete index for repair.

Ordinary external edits/additions/removals/renames invalidate the manifest.
External tools that preserve size and modification time, particularly across a
restart/cache eviction, must explicitly rebuild after editing:

```sh
tutor-mcp memory-index --filesystem --memory-root /existing/legacy-memory
```

This rebuild retains all Markdown sources. One corrupt learner is reported as
incomplete while other learners are repaired. A forced rebuild marks the old
manifest dirty before scanning; a failed/interrupted rebuild cannot reactivate
an old manifest. The command honors the runtime `TUTOR_MCP_MEMORY_MAX_*` limits.
Disabling narrative memory also
disables automatic import/index maintenance.

## Key rotation

Add the new key while retaining old keys and select the new current ID. Startup
authenticates every narrative, re-encrypts old envelopes atomically and does not
change narrative versions or semantic `updated_at`. Remove an old key only
after all instances report zero objects on that key and backups using it have
expired or remain paired with the retained key.

## Rollback

Rolling application instances back while retaining the `database` backend is
safe only to a version that understands the shared schema. A rollback to
`local` is a data migration, not a configuration toggle: stop narrative writes,
take database and keyring backups, export/decrypt the authoritative database
objects to a new local root, verify every checksum and object count, then switch
all instances together. The pre-cutover local tree alone does not contain
post-cutover writes.

The SQL migrations are additive; rollback normally leaves their tables in
place. Dropping them is destructive and must occur only after the authoritative
objects have been exported, verified and backed up.
