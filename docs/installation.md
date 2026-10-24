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

The installer also verifies and keeps licence texts in `tutor-mcp-notices/`
beside the executable. For the unchanged v0.6.1 archives it downloads the
matching notice supplement and `THIRD_PARTY_NOTICES_SHA256SUMS` from that release;
it checks the supplement against the exact archive, binary and platform. Missing
or mismatched notices stop installation before replacing an existing binary.
See [binary distribution notices](distribution-notices.md).

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

Licence texts are kept under `tutor-mcp-notices` in that installation directory.
The installer verifies the inventory and notice files, using the separately
checked v0.6.1 supplement when needed. It needs neither Python nor Go.

Alternatively, unpack the matching Windows ZIP from the release, verify its
checksum, retain its licence texts and use the full path to `tutor-mcp.exe` in
your client configuration. For v0.6.1, use the updated installer to obtain and
verify the corresponding notice supplement as well.
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

`/ready` and its `/health` alias share one database/schema probe per process.
Success is cached for one second, failure for 250 milliseconds; each probe has
a two-second deadline. With continuous probes, a persistent database or ledger
failure is visible within three seconds, including when a database
driver ignores cancellation. Only one unfinished probe is allowed. `/live`
does not access the database. Administrative catalog and review APIs share the
MCP IP, authenticated-principal rate and concurrency guards.

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

`--profile institution` uses cryptographic identities. Members need no email
address, server password or browser login. The Windows [Tutor client](../client/README.md)
generates a Nostr-compatible secp256k1 identity (`npub`/`nsec`) and a distinct
recovery key locally. Codex receives a short-lived, scoped session token; it
never receives either private key.

Use [institution.env.example](../deploy/institution.env.example) for separate
API, worker and migrator environment files. Supply operator-managed Ed25519
JWT signing keys, integration encryption keys and PostgreSQL credentials. These
server keys are independent of members' identities. SMTP is not required.

PostgreSQL URLs require `sslmode=verify-full` and an explicit CA file. Keep the
existing [operations requirements](../OPERATIONS.md). Run the migrator with a
dedicated owner login, then install runtime permissions in this order:

1. As a superuser, give the owner `CREATEROLE` on first installation, or create
   the `tutor_api`, `tutor_worker` and `tutor_restore` group roles yourself.
2. As the database owner, apply [postgres-roles.sql](../deploy/postgres-roles.sql).
3. As a superuser, apply [postgres-roles-superuser.sql](../deploy/postgres-roles-superuser.sql)
   for logical tenant restore.
4. Create one login per process with exactly one group: for example,
   `CREATE ROLE api_login LOGIN PASSWORD '…' IN ROLE tutor_api;` and
   `CREATE ROLE worker_login LOGIN PASSWORD '…' IN ROLE tutor_worker;`.

Runtime logins must not own tables or have SUPERUSER/BYPASSRLS; startup rejects
those permissions. Reapply the grants after every schema upgrade. The API
re-encrypts integration and narrative secrets with the current key at startup;
the worker only decrypts them.

### Create the institution and its first owner

Registration is operator controlled in this release. On the owner's Windows
machine, open Tutor, create an identity, export its encrypted backup and confirm
that it has been saved. Copy only the two public values shown by the client:
the active `npub` and recovery `npub`. Keep the backup and its passphrase offline.
The operator never needs a private key or backup passphrase.

Build the operator utility with `go build ./cmd/tutor-control-plane`. With the
migrator/owner database credentials in its private environment, create an active
plan, provision the tenant and bind the owner's public keys:

```sh
tutor-control-plane -action=plan-upsert -plan=pilot -name=Pilot -status=active \
    -entitlements='{"active_learners":50,"mcp_calls_month":100000}' \
    -reason='institution pilot' -request-id=PILOT-1
tutor-control-plane -action=provision -slug=acme -name='Acme Academy' \
    -region=eu -plan=pilot -reason='institution pilot' -request-id=PILOT-2
tutor-control-plane -action=bootstrap-owner -tenant=TENANT_ID -name='Owner' \
    -owner-npub=OWNER_NPUB -recovery-npub=RECOVERY_NPUB \
    -reason='owner bootstrap' -request-id=PILOT-3
```

