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
