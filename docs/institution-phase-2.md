# Institution service: phase 2

## Audit and scope (29 September 2026)

Baseline: `adf81b34cf321fd82cd1e187edf3d6c10b55264a`, shared by `main`
and `staging` after promotion of PR #183. Phase 0 is merged. Phase 1 supplies
signup, verified invitations, a separate console session, confirmed TOTP,
recovery codes, member management and an institution smoke using real accounts.
It does not yet satisfy every identity prerequisite for the next phase:

- `GetPrincipal`, authorization codes and refresh tokens still require a learner
  profile. The smoke gives the manager both `pedagogy_manager` and `learner`.
- `/admin/catalog` accepts the same OAuth bearer as `/mcp`. Console cookies are
  separate, but the administrative HTTP API is not yet separated.
- Enrollment creates a catalogue row without a tutor domain. The smoke creates
  a separate free domain, so its success does not demonstrate the catalogue bridge.
- Catalogue JSON is exported Go field names; formation ownership, version reads,
  cloning, archive, pedagogical metadata and version migration are absent.

Invitations are emailed only: the security corrections in PR #183 deliberately
removed invitee bearer links from the inviter's console. Preserve that behavior;
operator bootstrap still prints its explicit owner invitation. Public opening,
email-provider configuration, GDPR erasure and the learner portal are later phases.

The maintainer authorized fixing the identity prerequisites and implementing
phase 2. No server-side model inference is introduced.

## Implementation sequence

### 2a. Identity and authorization prerequisites

Support a membership without a learner in principal lookup and the complete
OAuth authorization-code/refresh flow, including routing, token revocation and
MFA. Do not create a fictitious learner to make a staff identity work. Existing
learner and local/hobby authentication remain compatible.

An institution's administrative HTTP API uses the verified console session
under `/console/admin/`, with CSRF protection on writes. Public-client OAuth
bearers cannot access it. The old `/admin/` institution endpoints fail closed.
MCP exposes only the explicit pedagogical operations described below, authorized
by permission and by formation ownership/assignment; membership and tenant
administration stay in the console. A learner OAuth grant must not implicitly
grant new pedagogical capabilities.

### 2b. Catalogue and formation rights

Use `snake_case` JSON; expose a complete version (modules, concepts,
prerequisites and pedagogical definitions). Each concept has a stable authoring
key, a display label, description, observable objectives and assessment criteria.
The published concept row ID is shared by every learner using that version.

Record `owner_membership_id` and explicit formation assignments. Owners/admins
manage all formations; pedagogy managers manage only their own or assigned
formations. Trainer assistants retain their existing assigned-cohort read access.
Enforce the same authorization for reads, writes and idempotent replays.

Persist an enrollment policy (`open`, `invitation`, `approval`) on the formation.
Administrative cohort enrollment remains explicit; learner self-service is
phase 3. New drafts clone a version with fresh row IDs and preserved stable keys.
Published content cannot change. Removing a published formation archives it and
blocks new enrollments; historical enrollments keep working. Only an entirely
unpublished formation can be physically deleted.

### 2c. Transactional catalogue-to-tutor bridge

Enrollment and domain creation commit together, including the initial curriculum,
concept state, and exact enrollment/concept mappings. Domain creation must not
invoke the legacy path that invents a private formation for each learner.
Retain the existing free-domain compatibility path separately.

Persist the source enrollment/version on the domain. Learning sessions,
interactions, assessments, narrative memory and canonical learner concept state
resolve to the real cohort enrollment. Unknown concepts in a formation domain
fail closed; never silently quarantine them as unrelated legacy evidence.
Fix cohort counts so joining multiple concept states does not count a learner
multiple times.

Published formation domains cannot be rewritten through `add_concepts`,
`publish_curriculum_revision` or a direct store update. Free domains are governed
by the institution policy; local/hobby keep their existing behavior. Domain
deletion must not erase the formation's history.

### 2d. Authoring and explicit version migration

Provide `draft_formation`, `add_formation_concepts`, `get_formation_version` and
`publish_formation` MCP operations with explicit pedagogical permissions,
transactional batches and bounded input. Staff without `learning:self` can use
these tools but cannot call learner tools. Console/API and MCP use the same
store contracts and validation.

Publishing a new version leaves existing enrollments pinned. A formateur can
explicitly migrate an enrollment to a cohort of another published version of
the same formation. Preserve source history, use stable keys to match definitions,
and reuse curriculum reconciliation to retain unchanged evidence and invalidate
changed/retired definitions. Audit the source and target; seat movements and
curriculum changes are atomic. No silent migration and no rewriting old evidence
as if it had been collected on the new definition.

## Acceptance gates

- SQLite and PostgreSQL: staff OAuth without a learner, refresh, MFA, revocation,
  console cookie/CSRF boundaries and rejection of an MCP bearer on admin routes.
- Ownership and assignments: learners and unrelated managers cannot read/change
  a formation or replay a cached administrative response; cross-tenant IDs fail.
- Catalogue: snake_case, typed metadata, valid prerequisite graph, immutable
  published versions, cloning and archive/draft deletion.
- Two learners in one cohort obtain different domains/enrollments but identical
  published concept IDs. Their recorded responses and mastery enter the same
  cohort report. Failed enrollment rolls back its seat and domain.
- Learner curriculum mutation is refused for formation domains at both MCP and
  store boundaries; permitted free domains continue to work.
- New publication leaves old enrollments unchanged. Explicit migration keeps
  unchanged evidence, invalidates changed definitions, preserves history and
  respects target capacity.
- Extend `scripts/smoke-institution.py` to exercise these real HTTP/MCP journeys
  with PostgreSQL runtime roles. Run required local checks, then publish to
  `staging`; promote to `main` only after required GitHub checks pass.

Phase 3 (learner catalogue/join/leave tools and `/learn`), trainer dashboards,
badges and collective statistics are not part of phase 2. This phase's bridge
is a prerequisite for them, not proof that milestone M1 is complete.
