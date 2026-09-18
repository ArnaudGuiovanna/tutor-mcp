# TUTOR MCP v0.6.1

A patch release in the v0.6 series fixing two tool-input inconsistencies found
during a local tutoring session.

- **Session summaries:** writing a summary with `domain_id` now succeeds on the
  encrypted database narrative backend used by local, hobby and institution
  profiles. The tool still checks that the domain matches the durable session;
  the storage key follows the existing learner-scoped session layout.
- **Assessment scores:** `criteria_scores` now accepts objects keyed by criterion
  ID, with either numeric scores or score objects, as well as arrays. Outcome
  calculation uses the same accepted input shapes as rubric validation.
- Add regression tests for database-backed summary writes and reads and for
  object-form assessment scoring.

There are no new database migrations or configuration changes from v0.6.0.
Back up the database and keys together, replace the binary, and restart the
client or service using the existing data directory.

Release archives cover Linux, macOS and Windows on amd64 and arm64, with
SHA-256 checksums in `SHA256SUMS`. See the
[installation guide](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/v0.6.1/docs/installation.md)
for installation and upgrade instructions.
