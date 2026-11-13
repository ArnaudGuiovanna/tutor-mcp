# Tutor MCP v0.7.0 · ACP Client 0.0.1

**Lifelong learning. Growing within learning communities.**

Tutor supports aspirations, projects and career changes through personal
learning paths that can be resumed over time. The ACP client gives communities
a space to build, review and share courses, while everyone keeps their own pace
and progress.

## Tutor MCP 0.7.0

- Personal courses created immediately from the conversation, a unified
  library, search and explicit resumption of the active course.
- Institutional courses, invitations, contributions and version approval.
  Named recipients remain protected and the published versions learners use
  remain immutable.
- Management of curricula, access, enrolments and pedagogical follow-up.
  Authors can share a copy of their design without publishing their
  conversations or their private progress.
- Individual progress, reviews and structured assessments. Collective
  calibration is isolated by institution, version and concept; it requires
  30 contributors each with five eligible observations and excludes
  private history.
- Public identities and recovery, member management and controlled institutional
  erasure. Account actions are signed locally and bound to the session;
  administrative rights are not entrusted to the agent.
- Persistent MCP permissions, PostgreSQL cleanup with a least-privilege worker,
  diagnostics without private content, and abandonment of cancelled connections
  whose read protection cannot be safely reset.

## ACP Client 0.0.1 for Windows x64

The new `Tutor_ACP_0.0.1_windows_x64-setup.exe` asset is the client installer,
with Codex and Claude built in. Its product version really is **0.0.1**.

- Conversation and courses, with a "Compte" (Account) area and "Paramètres" (Settings) organised
  into "Agents", "Institution", "Membres" (Members) and "Ergonomie" (Reading and appearance).
- French or English interface, light/dark/system themes, text size, line spacing
  and reduced motion kept per account.
- Per-tool permissions, model and reasoning settings offered by the agent,
  textual activity and a recoverable reply log.
- Archiving, restoring and deleting according to the server's rights; removing
  a local account is separate from institutional erasure.

This numbering starts distributing the ACP client in the server release. The
technical pre-releases `tutor-client-v0.2.x` and `tutor-client-v0.3.0` remain
available. The application identifier and its data folders are preserved:
install this version over the existing one without regenerating your keys.
Exported backups are still required for recovery.

## Downloads and verification

The six server archives cover Linux, macOS and Windows, on amd64 and arm64.
They contain the binary and its original notices. The same families of notice
assets as in v0.6.1 are also available separately, with their provenance and
SHA-256 checksums. Older assets are not modified.

`SHA256SUMS` verifies every file in this release. The Windows reports document
separately the ACP protocol, the native MCP transport, the embedded agents and
the interactions of the installed application. An ACP negotiation does not
prove a model generation; UI Automation is not a manual Narrator audit. The
first package check remains slower than reconnections within the same
application.

## Server upgrade

Since v0.6.1, this version includes schema and permission changes. Back up the
database and keys together. For an institution, run the migrator role, then
apply `deploy/postgres-roles.sql` as the database owner before restarting the
API and worker. Keep the existing data directories.
No existing course is published automatically.

See the [installation guide](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/v0.7.0/docs/installation.md),
the [ACP client](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/v0.7.0/client/README.md)
and the attached reports for the commands and the scope of the checks.
