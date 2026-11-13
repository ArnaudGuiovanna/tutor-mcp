# Tutor ACP 0.0.14 · Tutor-MCP v0.7.6

A stabilization release of the pedagogical server and the desktop client. The
MCP surface is tightened, initial placement becomes adaptive, permissions are
expressed per operation, and the client keeps only the Codex and Claude
harnesses.

## MCP server

- The MCP surface goes from 71 to 57 tools. Several related tools are merged
  behind an operation selector (`action` or `mode`), for example
  `manage_domain`, `edit_training`, `manage_training_lifecycle` and
  `prepare_pedagogical_turn`. Clients that called the former names must use
  the merged tools.
- Tool policies accept `<tool>.<operation>` keys, for example
  `manage_domain.delete`. A rule on the bare name applies to every operation;
  an undetermined operation is denied.
- Initial placement picks its questions adaptively and stops once the level is
  established with enough confidence.
- Periodic cleanup of expired codes, tokens and credential routes runs tenant
  by tenant, including suspended or closed tenants. Routes still needed for
  replay detection are kept.
- Invitations and the former per-membership second factor are removed.

## ACP client

- Only the Codex and Claude harnesses are offered. Experimental local
  inference (embedded GGUF model) is removed from the client and its release
  artifacts.
- Local permissions are enforced per operation: an "Ask" rule on
  `manage_domain.delete` is honoured even when `manage_domain` is allowed.
  "Always allow" and "Always deny" remember the specific operation. Settings
  show these per-operation rules.
- Conversation turns are better isolated: events carry their conversation, the
  local MCP relay authenticates before reading a request and enforces
  timeouts, and loopback addresses no longer go through a proxy.

## Upgrading

The expected installer is `Tutor_ACP_0.0.14_windows_x64-setup.exe`. The
client identifier and data locations are unchanged.

The server applies new migrations (SQLite up to 0089, PostgreSQL up to
`postgres_0083`). They drop the invitation and per-membership second-factor
tables: take a backup before upgrading. On PostgreSQL deployments, run the
`PROCESS_ROLE=migrator` process once, then re-apply
[`deploy/postgres-roles.sql`](../deploy/postgres-roles.sql) as the owner before
restarting the worker and the API. Publishing release files does not update
remote servers.

## Distribution

The client targets Windows x64, with a native or WSL harness. The server ships
four archives: Linux and Windows, amd64 and arm64.
