# Store scope contract

Every new exported business method on `db.Store` must receive a
`models.TenantScope` or `models.Principal`. The
`TestNoNewUnscopedStoreMethods` test records a SHA-256 fingerprint of legacy
methods still exposed without either type. Adding or renaming an unscoped
method fails the CI gate.

The frozen exceptions fall into four categories: the legacy teaching API
maintained during dual reads and writes, global authentication/control-plane
operations, migrations/health checks, and test access (`RawDB`). They are not
patterns for new code. `VerifySchemaCurrent`, the health-check exception added
on 2026-08-12, reads only the global migration ledger for `/ready` and never
accesses business data. SaaS, catalog, RBAC, quota, usage, job and outbox methods
all use a typed tenant boundary.

When a credential must be resolved before its tenant is known, the method
receives a typed capability without a freely supplied tenant selector
(`BillingWebhookCredential`, `ServiceAccountCredential`,
`SupportAccessCredential` or an already verified federated assertion). The
global routing table stores only a digest and supplies the authoritative
tenant. The subsequent business read immediately uses `SET LOCAL` and RLS.

Gradual replacement must not silently expand the fingerprint's allowlist.
Removing a method may change the fingerprint after review; every new exception
requires an explicit justification in this document. Tenant-owned queries also
remain protected by transactional `SET LOCAL`, composite foreign keys and
`FORCE ROW LEVEL SECURITY` on PostgreSQL.

The local and hobby implementation adds nine explicitly documented exceptions
to this boundary. `EnsureInstallation` validates the global deployment marker
before starting any transport. `EnsureLocalIdentity` transactionally creates
the single learner in a SQLite database marked `local`. The seven methods
`CreateHobbyLink`, `AcceptHobbyInvite`, `HobbyLinkValid`, `ResetHobbyPassword`,
`GetUserByLoginName`, `ListHobbyUsers` and `DisableHobbyUser` serve authentication
before a principal is resolved, or SSH administration. They reject PostgreSQL
and installations not marked `hobby`. Links carry a random secret whose digest
alone is stored; transactional consumption selects the authoritative identity.
None of these methods is an MCP tool. After authentication, both new profiles
use the existing learner principal's transactions and permissions. The test
fingerprint explicitly includes these exceptions; business methods remain
subject to the general rule.

`SetHostLLMDemonstrationPolicy` is a process-level configuration setter like
`SetIntegrationSecretKeyring`: it is called once at startup from the parsed
configuration, touches no tenant data and only changes the SQL projection that
derives effective evaluation trust. It is part of the fingerprint for the same
reason.

Institution accounts (browser console, invitations and self-service signup)
resolve their tenant from a typed secret, like the credentials above:
`ConsoleSessionCredential`, `InvitationCredential`, `SignupCredential` and
`PasswordResetCredential` carry the raw link or cookie token and the store keeps only its digest. Two
global catalog reads are added to the fingerprint: `TenantSlugAvailable`
checks whether an institution identifier is free before a signup email is
sent, and `PlanIsActive` validates the configured signup plan at startup.
Neither reads tenant-owned data. The MFA ceremony, console membership reads
and member administration take a `models.TenantScope` or `models.Principal`.

`GetLearnerDomainByID` (2026-10-04) narrows the legacy `GetDomainByID`: it
adds the owning learner to the query so that a domain identifier supplied by an
MCP client can neither reach nor probe another learner's domain inside a
tenant, where row-level security does not separate learners. It runs in the
caller's tenant transaction like the legacy teaching API it replaces at those
call sites, and `GetDomainByID` remains only for trusted system paths.

`WithTenantReadTx` shares the same explicit tenant boundary as `WithTenantTx`.
The 26 tools in the reviewed `readOnlyTools` map use it; an unfamiliar `get_`
name does not grant read-only routing. `get_curriculum_snapshot`,
`get_memory_state`, `get_next_activity` and `get_metacognitive_mirror` retain
writer transactions because their getters can persist state or queue work.
Nested calls inherit the current transaction, scope and root identity. A
writer wrapper inside a read transaction cannot promote it; a read wrapper
inside a writer transaction does not split its atomic snapshot or commit.

After SQLite migrations, `NewSQLiteStore(writer, path)` opens an independent
four-connection reader pool for the same existing file. The writer retains
one connection and `BEGIN IMMEDIATE`. Readers use deferred transactions with
`mode=ro` and `query_only=ON`; both ordinary DML and `INSERT ... RETURNING`
are rejected by SQLite. WAL lets a writer commit while readers retain their
earlier snapshots. The constructor takes writer ownership on success and
`Store.Close` closes both pools. HTTP, local/hobby profiles and memory-index
use this constructor. Constructing a Store does not run migrations.

Embedders using `NewStore(rawDB)` without a separate reader retain a protected
compatibility path: one connection is pinned, its previous `query_only` value
is saved, the guard is enabled before a deferred transaction, and restored
after the transaction ends. Restoration uses an independent bounded context;
if it fails, the connection is discarded. This path shares writer-pool capacity
and provides no extra read concurrency. Prefer the configured constructor for
file-backed SQLite runtimes.

PostgreSQL uses its existing pool once, with `TxOptions.ReadOnly=true` for
new read transactions and the same transaction-local tenant/user/membership
settings and RLS. Pool telemetry labels physical pools as `writer` and `reader`
for configured SQLite, or `shared` for PostgreSQL and rawDB compatibility;
it never counts the shared PostgreSQL pool twice.
