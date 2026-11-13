# Institution service: phase 3

> **Current state.** This phase plan predates the removal of the browser
> surfaces: the institution console (`/console`, `/console/admin/`,
> `/console/progress`), the learner portal (`/learn`), self-service signup and
> email invitations no longer exist. Institutions are provisioned by the
> operator with native key identities; members are managed through
> `POST /api/client/v1/institution/actions`, formations, cohorts and admissions
> through the `/admin/catalog` HTTP API, and staff read progress through the
> MCP tools (`list_trainer_cohorts`, `get_learner_progress`,
> `get_cohort_insights`, `get_cohort_statistics`). Passages below that named the
> removed surfaces have been updated accordingly.

Baseline: `5ea49431997e39fafc60591cec16ce470262978e`, published on both
`staging` and `main`, with successful CI. Phase 3 completes the learner journey
and the institution acceptance scenario M1.

## Contract

- A learner sees published cohorts in their current institution, plus their own
  historical enrollments. Drafts and other learners' data are never exposed.
  The learner chooses a cohort explicitly; its published version is authoritative.
- `open` permits immediate enrollment, `invitation` requires an explicit cohort
  invitation, and `approval` records a pending request. Pending requests and
  invitations consume no seat. An authorized formation manager approves or rejects
  requests through the `/admin/catalog` admissions API; an approved learner then
  confirms enrollment.
- Admission decisions are bound to a membership and cohort. Invitations and
  approvals remain valid for rejoining until revoked. Rejecting or cancelling a
  request permits a new request; a revoked admission requires a new staff decision.
- Enrollment requires an open, unexpired cohort and a non-archived formation.
  Capacity, enrollment and tutor domain creation commit together. Enrollment
  before a cohort starts is permitted; an ended cohort cannot accept new joins.
- Leaving releases exactly one seat, closes its open learning session and archives the formation domain, without
  deleting history. Pending requests can also be cancelled. Rejoining the same
  cohort restores the same enrollment, domain, curriculum and progress, subject
  to admission and capacity checks. Suspended/completed enrollments and enrollments
  superseded by an explicit version migration cannot be reactivated this way.
- Choosing another cohort creates its own learning history. Version migration
  remains a separate explicit staff operation from phase 2.
- Mutations require retry keys. Replaying an old leave after rejoining cannot
  cancel the new active state. A receipt describes the original action; the
  learner's current catalog/enrollment view remains authoritative.

## Delivery

1. Shared learner catalog, program read, admission and join/leave store contracts,
   with additive SQLite/PostgreSQL migrations and PostgreSQL tenant RLS.
2. Institution-only MCP tools: `list_available_formations`, `get_my_formations`,
   `get_learner_formation`, `join_formation`, `leave_formation`. Reads require
   `learner:read`; mutations require `learner:write`; every operation requires
   live `learning:self` permission.
3. (Removed since: a `/learn` browser portal; learners use MCP and the native
   client.)
4. Admission decisions and invitations through the `/admin/catalog` admissions
   API, restricted to formation owners or assigned managers (tenant owner/admin
   retain their administrative scope).
5. SQLite/PostgreSQL authorization and lifecycle tests and the real HTTP/MCP
   institution smoke, including two learners, shared report, leave/rejoin,
   preserved evidence and released seats.

## Acceptance

Verify capacity races and retry behavior, cross-tenant and cross-learner denials,
stale memberships, immutable published curricula, no learning writes after leave,
and isolation of learner credentials from administrative routes. Extend the
smoke with MCP self-enrollment and complete the existing required
verification/publication workflow. Badges, collective statistics, trainer
dashboards and public deployment remain later phases.
