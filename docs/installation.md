# Install TUTOR MCP

The local and hobby profiles require v0.6.0 or later. Choose local stdio for
one person on their own machine, hobby for one VPS shared by a small group,
or institution for the existing PostgreSQL deployment model. See
[profile behavior and accounts](profiles.md).

**Release availability:** [v0.6.1](https://github.com/ArnaudGuiovanna/tutor-mcp/releases/tag/v0.6.1)
is available with binaries and checksums for all supported platforms. Use the
installers below or the [source quickstart](../README.md#quickstart).

## Linux and macOS

Download the shell installer from the release's source, inspect it, and run it:

```sh
curl -fsSL https://raw.githubusercontent.com/ArnaudGuiovanna/tutor-mcp/main/scripts/install.sh -o install-tutor.sh
TUTOR_MCP_VERSION=v0.6.1 TUTOR_MCP_INSTALL_DIR="$HOME/.local/bin" sh install-tutor.sh
```

Add `$HOME/.local/bin` to your PATH and restart your MCP client. The installer
selects Linux or macOS and amd64 or arm64, and verifies the archive against
`SHA256SUMS`. Without `TUTOR_MCP_INSTALL_DIR`, it uses `/usr/local/bin` and may
request sudo. To build from source, use the Go version in `go.mod` and run
`go build -o tutor-mcp .`.

## Windows

Download [install.ps1](../scripts/install.ps1), inspect it, then run it from
PowerShell as your normal user:

```powershell
.\install.ps1 -Version v0.6.1
```

The installer chooses amd64 or arm64, verifies SHA-256, and installs under
`%LOCALAPPDATA%\Programs\tutor-mcp`. It adds that directory to the user's PATH.
Restart the MCP client to pick up the new PATH. `-InstallDir` selects another
directory and `-NoPathUpdate` leaves PATH unchanged. Follow your organization's
PowerShell execution policy; the installer does not modify it.

Alternatively, unpack the matching Windows ZIP from the release, verify its
checksum, and use the full path to `tutor-mcp.exe` in your client configuration.
No WSL, Docker, SMTP server or account is required for local stdio.

## Local client configuration

The client starts TUTOR and closes it when the connection ends. Do not run a
separate HTTP service for this mode. If a GUI client cannot find `tutor-mcp`,
replace `command` with its absolute installed path. Add `--data-dir` only when
you want a different private profile directory.

### Hermes

Add this to `~/.hermes/config.yaml`, then start Hermes:

```yaml
mcp_servers:
  tutor:
    command: tutor-mcp
    args: ["--local"]
```

Hermes documents stdio and automatic OAuth for remote servers in its
[MCP guide](https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp).

### Claude Desktop

Open Settings → Developer → Edit Config and merge this into
`claude_desktop_config.json`, then restart Claude Desktop:

```json
{
  "mcpServers": {
    "tutor": { "command": "tutor-mcp", "args": ["--local"] }
  }
}
```

See the official [local server configuration guide](https://modelcontextprotocol.io/docs/develop/connect-local-servers).

### Claude Code

Add a user-scoped stdio server:

```sh
claude mcp add --transport stdio --scope user tutor -- tutor-mcp --local
```

Or use the same `mcpServers` JSON above in the project's `.mcp.json` and approve
that project server in Claude Code. See its [MCP configuration guide](https://code.claude.com/docs/en/mcp).

## VPS prerequisites

Choose a DNS name, for example `tutor.example.org`. Point its A record at the
VPS; publish an AAAA record only if IPv6 reaches that VPS too. Allow inbound
TCP 80 and 443 to Caddy (UDP 443 is optional for HTTP/3). Keep SSH available
for administration. Only Caddy's HTTPS endpoint is public; TUTOR's application
port stays on loopback or inside the container network.

Caddy provisions and renews certificates when DNS, reachability and persistent
certificate storage are configured. See [Caddy automatic HTTPS](https://caddyserver.com/docs/automatic-https).
Public cloud clients need a publicly trusted HTTPS endpoint.

## Native hobby VPS: binary, systemd and Caddy

Install the Linux binary into `/usr/local/bin/tutor-mcp`. Install Caddy using
its [official package instructions](https://caddyserver.com/docs/install).
From a checkout matching the binary version:

```sh
sudo useradd --system --home-dir /var/lib/tutor-mcp --shell /usr/sbin/nologin tutor
sudo install -d -m 0700 -o tutor -g tutor /var/lib/tutor-mcp
sudo -u tutor /usr/local/bin/tutor-mcp init --profile hobby \
  --data-dir /var/lib/tutor-mcp/hobby --public-url https://tutor.example.org
sudo install -m 0644 deploy/tutor-hobby.service /etc/systemd/system/tutor-hobby.service
sudo systemctl daemon-reload
sudo systemctl enable --now tutor-hobby
```

Keep the printed invitation private and open it after HTTPS is ready. Configure
Caddy using [Caddyfile.native](../deploy/Caddyfile.native): replace
`{$TUTOR_DOMAIN}` with your actual DNS name when placing it in `/etc/caddy/Caddyfile`.
Validate and reload:

```sh
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy
curl --fail https://tutor.example.org/ready
```

TUTOR listens on `127.0.0.1:3000`. Your final MCP URL is
**`https://tutor.example.org/mcp`**. Paste it into the remote client, sign in
with the invited account and authorize that client. Account creation alone
does not grant client access.

Run SSH administration as the same service user:

```sh
sudo -u tutor tutor-mcp users invite --data-dir /var/lib/tutor-mcp/hobby
sudo -u tutor tutor-mcp users list --data-dir /var/lib/tutor-mcp/hobby
sudo -u tutor tutor-mcp users reset alice --data-dir /var/lib/tutor-mcp/hobby
sudo -u tutor tutor-mcp users disable alice --data-dir /var/lib/tutor-mcp/hobby
```

## Hobby VPS with Docker Compose

Install Docker with Compose. From the matching release checkout:

```sh
export TUTOR_DOMAIN=tutor.example.org
export TUTOR_VERSION=v0.6.1
docker compose -f deploy/compose.hobby.yml build
docker compose -f deploy/compose.hobby.yml run --rm --no-deps tutor \
  init --profile hobby --data-dir /data/hobby --public-url "https://$TUTOR_DOMAIN"
docker compose -f deploy/compose.hobby.yml up -d
curl --fail "https://$TUTOR_DOMAIN/ready"
```

The TUTOR image runs as UID/GID 65532, with a read-only root filesystem and a
persistent data volume. No TUTOR port is published. Caddy is the only trusted
proxy at `172.30.42.2`; if the subnet conflicts with an existing network,
change the Compose subnet, both static addresses and `TRUSTED_PROXY_CIDRS`
together. Keep the three named volumes across upgrades. Never use
`down --volumes` to upgrade an existing installation.

SSH administration while the service is running:

```sh
docker compose -f deploy/compose.hobby.yml exec tutor /usr/local/bin/tutor-mcp \
  users invite --data-dir /data/hobby
```

Replace `users invite` with `users list`, `users reset alice` or
`users disable alice` as needed. Your final MCP URL is
**`https://tutor.example.org/mcp`**.

## Institutional deployment

Use [institution.env.example](../deploy/institution.env.example) as a template
for separate API, worker and migrator environment files. Supply operator-managed
Ed25519 signing keys, encryption keys, PostgreSQL credentials and SMTP settings;
institutional secrets are never automatically generated by the hobby initializer.
Retain the existing [operations requirements](../OPERATIONS.md).

PostgreSQL and SMTP are external. PostgreSQL URLs must use `sslmode=verify-full`
and an explicit CA file. Run migrations with a dedicated owner login, then set
up the runtime roles in this order:

1. As a superuser, give the owner `CREATEROLE` (first installation only), or
   create the `tutor_api`, `tutor_worker` and `tutor_restore` group roles yourself.
2. As the database owner, apply [postgres-roles.sql](../deploy/postgres-roles.sql).
3. As a superuser, apply [postgres-roles-superuser.sql](../deploy/postgres-roles-superuser.sql)
   (only needed for logical tenant restore).
4. Create one login per process and grant it exactly one group, for example
   `CREATE ROLE api_login LOGIN PASSWORD '…' IN ROLE tutor_api;` and
   `CREATE ROLE worker_login LOGIN PASSWORD '…' IN ROLE tutor_worker;`.

Runtime logins must not own tables or have SUPERUSER/BYPASSRLS; the API refuses
to start otherwise. Reapply step 2 after every upgrade. Integration and
narrative secrets are re-encrypted with the current key by the API process at
startup; the worker only decrypts them.

`bash scripts/smoke-institution.sh` replays this whole sequence in a
disposable directory: TLS PostgreSQL, grants, a local mail sink, an
operator-provisioned and a self-service institution, console sign-in with
TOTP, invitations, OAuth, admin catalog and MCP calls. CI runs it on every
change; operators can run it to check a toolchain before an upgrade.

### Institution accounts

The API serves a browser console at `/console` for owners, admins, pedagogy
managers and trainers. These roles must enroll an authenticator app (TOTP) on
their first console sign-in and receive ten single-use recovery codes. They
enter a code again at every console sign-in and when they connect an AI
client. Learners never use the console: they connect their AI client to
`https://tutor.example.org/mcp` and sign in with the email of their
invitation. Accounts are created only by invitation; self-registration into
the shared legacy tenant is closed with `--profile institution`.

Set `INSTITUTION_SIGNUP` on the API; startup fails without it:

- `operator`: institutions are created with `tutor-control-plane` only.
  Provision the tenant, then create the first owner's invitation link:

  ```sh
  tutor-control-plane -action=provision -slug=acme -name='Acme Academy' \
      -region=eu -plan=PLAN_ID -reason='new customer' -request-id=TICKET
  tutor-control-plane -action=invite-owner -tenant=TENANT_ID -email=head@acme.example \
      -base-url=https://tutor.example.org -reason='owner bootstrap' -request-id=TICKET
  ```

  Send the printed `invitation_url` to the owner. It works once, for 7 days.
- `open`: anyone can create an institution at `/signup`. Nothing is created
  until the requester confirms their email; they then become its owner. Set
  `SIGNUP_PLAN` to the active plan given to new institutions (create it first
  with `tutor-control-plane -action=plan-upsert`). Opening signup requires
  working email delivery.

Owners and admins invite members from the console, one address per line,
with their roles. Invitation links are only emailed, never shown to the
inviter: opening one proves the invitee owns the address. Emails never
contain the institution name, which is chosen by whoever signs up; the linked
page shows it. "Forgot your password?" resets the account password for every
institution member, staff included, and ends their existing sign-ins.

For native systemd, create separate `tutor-api`, `tutor-worker` and
`tutor-migrator` system users. Place role-specific environment files under
`/etc/tutor-mcp/{api,worker,migrator}.env`, readable only by root and the matching
service role. Install [tutor-institution@.service](../deploy/tutor-institution@.service)
and [tutor-migrator.service](../deploy/tutor-migrator.service).
Run `systemctl start tutor-migrator` once, verify its successful
exit, apply database grants, then enable/start `tutor-institution@api` and
`tutor-institution@worker`. Do not enable the migrator as a persistent service.
Use the native Caddy configuration above. The API binds to loopback by default.

For Compose, copy role-specific files to `deploy/api.env`, `deploy/worker.env`
and `deploy/migrator.env`, and put the PostgreSQL CA at
`deploy/secrets/postgres-ca.pem`. These paths are ignored by Git and Docker
build context. Environment files must remain private on the host.

```sh
export TUTOR_DOMAIN=tutor.example.org
docker compose -f deploy/compose.institution.yml build
docker compose -f deploy/compose.institution.yml run --rm migrator
# Apply deploy/postgres-roles.sql with the database owner's connection here.
docker compose -f deploy/compose.institution.yml up -d tutor worker caddy
```

The API trusts only this Compose proxy's address, `172.30.43.2`. Keep the
institutional DCR policy `disabled`, or configure `token` with the existing
initial access token policy. CIMD remains available. Email verification,
tenant isolation, audit and separated roles stay enforced. This release adds
no new SSO provider.

### Formation authoring and enrollment (phase 2)

In institution mode, a pedagogy manager can connect an AI client with the OAuth
scopes `formation:read formation:write`, including when their membership has no
learner role. The existing `learner` scope does not grant these capabilities.
The manager creates a draft with `draft_formation`, supplies modules and concepts
with `add_formation_concepts`, reviews it with `get_formation_version`, and calls
`publish_formation` after deciding to publish. Mutation tools require a unique
`idempotency_key`; reuse the same key only when retrying the same request.

Administrative JSON calls use the browser's MFA-verified console session under
`/console/admin/catalog/`. The old `/admin/catalog/`, assessment-review and
curriculum-review routes reject OAuth bearers in institution mode; their new
prefix is `/console/admin/`. Before each write, request `GET /console/api-csrf`
using the console cookie and pass the returned `csrf_token` as `X-CSRF-Token`,
with that same cookie jar. The token is consumed once. Existing catalogue POST
mutations also require `Idempotency-Key`. JSON fields use `snake_case`.

The catalogue API supports these additional operations (paths below are relative
to `/console/admin/catalog`):

| Method and path | Behavior |
| --- | --- |
| `GET /formation-versions/{id}` | Read the complete version and its concept IDs. |
| `POST /formation-versions/{id}/clone` | Clone into a new draft; requires `Idempotency-Key`. |
| `PUT /formation-versions/{id}/content` | Atomically replace draft modules and concepts; requires `Idempotency-Key`. |
| `PUT /formations/{id}/enrollment-policy` | Set `policy` to `open`, `invitation` or `approval`. |
| `PUT /formations/{id}/trainers/{membership_id}` | Admin assigns/removes a pedagogy manager with `assigned: true/false`. |
| `DELETE /formations/{id}` | Delete an unpublished draft or archive a published formation. |
| `POST /enrollments/{id}/migrate` | Migrate explicitly to the supplied `cohort_id`, on another version of the same formation. |
| `PUT /learning-policy` | Admin sets `allow_free_domains: true/false`. |

Owners/admins manage every formation; pedagogy managers access only their own or
assigned formations, including cohort reports and pedagogical reviews. A cohort
enrollment requires an active learner membership and returns a `domain_id` ready
for tutoring. Concept IDs are shared by all learners on the same published
version. Publishing another version does not migrate existing enrollments.
Migration keeps the domain, closes its session, moves the seat and reconciles
changed definitions; observations retain their original enrollment. Archiving
blocks new enrollment while preserving the existing learning history.

Free domains are disabled by default for new institutions; an admin may enable
them. Local/hobby retain their existing behavior. See the
[phase 2 design and acceptance gates](institution-phase-2.md).

### Learner portal and self-enrollment

Institution learners sign in at `/learn` with the account used to accept their
institution invitation. They choose an institution when they belong to several.
The portal has its own `Secure`, `HttpOnly`, `SameSite=Strict` cookie scoped to
`/learn`, with a 12-hour lifetime and 30-minute idle timeout. Session credentials
are isolated from the console and OAuth. Membership changes invalidate the
session; required MFA also applies to learners who hold staff roles.

The catalog and program pages show published formation cohorts and only the
current learner's enrollment/admission state. The same journey is available in
an AI client:

| Tool | Purpose |
| --- | --- |
| `list_available_formations` | Browse cohorts with `after` / `limit` pagination. |
| `get_my_formations` | Read current and historical enrollments and requests. |
| `get_learner_formation` | Read the published program for `cohort_id`. |
| `join_formation` | Confirm a selected `cohort_id`, with `idempotency_key`. |
| `leave_formation` | Leave a cohort or cancel a request, with `idempotency_key`. |

Read tools use `learner:read`; join/leave use `learner:write`. The existing
`learner` bundle includes both. Only the caller's live learner membership can
use these tools. They are not registered in local/hobby mode.

Admission policies behave as follows:

- `open`: join immediately if a seat is available.
- `invitation`: staff grant a cohort invitation to an existing learner, then the
  learner joins. These grants appear in the learner portal; they are distinct
  from the emailed invitations that create institution memberships.
- `approval`: joining records a pending request. An authorized formation manager
  approves or rejects it at `/console/admissions`. The learner then confirms
  enrollment with a **new** retry key. Approval does not reserve a seat.

The console API also provides `GET /cohorts/{id}/admissions` (paginated by
membership ID) and `POST /cohorts/{id}/admissions`, relative to
`/console/admin/catalog`. Decisions require console CSRF, `Idempotency-Key`,
`membership_id`, `decision` (`invited`, `approved`, `rejected`, `revoked`) and
`expected_version` (zero for a new invitation; otherwise the returned version).
Formation owners/assigned managers and tenant owners/admins can act; unrelated
managers and learners cannot read the requests or decide them.

Enrollment returns the `domain_id` to use with the tutor. Leaving archives that
domain, closes its open session and releases one seat without deleting learning evidence. Rejoining the
same cohort restores the same enrollment, domain and progress, subject to current
policy and capacity. Invitations/approvals remain valid until revoked. Another
cohort starts its own enrollment; migration between published versions remains
an explicit staff operation. Archived formations and closed/ended cohorts reject
new joins. Completed/suspended or migrated-away enrollments cannot be reactivated
by self-enrollment.

Reuse a retry key only for the identical original request. An old receipt reports
that original action; `get_my_formations` returns current state. The
[phase 3 acceptance contract](institution-phase-3.md) and institution smoke cover
the full institution journey M1. Badges and anonymous collective statistics
are described below; learner-facing comparison remains a later phase.

### Staff progress and AI-assisted synthesis

Staff open `/console/progress` using their existing console account and MFA.
Owners/admins see published institutional cohorts across the institution;
pedagogy managers see cohorts of their owned/assigned formations; trainers see
only explicitly assigned cohorts. Private free domains are excluded. Staff roles
combine their grants; learner-only, auditor-only, support and service accounts
cannot use these named follow-up surfaces.

On a cohort page, an authorized formation manager can grant or revoke access
using an existing trainer's email. The trainer must have accepted an institution
invitation with the `trainer` role. Formation-manager assignments and cohort
trainer assignments are distinct. Revocation takes effect on the next read,
including with an already issued OAuth token; another role may still grant access.
The console form uses single-use CSRF and assignment changes are audited.

The same reads are available in an AI client, with explicit OAuth consent to
`progress:read`. Consent covers learner identities and progress within the staff
member's current assignments. Existing `learner` and `formation:*` grants do not
include this capability; reconnect or follow the tool's scope-upgrade challenge.
The tools are registered only in institution mode.

| Tool | Parameters and result |
| --- | --- |
| `list_trainer_cohorts` | `after`, `limit`: cohort IDs, formation/version and whole-cohort enrollment counts. |
| `get_cohort_insights` | `cohort_id`, `after`, `concept_after`, `limit`: named roster, attention signals and concept observations. Roster and concept cursors are independent. |
| `get_cohort_statistics` | `cohort_id`, `concept_after`, `limit`: the latest anonymous worker snapshot (see [collective statistics](#collective-statistics)). |
| `get_learner_progress` | `enrollment_id`, `after`, `limit`: concept estimates, review dates, evidence counts and up to ten recent sessions for that exact enrollment. |

Page sizes default to 20 through MCP and are bounded to 1–100 (50 in the
dashboard). Follow `next_after` and `next_concept_after` before describing a list
as complete. Cohort counts and individual summary counts cover the full scope,
not just the displayed page. The trainer-assignment preview is bounded to 100;
the email form can revoke an assignment beyond that preview.

A concept contributes to observed mastery only when it has a recorded review
(`reps > 0` or a last-review date). Initial priors are not evidence. Individual
averages use observed concepts only; the dashboard separately reports reviewed
concepts versus the full program and the number at or above an 80% estimate.
Concept-level cohort averages use active enrollments only and require five
distinct observed learners **for that concept**. Below that threshold the average
is `null` with `insufficient_observed_learners`. Named individual reads remain
available to authorized staff. This is not a learner-facing comparison feature.

Attention signals mean exactly “no reviews”, “no review in 14 days” or “reviewed
concepts below the 80% estimate”. They do not diagnose disengagement or explain
causes. Assessment counts distinguish trusted evaluations from host-reported
evaluations and exclude invalidated attempts. Raw answers, chat text, affect and
private learner profiles are not returned. After migration, historical reads use
the old enrollment/version and never follow the domain into the target cohort.
If an older server updated a source estimate after migration, that estimate is
withheld and flagged with `historical_estimates_unavailable`; the interface
explains the incomplete history rather than claiming no reviews ever occurred.

Ask the connected AI client to synthesize a cohort or an individual enrollment.
Responses include guidance to cite identifiers/dates, distinguish observations
from interpretations, acknowledge pagination and missing evidence, and treat
names/labels as data. The server neither calls a model nor stores generated
syntheses. Use a client approved by the institution for learner identities and
progress. A single snapshot does not establish a trend or certify competence.

New progress errors use stable codes: `invalid_request` (bad identifier/page),
`forbidden` (membership/capability), `not_found` (missing or outside the assigned
scope) and `unavailable` (retryable internal failure). MCP insufficient-scope
challenges continue to use the existing OAuth mechanism. See the
[phase 4 acceptance contract](institution-phase-4.md).

### Learning badges

Institution learners select **My badges** on an enrolled formation in `/learn`.
Staff select **Learning badges** in an individual enrollment's progress page.
Both views include historical enrollments and use the current membership and
assignment checks; there is no public badge-sharing endpoint.

| Badge | Attribution rule |
| --- | --- |
| Mastery challenge passed | A passed `MASTERY_CHALLENGE` for the published concept, with frozen assessment evidence and effective trust under the demonstrated-mastery policy. |
| Formation completed | Every concept in the enrollment's published version has a qualifying mastery challenge. The enrollment remains available for FSRS revision. |
| Memory maintained — 7 / 30 / 90 days | At least three passed, unassisted, assessment-linked `RECALL` responses at or after their FSRS due dates, each at least 24 hours after the last exposure, spanning the milestone. A failed review restarts the qualifying run. |

The server records awards alongside learning observations or assessment
adjudication, with one award per enrollment/concept/milestone/policy. A model
estimate, ordinary practice, elapsed time or an enrollment status cannot award
a badge. FSRS milestones use committed response times, not the time a delayed
grade arrives. The evaluator is the connected client under the configured
assessment policy, or an independently attested evaluator when configured.

`get_my_badges` requires `learner:read`; `get_learner_badges` requires
`progress:read` and the same live cohort assignments as staff progress. Both
accept `enrollment_id`, optional `after` and `limit` (default 20, maximum 100),
and return `items` / `next_after`. The browser displays 30 badges per page.
Evidence contains assessment/concept/snapshot identifiers and dates, never
copies of the learner's answers. `evidence_status: changed_or_unavailable`
flags historical awards whose evidence has since been invalidated, withdrawn,
removed or is no longer trusted under the current policy.

Badges attest dated achievements, not permanent memory or an external
qualification. Leaving preserves them; a new enrollment/version earns its own.
Learner erasure removes awards and their evidence references. No retroactive
bulk backfill runs during migration; the next observation or adjudication
evaluates eligible stored evidence. Retention evaluation examines a bounded
suffix of 500 observations; truncation can delay an award but never invent one.
See the [phase 5 acceptance contract](institution-phase-5.md).

### Collective statistics

Staff open **Collective statistics** from a cohort in `/console/progress`, or call
`get_cohort_statistics` (`progress:read`, the same live cohort assignments as the
other progress reads). The figures are anonymous aggregates of one cohort's
published version: learners with recorded reviews, active or completed learners,
learners who completed the formation, the mean and median of each learner's
average mastery estimate, learners holding each badge kind/milestone, and per
concept the observed learners with mean and median mastery.

A background worker recomputes every published institutional cohort about once
an hour and stores only the latest snapshot; nothing is computed while you read.
Responses carry `computed_at`, `status` (`available`, `insufficient_data`,
`not_yet_computed`) and `stale` (older than six hours, for example when the
worker is stopped). Activity, departures and erasures appear at the next run.

Privacy rules are enforced when the worker writes, not when you read:

- a cohort with fewer than five learners with recorded reviews stores no figure
  at all; a concept needs five observed learners of its own;
- a holder count (completion, each badge) is withheld when the holders or the
  non-holders are a group of one to four, so a hidden small group cannot be
  recovered by subtraction. A withheld cell is `null` with
  `status: insufficient_data`, never zero; a real zero is reported as `0`;
- only active and completed enrollments count, and estimates written after an
  enrollment migrated are excluded, as in the individual views;
- no learner identifier, answer, chat or profile data is stored.

Statistics describe the cohort, not individuals, and mastery remains an estimate,
not a grade or certification. The client AI writes any synthesis from the returned
figures (see `synthesis_guidance`). With PostgreSQL roles, re-apply
`deploy/postgres-roles.sql` after upgrading so the worker can write the three
`cohort_*_statistics` tables. See the
[phase 6 acceptance contract](institution-phase-6.md).

### Collective weights

For every concept of a published institutional formation version with at least
thirty learners who each have five recorded reviews of it, a worker publishes
(every six hours) collective BKT parameters: the median of the contributors'
learn, forget, slip and guess parameters, clamped to 0.01–0.5. A new version is
published only when it differs by at least 0.01 and never moves a parameter by
more than 0.02, so the collective stays stable. Versions are immutable, keep
their contributor count and policy version, and contain no learner identifier.
This is a robust consensus of already individualized parameters, not a
maximum-likelihood fit. The current weight of each concept appears in the
statistics view and in `get_cohort_statistics` (`collective_weight`).

The worker never writes learner state. A weight reaches a learner inside that
learner's own interaction, at most once per weight version, and only for active
enrollments in published institutional formations: a learner without reviews of
the concept takes it as their prior; a learner with reviews moves 25% of the way
toward it, by at most 0.05 per parameter. Mastery estimates, review schedules and
review counts are never changed. Each application is recorded in the interaction
(`bkt_collective_weights`, with the parameters before and after) and in a per-learner
ledger removed by learner erasure. See the
[phase 7 acceptance contract](institution-phase-7.md).

## Backups, restore and upgrades

Local/hobby backups must include the database **and** `keys.json`. For a native
installation, stop all clients or stop `tutor-hobby`, then copy the entire
profile directory, preserving private ownership and modes, to a private backup.
Include any WAL/SHM files. Restart only after the copy finishes.

For Compose, stop `tutor` and back up its `tutor_data` volume using your volume
backup tool. Also preserve Caddy's `caddy_data` and `caddy_config` volumes for
certificate continuity. Keep the Compose project name stable when restoring so
the restored named volumes are used. Do not copy only an open SQLite main file.

Restore into an empty private directory or volume, with the original database
and original keys together. Native files belong to `tutor`; container files
belong to UID/GID 65532. Restart with the same profile and data path. Verify
`/ready`, account login and a known learner domain. A missing key fails startup;
never generate a replacement key for an existing encrypted database.

Institutional backups use the existing PostgreSQL backup/restore scripts in
`deploy/`, with separate recoverable signing/encryption key backups. Exercise
restoration in an isolated environment. Before upgrading, keep a matching
binary and backup: schema rollback is not automatic.

## Release validation

Protocol fixtures exercise Hermes CIMD/DCR and Claude/ChatGPT CIMD. Native CI
checks local stdio and private permissions on the three operating systems.
Neither is a claim that a live Claude or ChatGPT account has been tested.
Record actual client acceptance separately before calling that compatibility
verified. Local and VPS installations remain independent; no automatic data
synchronization or conversion is provided.
