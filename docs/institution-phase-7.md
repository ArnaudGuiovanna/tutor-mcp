# Institution service: phase 7 — collective weights

Baseline: `2950c0e0c2c3f268fb0ac5b1da61909740199d2e` on main and staging.

## Contract

- For each concept of a published institutional formation version, the worker
  derives collective BKT parameters (learn, forget, slip, guess) from the
  learners of that version across its cohorts. A concept qualifies only with
  at least thirty distinct learners, each with at least five admissible committed
  observations of it, in `active` or `completed` enrollments. FSRS review counts
  do not determine eligibility. Below that, no compatible weight is published.
- A collective parameter is the median of the contributors' independent estimates,
  clamped to fixed safe bounds. A new weight version is published only when it
  moves at least 0.01 from the current one, and never by more than 0.02 per
  version, so the collective cannot jump or oscillate. The estimate is a robust
  consensus of individualized heuristic estimates, not a maximum-likelihood fit;
  it is documented as such and carries its policy version.
- Independent estimates live in `concept_bkt_estimates`, separate from applied
  `concept_states.p_*`. They start from the immutable reference and use only
  admissible current-version responses, including the latest response, with no
  inherited mastery, collective parameters or FSRS stability. The worker checks
  that their evidence remains current before accepting a contribution. See the
  [parameter policy and correction plan](bkt-parameter-policy.md).
- Weights are append-only: every version records its parameters, contributor
  count, policy version and computation time, and the history is retained. The
  current weight is the highest version. Weights contain no learner identifier
  and survive learner erasure, which is permitted because each is aggregated
  over at least thirty learners.
- Weights take effect lazily, inside the learner's own interaction transaction,
  never from the worker: the worker never writes learner state, so it cannot
  race a learner's update. At a learner's next recorded interaction on a
  concept whose current weight is newer than the one last applied to that
  enrollment, parameters change once:
  - a state with no review yet takes the collective parameters as its prior;
  - a state with reviews moves 25% of the way toward the collective, by at most
    0.05 per parameter, then is clamped.
  Mastery, FSRS scheduling, review counts and evidence are never modified.
- Each application is recorded in a ledger keyed by enrollment and concept with
  the weight version, mode (`prior` or `blend`) and time, so a version is
  applied at most once, and in the interaction's observation as
  `bkt_collective_weights` with the parameters before and after. The ledger
  carries the learner identifier and is removed by learner erasure.
- Only enrollments in published institutional formations with `active`
  status are affected. Local, hobby, legacy and quarantined domains are not.
  Weights never cross tenants or formation versions.
- Staff read the current weight of each concept, with version, contributors and
  computation time, in `get_cohort_statistics` and the console. They see no
  individual contribution. A concept without a weight shows none.
- No server inference and no stored generated text.

The corrected source uses collective policy `institution-collective-weights-v2`.
Historical v1 weights remain in history but are not applied by the new runtime.
There is no backfill from historical applied parameters. New compatible weights
require the actual eligible source volume and retain globally increasing version
numbers. Personal domains use fixed reference parameters while their mastery and
FSRS state continue to evolve.

## Breakdown

1. Schema: `concept_collective_weights`, `collective_weight_applications`, `concept_bkt_estimates`,
   PostgreSQL RLS and worker grants, erasure registration.
2. Pure policy: median, bounds, hysteresis and step cap, prior/blend rules.
3. Worker: `RecomputeCollectiveWeights`, run every six hours per tenant.
4. Application inside the interaction transaction, with trace.
5. Staff read, smoke, documentation and publication.

## Acceptance

SQLite and PostgreSQL tests cover twenty-nine versus thirty contributors, fewer
than five admissible observations, median robustness to an outlier, hysteresis and step cap,
append-only versions, prior versus blend, single application per version,
unchanged mastery/FSRS/reviews, rollback with the interaction, non-institution
domains, tenant isolation with the real runtime role, erasure of the ledger,
and the staff read. The correction also tests real responses through the complete
source/weight/application chain, contrasting histories, bounded source windows,
used-window invalidation, old-policy exclusion and fresh evidence after migration.
Finish with the repository verification and the normal
staging-to-main publication workflow.

Public deployment, real SMTP, complete erasure and external-client acceptance
(phase 8) remain later.
