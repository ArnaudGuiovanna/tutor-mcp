# Institution service: phase 5 — badges

Baseline: `b1d41f41bbad0406ec4f354863381db3a47e6268` on main and staging.

## Contract

- Three badge families: a successful mastery challenge for a published concept;
  completion of the published formation (a successful mastery challenge for
  every concept); maintained FSRS memory at 7, 30 and 90 days per concept.
- Mastery and completion require evaluated, passed, curriculum-valid attempts
  with effective trust under the same deployment policy as demonstrated mastery.
  A high BKT estimate, elapsed course dates or a `completed` enrollment alone
  cannot award a badge. The client cannot request an award or choose its date.
- Retention requires at least three successful, unassisted, assessment-linked
  RECALL responses with FSRS updates applied. Each is at or after its stored
  FSRS due date and at least 24 hours after the last exposure. Failed reviews
  reset the qualifying run. The run must span the milestone from its initial
  review to its final response. Waiting without a new successful recall awards
  nothing. Response timestamps, not delayed grading, determine the interval.
- Automatic attribution accompanies persisted learning evidence in the same
  transaction. Each enrollment/concept/milestone/policy combination is awarded
  once, with dates and evidence identifiers. Independent adjudication can also
  supply the mastery evidence. No answers or generated images are duplicated.
- Awards remain dated history, including after leaving or migration. They do
  not claim permanently retained memory or an external certification. Reads
  indicate when supporting evidence is no longer valid/available. Evidence is
  never borrowed from another enrollment or version. New version enrollment
  earns its own badges. Completion does not close access to FSRS revision.
- Learners read their own badges with `learner:read` and in `/learn`; staff
  use `progress:read` and the existing live cohort authorization. Pagination,
  live membership/MFA checks, tenant RLS and generic errors apply. Badge data
  participates in learner erasure. No email or external notification is sent.

## Acceptance

SQLite and PostgreSQL tests cover proof/trust boundaries, retry and transaction
atomicity, all-concept completion, FSRS milestones and response-time boundaries,
failures and assistance, isolation and revocation, historical enrollment reads,
and erasure. Exercise MCP and browser reads and the real institution smoke,
then use the normal staging/main publication and CI workflow.
