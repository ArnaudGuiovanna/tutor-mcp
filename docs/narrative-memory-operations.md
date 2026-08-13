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
