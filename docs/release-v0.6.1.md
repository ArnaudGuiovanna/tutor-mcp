# TUTOR MCP v0.6.1

A patch release in the v0.6 series fixing the tool-input inconsistencies found
by replaying a real tutoring session end to end. Each one let a caller send
something one layer had already accepted and the next layer refused.

- **Session summaries:** writing a summary with `domain_id` now succeeds on the
  encrypted database narrative backend used by local, hobby and institution
  profiles. The tool still checks that the domain matches the durable session;
  the storage key follows the existing learner-scoped session layout.
- **Assessment scores:** `criteria_scores` now accepts objects keyed by criterion
  ID, with either numeric scores or score objects, as well as arrays. Outcome
  calculation uses the same accepted input shapes as rubric validation.
- **Assessment scores, second scoring path:** the bound-evaluation scorer used
  by the runtime kept refusing the object form the rubric layer accepts, so the
  fix above only covered one of the two implementations. Both now agree.
- **Rubric aggregates:** `rubric_json` accepts a redundant `max_total` when it
  matches the criteria it summarises, and still refuses one that contradicts
  them, mirroring how a redundant criterion `max_score` is already handled.
- **Learning events:** `kind` advertises its two accepted values as a schema
  enum instead of naming them in prose a caller can ignore.
- **Session close:** `implementation_intention` accepts a single "when, what"
  sentence in addition to the `{trigger, action}` object, splitting it into the
  two clauses rather than failing schema validation.
- **Interactions:** `notes` is no longer required by the schema while its own
  description calls it optional, and both rubric documents now list their
  accepted fields so the two are not confused for one another.
- **Learner context:** `last_session` and `opening_message` are in English,
  matching every other string the tools return.
- Add regression tests for database-backed summary writes and reads, for
  object-form assessment scoring on both paths, for the rubric aggregate, for
  the sentence form of an implementation intention, and a guard that no tool
  input is required by the schema while its description calls it optional.

There are no new database migrations or configuration changes from v0.6.0.
Back up the database and keys together, replace the binary, and restart the
client or service using the existing data directory.

Release archives cover Linux, macOS and Windows on amd64 and arm64, with
SHA-256 checksums in `SHA256SUMS`. See the
[installation guide](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/v0.6.1/docs/installation.md)
for installation and upgrade instructions.
