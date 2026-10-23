# Institution service: phase 8 — erasure and restore verification

Baseline: `ca2eaf5d13173de518b255cac697be3a4c77a4e3` on main and staging.

This project is published as open-source software that anyone can run. Hosting,
a mail provider, legal texts and acceptance with a real client are decisions of
whoever operates an instance; they are not part of this repository and are not
delivered here. This phase delivers the technical guarantees an operator needs.

## Contract

- An erasure request removes everything the system holds about a learner except
  what is retained by design: the append-only audit trail, legal holds, the
  request record, and a scrubbed identity skeleton. Credentials and grants are
  erased first. Domains are scrubbed and archived; enrollments are cancelled and
  lose their objectives and seat.
- Before the first deletion, a separate `revoke_access` checkpoint revokes the
  institution membership and its version, archives the domains and cancels the
  enrollments. Existing bearer and browser sessions fail their live membership
  check. The global account and memberships in other institutions stay intact.
  A legal hold prevents this checkpoint from starting; a failed transaction
  rolls back revocation, archival and seat release together. Older in-progress
  requests acquire this checkpoint before their next deletion.
- Cohort reservations are released exactly once from the reservations actually
  held by the erased learner's enrollments. Retrying an erasure never consumes
  another learner's place. The upgrade also reconciles existing cohort counters
  against enrollment reservations to repair places left behind by older erasures.
- Anonymous cohort statistics are recomputed in the erasure transaction, so an
  erased learner stops contributing immediately and a cohort that falls under the
  five-learner threshold loses its figures at once.
- A test enumerates every table with a `learner_id` column and fails unless it is
  erased or listed with a reason. Requests created before this phase acquire the
  new phases when they next run.
- Curriculum history is append-only by database trigger and is not erased; this
  limit is documented.
- Restore verification checksums the learning record, curriculum and institution
  structure. A test requires every tenant table to be checksummed or explicitly
  exempt with a reason, and a restore missing learner state must not verify.
- The worker role receives only the privileges the erasure needs: column-level
  selects on credential tables (no token value), deletes on learner tables, and
  column-level updates on domains and enrollments.

Upgrade requires SQLite migration `0080` or PostgreSQL migration `0072`, followed
by reapplying `deploy/postgres-roles.sql`. The PostgreSQL credential tables gain
non-secret `erasure_row_id` identifiers, allowing bounded deletes without reading
token values. The additive identity trigger preserves ordinary global password
changes while preventing a tenant's credential scrub from changing the global
password. An account already affected by an older erasure needs a password reset;
the upgrade cannot recover a password hash that the old binary overwrote.

## Acceptance

Tests cover the table inventories, an end-to-end erasure on a five-learner
cohort (canonical state, credentials, enrollment, domain, immediate statistics),
resuming a request created before the new phases, the legal-hold block, and a
restore that loses learning records. The repository verification and the normal
staging-to-main publication workflow apply.
