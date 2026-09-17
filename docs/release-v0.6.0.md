# TUTOR MCP v0.6.0

Local stdio now starts with `tutor-mcp --local`: one persistent learner,
encrypted SQLite memory and no account, email service or HTTP listener.
Multiple local clients share that profile without gaining administrator rights.

The hobby VPS profile adds username/password accounts managed through SSH
invitations and reset links. Remote MCP access still uses OAuth, PKCE and
explicit consent. Institutional deployments retain verified email, PostgreSQL,
separate process roles and the existing production security requirements.

- Fix #172: Hermes' CIMD document with ten loopback redirects is accepted.
  CIMD supports up to 32 redirect URIs; DCR retains its limit of five.
  Negotiate the plural authentication-method field, including public `none`
  clients when the singular preference is `private_key_jwt`.
- Fix #174: split filesystem protection into Unix and Windows implementations,
  enforce Windows ownership/ACLs, and add Windows builds and native tests.
- Add Linux/macOS/Windows amd64/arm64 archives and SHA-256 checksums, a macOS
  shell installer and a per-user PowerShell installer.
- Add native systemd/Caddy and non-root Compose installation templates.

See the [installation guide](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/main/docs/installation.md)
for local clients, VPS setup, backups and restoration.

Existing startup without new flags remains available. New profiles use
dedicated data directories; there is no automatic conversion or synchronization.
Back up the database and keys together before upgrading. Institutional
operators must run migrations and reapply `deploy/postgres-roles.sql`.

Automated protocol fixtures cover Hermes, Claude and ChatGPT authentication.
Live Claude and ChatGPT acceptance has not been verified; these fixtures do
not certify compatibility with the hosted products. This release adds no new SSO.
