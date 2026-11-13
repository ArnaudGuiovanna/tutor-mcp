# Tutor Desktop 0.3.0

Tutor now has a "Compte" (Account) screen and "Paramètres" (Settings) with four tabs:
"Agents", "Institution", "Membres" (Members) and "Ergonomie" (Reading and appearance). The Français/English choice changes
the interface and the language of upcoming replies, without translating conversations
or curricula already saved. Light, dark and system themes, text size, line spacing
and reduced motion are kept per account.

The built-in agents use the same qualified Rust launch path as the application.
Windows Node paths are fixed and checks of an unchanged package are cached.
Streaming writes a recoverable incremental log instead of rewriting the whole
history on every chunk. The tutor's activity is shown as text in the conversation,
following the actual events.

In "Agents", MCP permissions can be kept across conversations and restarts:
allow, ask or deny per tool, or allow all Tutor tools. The list comes from the
connected server. These choices stay tied to the account, the institution and
the agent; they grant no additional role. A denial is also enforced server-side.

"Compte" explains the public identity, local signing and the recovery pair,
with an encrypted backup. It lets you remove an account from the device or
separately request erasure of institutional data. Members, invitations, roles
and ownership transfers use the actual rights. These actions require a locally
signed proof, separate from the MCP access alone entrusted to the agent. The last
owner remains protected.

Personal courses can be archived, restored or deleted. Drafts and publications
have suitable actions; withdrawing a publication keeps the versions and progress
of learners already enrolled. The MCP selection contract now specifies which
fields are actually required, so an existing course can be resumed without
creating a duplicate.

The server fixes PostgreSQL cleanup with the restricted worker role and exposes
usable MCP durations and results without logging private content.

Install `Tutor_0.3.0_x64-setup.exe` over the previous version. Existing keys,
accounts, conversations and progress are preserved; no re-import or key
regeneration is needed for the update.

The [backlog with acceptance criteria](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/tutor-client-v0.3.0/docs/backlog-tutor-experience.yaml) distinguishes
the changes, their evidence and the distribution acceptance test. Qualification
reports and SHA-256 checksums accompany the release. An ACP negotiation or a
UI Automation audit does not by itself constitute a session with a model, nor
a full manual Narrator audit.