Use the tenant ID returned by `provision`. Set `INSTITUTION_ID=TENANT_ID` and
optionally `INSTITUTION_NAME=Acme Academy` on the API. Set `BASE_URL` to the exact
public HTTPS origin, without `/mcp`. The client discovers the published
institution at `/.well-known/tutor-institution`; enter this origin in Tutor.
An owner has no artificial learner account. Its initial session allows reading
formations. Enable the client's explicit formation-authoring authorization
before asking Codex to create a formation.

Add another member from their own two public keys:

```sh
tutor-control-plane -action=add-key-member -tenant=TENANT_ID -name='Learner' \
    -npub=MEMBER_NPUB -recovery-npub=MEMBER_RECOVERY_NPUB -roles=learner \
    -reason='pilot participant' -request-id=PILOT-4
```

Supported member roles include `learner`, `admin`, `pedagogy_manager`, `trainer`,
`auditor` and `billing_admin`; `owner` is reserved for owner bootstrap. Only a
member with the learner role gets a learner profile. An unknown public key
cannot self-register or claim an owner role.

Existing accounts are never linked by matching an email address. After checking
the exact global user ID, the operator may migrate it with
`-action=bind-key-member -tenant=TENANT_ID -user=EXACT_USER_ID
-owner-npub=ACTIVE_NPUB -recovery-npub=RECOVERY_NPUB`, plus `-reason` and
`-request-id`. This preserves its memberships, roles, learner IDs and learning
history while retiring its former login credentials and sessions. Take a backup
before switching an existing deployment; migration and rollback are not automatic.

### Run the separate processes

For systemd, use separate `tutor-api`, `tutor-worker` and `tutor-migrator` users
and private `/etc/tutor-mcp/{api,worker,migrator}.env` files. Install
[tutor-institution@.service](../deploy/tutor-institution@.service) and
[tutor-migrator.service](../deploy/tutor-migrator.service). Start the migrator
once, check its exit, apply grants, provision the institution and then enable the
API and worker. Keep the migrator out of persistent service startup. The API
binds to loopback by default; expose it through the native Caddy configuration
or the explicitly configured HTTPS proxy. Preserve the public Host header for
signed-request verification and restrict `TRUSTED_PROXY_CIDRS` to that proxy.

For Compose, use private `deploy/{api,worker,migrator}.env` files and
`deploy/secrets/postgres-ca.pem`; these are excluded from Git and Docker context.

```sh
export TUTOR_DOMAIN=tutor.example.org
docker compose -f deploy/compose.institution.yml build
docker compose -f deploy/compose.institution.yml run --rm migrator
# Apply grants and provision the tenant with the owner connection.
docker compose -f deploy/compose.institution.yml up -d tutor worker caddy
```

The Compose proxy has the fixed trusted address `172.30.43.2`. Keep DCR disabled
for the native institution. Its email, password, invitation, signup, OAuth
login and cookie-console routes are not mounted. Historical account storage and
other profiles remain compatible, but cannot authenticate to a native institution.

### Identity backup, rotation and recovery

The active private key is protected by Windows DPAPI in the client vault.
Unix development uses private owner-only files. Portable backups contain two
NIP49 `ncryptsec` values in a versioned JSON file: active and offline recovery
keys, encrypted by the local backup passphrase. The server stores public keys
only. After backup confirmation, the client removes the recovery private key
from its active vault. Store the backup separately from the computer.

Connecting signs a one-use, 120-second challenge using a NIP98 kind-27235 event.
It binds the exact URL, method, body hash, institution, action, requested scopes
and key version. A session lasts at most 30 minutes; the client renews it with a
fresh signature. Every protected request checks current membership and session
revocation. Signing out immediately revokes that session.

