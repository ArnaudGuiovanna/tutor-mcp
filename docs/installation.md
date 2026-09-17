# Install TUTOR MCP

The local and hobby profiles require v0.6.0 or later. Choose local stdio for
one person on their own machine, hobby for one VPS shared by a small group,
or institution for the existing PostgreSQL deployment model. See
[profile behavior and accounts](profiles.md).

**Release availability:** [v0.6.0](https://github.com/ArnaudGuiovanna/tutor-mcp/releases/tag/v0.6.0)
is available with binaries and checksums for all supported platforms. Use the
installers below or the [source quickstart](../README.md#quickstart).

## Linux and macOS

Download the shell installer from the release's source, inspect it, and run it:

```sh
curl -fsSL https://raw.githubusercontent.com/ArnaudGuiovanna/tutor-mcp/main/scripts/install.sh -o install-tutor.sh
TUTOR_MCP_VERSION=v0.6.0 TUTOR_MCP_INSTALL_DIR="$HOME/.local/bin" sh install-tutor.sh
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
.\install.ps1 -Version v0.6.0
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
export TUTOR_VERSION=v0.6.0
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
and an explicit CA file. Run migrations with a dedicated owner login. Then
apply [postgres-roles.sql](../deploy/postgres-roles.sql) as the database owner
and grant each runtime login exactly its corresponding API or worker group.
Runtime logins must not own tables or have SUPERUSER/BYPASSRLS. Reapply grants
after upgrades, including the new installation-marker read permission.

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
