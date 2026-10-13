# BKT parameter policy and independent collective calibration

## Implementation plan

This correction reuses the existing curriculum, assessment, learning-event and
state machinery. It changes how BKT parameters are selected and how a collective
weight obtains its source data.

1. **Separate the calibration source from applied parameters.** Add a
   tenant-scoped estimate store, populated only by admissible committed
   assessments in published institutional formations. Derive each estimate
   from a fixed reference and the learner's own current-version observations.
   The worker aggregates these independent estimates rather than
   `concept_states.p_*` or FSRS review counts.
2. **Make the effective parameter policy explicit.** Personal domains use
   fixed reference parameters. Institutional common domains use the reference
   until a compatible collective version is available, then retain the existing
   bounded prior/blend application. Observations still update mastery and FSRS;
   individualized estimation runs separately for the collective source.
3. **Prove the complete chain and lifecycle.** Test real responses through
   estimation, aggregation and application, including personal stability,
   absence of collective feedback, deduplication, rollback, invalidation,
   tenant/version boundaries, erasure, restore and PostgreSQL runtime roles.
   Review the task-owned diff, run repository verification and publish through
   the existing staging-to-main workflow.

## Parameter roles

| Role | Source and purpose |
| --- | --- |
| Reference | Versioned policy values: learn 0.15, forget 0.05, slip 0.10, guess 0.20. |
| Independent estimate | A bounded individualized heuristic computed from the reference and admissible observations; never seeded from an applied collective weight. |
| Effective parameters | The values used to update one learner's mastery: fixed reference for personal domains, reference or bounded collective application for common domains. |
| Collective weight | The versioned median of eligible independent estimates for one published formation version and concept. |

The source estimator uses the newest twenty admissible observations from at
most the newest 256 scoped assessment-linked candidates, including the current
committed response, and no inherited FSRS stability. Its eligibility count is
the count in that actual estimation window, not a lifetime total or
`concept_states.reps`. Source verification hashes exactly the observations
used; changes outside the window do not influence the estimate. History reads
on the interaction path remain bounded.
The estimator remains a documented heuristic, not a maximum-likelihood fit or
a claim of demonstrated learning effectiveness.

## Personal and common domains

Personal domains include local, hobby, legacy and private compatibility
formations. Their four declared BKT parameters remain fixed even with a long
history or an error-type hint. Mastery, memory state, review dates and activity
recommendations still change with observations.

Only active enrollments in a published institutional common formation receive
collective weights. Active or completed enrollments may contribute source
estimates. No contribution crosses a tenant, formation version, concept or
curriculum revision. Migration does not copy an independent estimate into the
target version.

Activity purpose is preserved. An observation-only diagnostic updates the
posterior without a learning transition; its effective learn/forget transition
parameters in the compatibility `bkt_individualized_params` trace are reported
as zero. `bkt_effective_params` identifies the full declared parameter set.
This does not change the declared reference
or the independently estimated parameters.

## Admissible source evidence

Source evidence must link an interaction to an evaluated, non-invalidated,
decision-bound assessment with a committed response under the learning-event
protocol, in the enrollment's current curriculum and published formation
version. Unlinked legacy observations, invalidated responses and explicitly
marked synthetic or preview evaluations do not contribute. Assessment identity
and the existing single-use evaluation transaction prevent replay from creating
new observations.

These checks establish technical provenance. They do not prove that every
account belongs to a different human or that a host-generated evaluation is
correct. The server's existing evaluation trust and certification rules remain
separate; ordinary host evaluations are not promoted to trusted certification.

## Upgrade and lifecycle

Historical applied parameters are never backfilled as independent estimates.
Existing collective versions remain available in history, but the previous
source policy is not used by the corrected runtime. New compatible versions
start from eligible independent estimates and keep a monotonic version sequence.
Until the new policy reaches its threshold of thirty contributing learners with
at least five admissible observations each, the runtime uses the reference.

The independent estimate is learner data. It participates in tenant export,
erasure and restore verification, has forced PostgreSQL row-level security,
and requires the worker's read/erasure grants. Re-apply
`deploy/postgres-roles.sql` after migration.

## Acceptance evidence

- Contrasting real response histories produce contrasting independent
  estimates and a collective weight different from the initial reference.
- Changing or applying a collective weight does not change its independent
  source or target median. Repeated computation may continue a permitted capped
  convergence toward that fixed target, without feeding its own outputs back.
- Personal effective parameters remain fixed while mastery and FSRS evolve,
  including failures, hints and nonempty interaction history.
- Thresholds use distinct learners and admissible responses, with tests for
  four/five observations and twenty-nine/thirty contributors.
- Duplicate evaluation and transaction rollback cannot advance source counts.
- Invalidation, migration and tenant/version isolation prevent stale or foreign
  contributions. Real PostgreSQL roles enforce the same boundaries.
- Populated source rows participate in export accounting and are erased;
  removing them from a restore
  changes the verification checksum.