Rotation requires the active key, the current offline recovery key and proofs
of possession of both replacement keys. Recovery uses the offline recovery key
and proofs of both replacements. Save the replacement encrypted backup before
submitting either change. A change preserves the account and learning history,
increments its key version and invalidates the old keys, pending challenges and
sessions in all memberships. The client retains the relevant backups when a
network or disk failure leaves the result uncertain. Loss of both the active key
and the recovery backup cannot be repaired with email or a server password.

### Formations, enrollment and tutoring

The native client displays actual authored drafts and published formations.
A staff session initially has `formation:read`. Explicitly granting
`formation:write` lets its agent call `draft_formation`,
`add_formation_concepts`, `get_formation_version` and `publish_formation`.
Review the content before asking the agent to publish it. Mutation tools need
an `idempotency_key`; reuse it only for the identical retried request.

Owners/admins manage all formations; pedagogy managers manage their own or
assigned formations. Roles and delegated scopes are checked separately. The
native `/admin/catalog/` JSON API uses live native bearer sessions with
`formation:read` for reads and `formation:write` for writes. POST mutations also
need `Idempotency-Key`; JSON fields use `snake_case`. The cohort report requires
`progress:read`. Assessment-review and curriculum-review administrative routes
remain closed to native delegation in this lot.

Learners connect with their own keys and receive `learner:read learner:write`.
They can browse `list_available_formations`, inspect `get_learner_formation`,
join with `join_formation` and leave with `leave_formation`. Joining returns a
private `domain_id` for the published curriculum; concepts retain shared public
formation IDs. A learner cannot edit the institution's curriculum.

The enrollment policy is `open`, `invitation` or `approval`. Cohort invitations
and approval decisions target an existing membership, without an email address,
at `/admin/catalog/cohorts/{id}/admissions`. Decisions use `membership_id`,
`decision`, `expected_version` and a retry key. Approval reserves no seat until
the learner confirms enrollment. Leaving releases a seat and archives the domain;
rejoining preserves its history. Publishing a new formation version never
silently migrates an existing enrollment. Archiving blocks new enrollment and
preserves historical evidence. Free domains are disabled by default for new
institutions.

Staff follow-up MCP reads require an explicit `progress:read` grant and current
formation/cohort assignments. Learner scopes and formation-authoring scopes do
not include named learner progress. Badges require eligible assessment evidence;
ordinary practice does not grant certification. Anonymous statistics suppress
small cohorts. Shared BKT parameter estimates require independent eligible
observations; private domains retain fixed reference parameters. See the
[formation](institution-phase-2.md), [enrollment](institution-phase-3.md),
[staff progress](institution-phase-4.md), [badge](institution-phase-5.md),
[statistics](institution-phase-6.md) and [parameter](bkt-parameter-policy.md)
contracts for the underlying data and authorization rules. Their earlier browser
and email journeys describe the legacy interface, which is closed in this profile.

### Erasure and acceptance

Learner erasure remains a bounded, resumable worker job and respects legal holds.
It revokes access first, deletes native challenges and sessions as well as
learning state and historical grants, cancels enrollments and releases seats.
The global public identity can serve other organizations and remains outside a
single tenant's erasure. Append-only audit/curriculum history and legal holds
retain their documented scope. See [erasure operations](saas-runtime-operations.md).

`bash scripts/smoke-institution.sh` uses disposable identities generated by the
real Rust client, verified HTTPS, TLS PostgreSQL and the delivered API/worker
roles. It checks owner bootstrap, explicit authoring, formation publication,
learner tutoring, encrypted backup recovery, old-session rejection and actual
queued worker erasure. It needs Go, Rust 1.93, Python, OpenSSL and PostgreSQL
server binaries; it creates no real member account and requires no SMTP service.

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
