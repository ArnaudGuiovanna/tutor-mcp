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

## Acceptance

Tests cover the table inventories, an end-to-end erasure on a five-learner
cohort (canonical state, credentials, enrollment, domain, immediate statistics),
resuming a request created before the new phases, the legal-hold block, and a
restore that loses learning records. The repository verification and the normal
staging-to-main publication workflow apply.
