# Tutor Desktop 0.2.1

Fixes "Permissions de session inconnues" (Unknown session permissions) when signing in
with a public key. Version 0.2.0 requested `formation:contribute`, then rejected that
right in its own local validation, before any call to the server.

The client now accepts the combinations of rights needed by the owner, the
instructor and the learner. Unknown rights, duplicates and non-canonical
requests are rejected before anything is signed. Effective permissions remain
enforced by the server; the client adds no role.

The tests use the sign-in function called by the interface, with a test HTTP
server that verifies NIP-98 signatures, the requested rights and the revocation
of the intermediate session. They also cover an owner without a learner profile
and the absence of automatic administrative delegation.

Install `Tutor_0.2.1_x64-setup.exe`, then reconnect the account to Tutor.
Existing vaults, backups, keys, conversations and progress are preserved.
No re-import or key regeneration is needed.

SHA-256 checksums and Windows reports accompany this release.
