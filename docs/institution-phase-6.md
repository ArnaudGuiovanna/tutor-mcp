# Institution service: phase 6 — collective statistics

Baseline: `f926a9499850430195985e4fd387634367379f91` on main and staging.

## Contract

- The worker periodically computes anonymous aggregates for every published
  institutional cohort and stores the latest snapshot in dedicated tables:
  per-cohort learner counts, mean and median of per-learner average mastery,
  per-concept observed-learner counts with mean and median mastery, completion
  and badge holder counts. Aggregates are never computed on the request path.
- A cell is stored and returned only when it concerns at least five distinct
  observed active learners (the phase 4 threshold, one shared constant). A
  suppressed cell is `NULL` in storage and `insufficient_data` in responses,
  never zero. Complementary suppression: a count of holders (completion, badge
  kind/milestone) is reported only when both the holders and the non-holders in
  the cohort are zero or at least five, so a suppressed small group cannot be
  recovered by subtraction.
- A cohort whose observed active learners number fewer than five stores no
  figure at all, including no learner count; its row only records that
  computation happened.
- Aggregates use the cohort's published version and canonical concept IDs, and
  exclude estimates written after an enrollment migrated, exactly like phase 4.
  They never follow a migrated domain into another cohort or mix versions.
  Only `active` and `completed` enrollments contribute; cancelled, suspended
  and invited enrollments do not. No cross-cohort, institution-wide or
  cross-institution comparison exists in this phase.
- Reported values are estimates, not grades or certifications. Aggregates carry
  no learner IDs, emails, answers, chats, affect or profile data.
- Only staff read aggregates, with the existing `progress:read` OAuth
  capability and the live phase 4 cohort authorization (membership, MFA, token
  version, formation/cohort assignments, per-request checks). Learners, auditor-
  only, support and service accounts, and private free-domain compatibility
  formations are excluded. A learner-facing comparison is deferred.
- The worker is the only writer. It runs under tenant RLS with the
  least-privilege worker role. It recomputes each cohort in a single
  transaction using upserts keyed by tenant and cohort, so a retry or two
  concurrent workers converge on the same rows and never double-count.
- Every snapshot records `computed_at` and the `policy_version`. A read reports
  `status`: `available`; `insufficient_data` when the cohort is below the
  threshold; or `not_yet_computed` when no snapshot exists. A snapshot older
  than six hours is flagged `stale`. These are payload states, not errors.
- Leaving a cohort, archival, migration and learner erasure take effect at the
  next hourly computation; a cohort that falls under the threshold has its
  stored figures overwritten with `NULL`. This lag, at most about one hour, is
  documented. Only the latest snapshot is kept; there is no history.
- Public errors reuse the stable set: `invalid_request`, `forbidden`,
  `not_found`, `unavailable`. Missing and out-of-scope cohorts are
  indistinguishable.
- No server inference and no stored generated text. The client AI writes any
  narrative from the figures, cites cohort, concept IDs and `computed_at`,
  states suppressed cells and staleness, and infers nothing about individuals.

## Breakdown

1. **Schema and policy.** Tables `cohort_statistics`,
   `cohort_concept_statistics` and `cohort_badge_statistics`; SQLite migration
   and PostgreSQL migration with forced RLS; worker role grants.
2. **Pure aggregation.** Deterministic functions for mean, median and the
   suppression rules, unit tested at four, five and six learners.
3. **Worker computation.** `RecomputeInstitutionStatistics` on the store, run
   hourly by the scheduler for every tenant through the existing tenant-root
   loop.
4. **Staff read.** `GetCohortStatistics` through the shared progress
   authorization, MCP tool `get_cohort_statistics` and a statistics section in
   `/console/progress`.
5. **Smoke, documentation and publication.** Extend the institution smoke,
   document in `docs/installation.md` and `CHANGELOG.md`.

## Acceptance

SQLite and PostgreSQL tests cover the threshold at four, five and six learners,
complementary suppression, completion and badge cells, version and migration
isolation, cancelled and left learners, idempotent and concurrent recomputation,
overwrite to `NULL` when a cohort falls under the threshold, tenant isolation
with the real runtime role, revoked assignments and memberships, scope
boundaries, stale and never-computed states, concept pagination,
generic errors and HTML escaping. Exercise the MCP tool, the console section and
the institution smoke. Finish with the repository verification and the normal
staging-to-main publication workflow.

Learner-facing comparison, institution-wide figures, statistics history, collective
weights (phase 7, thirty-learner threshold), public deployment, real SMTP,
complete erasure and external-client acceptance (phase 8) remain later phases.
