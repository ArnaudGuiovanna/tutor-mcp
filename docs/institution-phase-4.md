# Institution service: phase 4

Baseline: `9d1b2bb8e5a52a379e6c81627e86d0fb02c71652` on main and staging.

## Contract

- Staff can discover their published institutional cohorts and read cohort and
  individual enrollment progress in `/console/progress` and through MCP.
- Owners/admins have institution-wide progression access; pedagogy
  managers have their owned/assigned formations; trainers have explicitly assigned
  cohorts. Multiple staff roles combine grants. Auditor-only, support/service accounts and learners
  cannot use these surfaces. Private free-domain compatibility formations are
  excluded. Membership, MFA, token version and assignments are checked on every
  request, including each page of a result.
- MCP uses the separate `progress:read` OAuth capability. The legacy learner
  bundle and formation-authoring grants never imply access to other learners.
  Console reads require the existing live browser session and MFA gate.
- Formation managers grant/revoke cohort trainer access by email in the console,
  with single-use CSRF and audited idempotent set operations. Revocation works
  even after the trainer's membership/roles change.
- A named roster and individual progress are staff-only. Concepts without reviews
  have no reported mastery estimate; reported mastery is an estimate, not a grade
  or certification. Cohort concept averages require five distinct observed active
  learners per concept. Counts cover the complete cohort; lists are paginated.
- Reads use canonical enrollment/concept IDs and the enrollment's published
  version, including historical enrollments. They never follow a migrated domain
  into another cohort's progress. Raw answers, chats, affect and private profile
  data are excluded.
- Evidence includes review counts, last-review dates and bounded recent session
  summaries. Deterministic attention signals describe observable conditions only.
  The client AI writes the synthesis, cites enrollment/concept IDs and dates,
  respects missing evidence and pagination, treats labels as data, and makes no
  diagnosis or unsupported claim about the learner. No server inference or stored
  generated synthesis is introduced.
- New progress surfaces share stable public errors: `invalid_request`, `forbidden`,
  `not_found`, `unavailable`. Missing and out-of-scope resources are indistinguishable;
  internal database errors are never returned to the client.

## Acceptance

Verify SQLite and PostgreSQL authorization (including real runtime RLS in the
institution smoke), revoked assignments and memberships, scope/refresh boundaries,
pagination, empty evidence, the five-person threshold, migration isolation,
browser session separation and HTML escaping. Exercise staff OAuth, MCP cohort and
  individual reads and the console dashboard in the institution smoke. Finish with
  the repository verification and staging-to-main publication workflow.

Badges, learner-facing collective comparisons and worker aggregates, collective
weights, public deployment and external-client acceptance remain later phases.
