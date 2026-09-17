# Local, hobby and institution profiles

All three profiles share the teaching engine, learner permissions, quotas,
transactions and idempotency protections.

| Use case | Command | Identity | Storage |
|---|---|---|---|
| Individual, on their own machine | `tutor-mcp --local` | Automatic profile, no password | SQLite |
| Personal or small-group VPS | `tutor-mcp --profile hobby` | Username and password, SSH invitations | SQLite, one HTTP server |
| Institution | `tutor-mcp --profile institution` | Accounts with verified email | PostgreSQL, separate API/worker/migrator roles |

Without these options, the existing environment-based configuration remains
available. `--help` explains the options. Profiles are never converted or
synchronized automatically. Each database records its installation type.

## Local mode

Stdio configuration:

```json
{
  "mcpServers": {
    "tutor": { "command": "tutor-mcp", "args": ["--local"] }
  }
}
```

The MCP client starts the process. No HTTP server, JWT configuration or SMTP
service is required. `stdout` is reserved for MCP; logs go to `stderr`.
Closing MCP input ends the session and drains background tasks before closing
the database. Background tasks run while at least one client keeps TUTOR active.

The default directory is `~/.tutor-mcp/local`, independent of the current
working directory and server connection environment variables. To move it,
add `"--data-dir", "/private/path/tutor"` to the arguments. Multiple clients
can open the same profile concurrently: they share an identity with only the
`learner` role. Scheduled tasks use durable SQLite leases.

Narrative memory is encrypted and versioned in SQLite. `keys.json` contains
the installation's private key material. It is generated automatically before
the database, under an operating-system lock. An existing installation refuses
to start if its key is missing or invalid. Restore the original key instead of
generating a replacement. Permissions are restricted to the owner: 0700/0600
on Unix, and owner checks with private inherited ACLs on Windows.

## Hobby accounts

Initialize using the same operating-system user and data directory as the
HTTP service:

```sh
tutor-mcp init --profile hobby --public-url https://tutor.example.org
tutor-mcp --profile hobby
```

The default directory is `~/.tutor-mcp/hobby`; `--data-dir` overrides it.
`init` creates the configuration, encryption key and Ed25519 signing key,
then prints an account invitation valid for 24 hours. The recipient chooses
a username of 3–64 ASCII characters (letters, digits, `.`, `_`, `-`,
case-insensitive) and a password of 12–72 bytes.

Administration uses SSH:

```sh
tutor-mcp users invite
tutor-mcp users list
tutor-mcp users reset alice
tutor-mcp users disable alice
```

Add `--data-dir /path/to/service/data` to each command when needed.
A password reset link is valid for 15 minutes. Link secrets are never stored
in plaintext in the database. GET displays a CSRF-protected form; only a valid
POST consumes the link. Creating an account does not grant OAuth consent.
Resetting or disabling an account invalidates existing authorizations,
including access token versions checked on every request. Disabling an account
also clears its notification destination.

The profile sends no email. Failed attempts are limited by IP, tracked per
account and subject to the bcrypt budget. Failures caused by a third party
cannot force an email challenge or permanently block a correct password.

The server listens on `127.0.0.1:3000` by default. An HTTPS reverse proxy must
expose `https://tutor.example.org/mcp`. Use `LISTEN_ADDR` for a container network;
do not publish TUTOR's port directly. Trusted proxy CIDRs are explicit, with
loopback defaults for native installations. A lock limits each hobby
installation to one HTTP server. SSH commands remain available while it runs.

## OAuth and institutional deployments

For a remote client, paste the HTTPS URL, sign in, then authorize the displayed
client. HTTPS does not replace OAuth. Discovery, PKCE S256, consent, audience
validation and refresh token rotation remain active. CIMD identifies clients
without manually copying a client secret; bounded DCR remains available in hobby.

CIMD accepts up to 32 redirect URIs; DCR retains its limit of five. URL, size
and network restrictions still apply. The plural CIMD authentication-methods
field takes precedence: `none` is accepted when it is in the supported
intersection, even if the singular field prefers `private_key_jwt`. Documents
containing only the legacy field remain supported. See the
[OpenAI documentation](https://developers.openai.com/plugins/build/auth#client-registration)
and [Claude documentation](https://claude.com/docs/connectors/building/authentication).

`--profile institution` applies all existing production validation: PostgreSQL,
separate process roles, verified email and SMTP, asymmetric keys, encrypted
memory storage, isolation, audit and strict proxy configuration. The `token`
and `disabled` DCR policies remain available. Reapply
`deploy/postgres-roles.sql` after migration, including the worker's permission
to read the installation marker.

Automated fixtures cover Hermes/DCR and Claude/ChatGPT/CIMD. They do not replace
acceptance tests in the actual products. Compatibility must only be described
as verified after those tests.

## Backup and restore

For local and hobby profiles, stop every process using the profile, then copy
the entire directory, including `runtime.db`, any WAL/SHM files and
`keys.json`, to a private destination. Restore the database and keys together,
under the service's operating-system owner, then restart with the same profile.
Do not copy only the main file of an open WAL database. Backups contain secrets
and must remain private. Institutional deployments retain their PostgreSQL
procedures and separate key-manager backups.
