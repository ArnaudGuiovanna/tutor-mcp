# Tutor — desktop client

Tauri application for learning with Codex or Claude through the Agent Client Protocol (ACP).
The client is developed in this folder of the Tutor repository; the teaching server
remains at the root. The reference target is **Windows x64**. An agent can run
natively or in an explicitly chosen **WSL2** distribution.

## ACP client 0.0.14

With Tutor-MCP v0.7.6, the client only offers the Codex and Claude harnesses;
experimental local inference has been removed. Local permissions apply per
operation (`manage_domain.delete`, for example) and "Toujours autoriser/refuser"
(Always allow/deny) remembers the operation concerned. The resume prepared before
generation, introduced in 0.0.13, is retained.

Download the [Windows x64 installer](https://github.com/ArnaudGuiovanna/tutor-mcp/releases/download/v0.7.6/Tutor_ACP_0.0.14_windows_x64-setup.exe).
This client requires a v0.7.6 server (grouped MCP tools). See the
[release notes](../docs/release-v0.7.6.md).

This version includes the workspace with an icon rail, a collapsible sidebar,
a lighter home screen, a fixed composer and quiz/flashcard cards. Existing themes,
accounts, courses and conversations are preserved. Codex/Claude connections
are reused automatically; the harness remains responsible for its
authentication and billing. A provider refusal is no longer presented as proof
that the subscription is exhausted.

The **+** button attaches UTF-8 text/code files (five files, 32 KB each,
64 KB including the message). **Partager** (Share) copies or exports the
conversation as Markdown; attachments appear in it by name. Titles are reworded
on the first exchange, without an additional generation request. See the
[release notes](docs/release-0.0.10.md).

## Available flows

The learning flows below use the Codex or Claude harnesses connected to the
Tutor tools. The client does not run any model locally.

- Independent connections for the harness and Tutor, with process status,
  authentication status reported by the harness, and separate request incidents.
- Detection of the Codex and Claude adapters on Windows and in WSL2; manual path.
- Conversation home: writing what you want to learn creates a private personal program and lets you start immediately.
- Course design and tutoring with the server's real MCP tools, courses and
  catalog, progressive conversation, explicit permissions and interruption.
- Exchanges are kept on the device, and the agent session is resumed with
  the same Tutor account. Interrupted responses remain marked as such.
- 0.0.4 interface following the [Tutor-MCP brand guidelines](../ui/charte-tutor-mcp.html):
  midnight blue, blue accent, cursor wordmark and embedded Inter/JetBrains Mono fonts.
- Window without a native title bar, custom buttons, reworked spacing and readability.
- **Connexion et outils** (Connection and tools) brings together Tutor, identity, the agent and MCP permissions.
- **Ctrl+K** opens the palette: accent-insensitive search, conversations, courses,
  account, settings and themes. Arrow keys and Enter choose a command;
  Escape closes the palette and Tab stays within the dialog.

Programs are saved in Tutor from the first exchange. The Formations (Courses)
library comes from a paginated, searchable server projection. Enrollments, permissions and observations
remain controlled by the server. Estimates do not constitute proof
of mastery. The client does not create nonexistent votes or community publications.

### Quiz, flashcards and Planning (0.0.4)

In the input field, `/` opens the choice of modalities. `/quiz` prepares a question
with clickable answers and correction after submission; `/flashcards` prepares
a card to flip, followed by the choices **Pas retrouvé** (Not recalled), **Retrouvé
avec effort** (Recalled with effort) and **Retrouvé facilement** (Recalled easily).
A topic can follow the command. The modalities use the active course; without a
course, the tutor asks for or explicitly restates the topic before preparing the activity.

The **Planning** destination displays the due dates already computed by Tutor-MCP.
Enable a course and choose its format; **Réviser maintenant** (Review now) opens
the chat, confirms the course on the server side and starts its review directly.
Preferences are stored on the device per account, connection, institution
and course. Disabling a course in this planning view does not disable
learning tracking or the server's spaced repetition algorithm.

Answers and their contexts remain in the history. Offline requests
remain **À envoyer** (To send). Withdrawing a pending answer keeps a local
receipt, without sending it to the tutor, to prevent answering a card
that has already been revealed again. A flashcard self-assessment remains distinct from an
observed answer; due dates and learning evidence remain under the control
of the server. No change to the MCP server is required.

The presentation contract uses `tutor-activity`, `tutor-study-request` and
`tutor-response` JSON blocks, validated by the client. They are never
executed as HTML or JavaScript. Quizzes keep the attempt,
the activity type and the rubric prepared before the answer so that they can be
resumed without losing the assessment criteria. An interrupted or
invalid activity is hidden and does not reveal its correction.

## Windows and Codex installation

ACP client **0.0.4** is distributed as a release asset with a
[durable link](https://github.com/ArnaudGuiovanna/tutor-mcp/releases/download/v0.7.3/Tutor_ACP_0.0.4_windows_x64-setup.exe).
The **Client → Client (Windows)** workflow builds NSIS, checks the bundled
agents, installs the application and tests its window as well as its
UI Automation tree and keyboard input. The report is kept with the CI artifact.

Settings group together Connexion et outils (Connection and tools), Institution,
Membres (Members) and Ergonomie (Reading and appearance). Tutor, the required identity, the agent
and permissions are configured in the first tab, without going back and forth between
several pages. The Français/English language, themes and reading settings are stored per account.
Per-tool MCP permissions are tied to the selected server and agent;
they can apply to all conversations and be revoked later.
The [release backlog](../docs/backlog-tutor-experience.yaml) provides the
validation criteria and evidence for each batch.
The window without a native title bar, the window controls, spacing and
interactions are described in the [client experience notes](docs/desktop-experience.md).
A Linux build does not qualify Windows. Development installers
are not signed with a commercial certificate.

**Codex Desktop is not required.** The launched process is the
`@agentclientprotocol/codex-acp` adapter, which bundles a compatible Codex dependency.
A detected Codex installation is informational: automatic selection
uses this bundled runtime. In manual configuration, filling in **Chemin de Codex**
(Codex path) explicitly selects that CLI via `CODEX_PATH`, after checking its access and
`--version`. On Windows, select the native `codex.exe` binary, not
an npm `.cmd` wrapper. On WSL, use an absolute Linux path to the executable
CLI. Leaving this field empty keeps the runtime provided by the adapter.
Negotiation and the agent's real operations then verify the
compatibility of the chosen pair.

The installer includes **Codex ACP 2.2.2**, **Claude ACP 0.95.0**, their
compatible runtimes and **Node.js 24.13.0**. No separate npm/Node installation
is needed for these two agents. Choosing the agent saves the choice and
launches it; it is restored at the next startup. Its existing connection is
reused without choosing a billing mode in Tutor. The session folder
is prepared automatically. An
existing WSL2 installation remains accessible in the advanced settings.

If the harness reports that sign-in is required, the connection button
opens its official method as announced by ACP. On Windows, Claude's interactive
methods use a separate window, with the same adapter,
the same distribution and the same user for WSL. Tutor does not collect
Claude credentials. The system dependencies
required by the provider (notably Git Bash for Claude's shell usage
on Windows) remain those of its CLI. Claude's built-in file/terminal tools
are disabled in learning sessions; its MCP tools remain
subject to Tutor permissions. Codex's optional native voice module is
excluded from this text client. The retained executables remain unchanged.

To rebuild the resources from the repository:

```powershell
python client/scripts/prepare-agents.py
```

The script uses the locked npm archives and the official Node ZIP verified
by SHA256. Generated resources are ignored by Git. The workflow tests
both installed agents without an external Node in PATH.

For WSL2, install the adapter **in the relevant Linux distribution**, then
rerun detection. Launching keeps the same Linux user, finds
its Codex or Claude environment, and goes through `wsl.exe`. Bash and `setsid` (`util-linux`)
are required. `~/...`, absolute Linux and Windows paths are resolved in
the distribution before the ACP session is created. Cleanup terminates the
process group of this session, without stopping the distribution.

Connecting to the AI provider follows the methods announced by the adapter.
The provider account or key remains in the environment of the selected harness.
Windows and WSL environments do not automatically share their access.
The provider connection and any associated cost remain separate from the Tutor service.

The harness can be launched before or after connecting to Tutor. A change
in MCP rights invalidates old sessions and restores the process; the
next session receives the updated access. Changing agents does not disconnect
the remote Tutor identity. An unintended disconnection of the process triggers
at most three launch attempts in the interface, with progressive backoff.
An explicit disconnection stays stopped until a new user action.
No failed prompt is replayed automatically.

Request refusals keep the ACP process as long as its transport and
persistence remain healthy. The `limit` category alone does not prove an exhausted
plan. Messages distinguish temporary limits, session limits,
sign-in requirements and unknown causes. "Dernier incident de
requête" (Last request incident) keeps the ACP source and the filtered diagnostic; raw titles and details
that may contain secrets are not displayed.

The audit compared the preparation/authentication separation and the handling
of application errors with [Block's Buzz](https://github.com/block/buzz/tree/d342cf141cb3211c8e5a6c74f881cf1d39ad3b12).
Handling of interactive methods follows the [ACP authentication protocol](https://agentclientprotocol.com/protocol/v1/authentication).
Local tests exercise ACP processes, both connection orders,
renewed MCP credentials and retries without prompt repetition.
They do not replace an interactive trial with real accounts on Windows/WSL.

## Connecting to Tutor

**Local**: specify the Tutor binary and the `--local` arguments. The binary must
be usable in the same environment as the agent: a Linux path for WSL,
a Windows path for a native agent. To choose a profile, add
`--data-dir CHEMIN_ABSOLU`. Several Tutor processes share this durable profile.

**Institution**: create a local identity (or import a Nostr `nsec` key),
save its encrypted backup to a safe medium. Once the file has been written
and read back, confirmation is automatic and the badge changes to **Sauvegardée** (Backed up).
Keys remain the same after a restart before backup; the recovery
key is then temporarily protected in the DPAPI vault. If an older
client closed an unconfirmed identity without keeping this key, check the
already exported file; the application never silently replaces your keys.
A backup of another identity is rejected.
The Tutor connection button shows the remaining prerequisite when it is greyed out.
The identity uses secp256k1 and BIP340 signatures, like Buzz. The application
displays only the `npub` public keys of the identity and of its recovery key.
The administrator authorizes these two public keys in the institution; the client
does not create an e-mail account and does not ask for an account password.

Enter the institution's HTTPS origin, without `/mcp`, then check its name and
origin. The client discovers `/.well-known/tutor-institution` and signs a unique
NIP-98 challenge valid for 120 seconds. The private key stays in Rust, protected for
the Windows user by DPAPI. The Unix development variant uses private
`0600` files, without claiming to encrypt them on the machine.

The portable backup is a `tutor-identity-nip49` version 1 envelope with
**two keys encrypted separately in the standard NIP-49 `ncryptsec` format**. Scrypt
(logN 16, 64 MiB), NFKC normalization and XChaCha20-Poly1305 are implemented by
the Nostr library. The passphrase encrypts the backup and is not used to
sign in. Keep the file offline and its passphrase separately. The recovery private
key does not remain in the active vault after confirmation; its
public counterpart lets the institution verify a future recovery.
Before this confirmation, no institutional session can be opened.

The server returns a revocable 30-minute session. No permanent refresh
token is kept: the application signs a new challenge as
expiry approaches. Only the temporary access token of the MCP session is
delegated to the agent. The private key, the backup and its passphrase never pass into
its parameters, its environment or its prompts. Disconnecting revokes the
remote session and removes its local access; revocation errors are
reported. Old OAuth vaults are kept without being reused.

The **Mon compte et mes rôles** (My account and roles) menu shows the local identity, the institution and
the roles returned by the server. Create or select several local accounts
on the same client; each has its own vault, conversations and working
folder. Switching terminates the agent and revokes Tutor access for the previous
account. An already connected account reconnects to the institution with a new signature
when it is selected. The legacy account remains in its original folder.

The two main destinations are **Conversation** and **Formations** (Courses).
The account's last conversation is resumed on opening. Its history
stays vertical in the sidebar; a new conversation is always accessible.
The account, identity, agents and institution have their own pages.
Former intentions are kept in the account's historical notes.

Writing "Je veux apprendre le Go" (I want to learn Go) first searches for an existing program,
then saves a private personal program and its concepts if needed.
No intention form is required. An owner or trainer who
starts learning gets their learner space without changing their team rights.
Useful session permissions are requested with the same identity.
An incomplete configuration keeps the message **À envoyer** (To send) on the device;
the connection completes in this window, then the message resumes exactly once.

Each conversation has a server context with its active course.
Changes are explicit and turns keep their learning reference.
A different domain is refused until it has been selected in this
conversation. Institution or account changes do not replay any exchange.

The course sheet replaces the central area and offers **Programme** (Program),
**Progression** (Progress), **Partage et accès** (Sharing and access). Manual editing and **Modifier avec l'agent**
(Edit with the agent) use the same editor and the same server preview. A conflict keeps
the text to review; it does not silently replace the recent version.
**Valider les modifications** (Validate changes), **Soumettre au propriétaire** (Submit to owner) and **Publier dans
l'institution** (Publish in the institution) are distinct actions.

Sharing a personal program creates a copy of its design. Private exchanges
and progress remain separate. Contributions require
owner validation at each revision, unless open publication is explicitly
allowed for that course. **Approuver et publier** (Approve and publish) is atomic.
Named offers remain hidden from non-invitees. "Tous les membres" (All members)
refers only to members of the institution. Published shared versions
are immutable and enrollees keep their version until an explicit migration.

Conversation text is 20 px, with a line height of 1.65 and a size adjustable
from 16 to 24 px in the account. Code wraps and copying it preserves
the original characters. Tables that are too wide become vertical.
Settings and courses use the same dark surface. See the
[contract and reference screenshots](../docs/tutor-conversation-formations.md).

Rotation requires approval from the current recovery key, read
temporarily from the offline backup. A stolen active key therefore cannot
replace recovery on its own. Rotation creates a new key pair and its backup file before
revoking the old pair in the selected institution. Recovery signs
with the old offline key and installs a fresh pair. Keep both
backups until reconnection succeeds; a network error after sending may
make the remote outcome uncertain. A local write failure does not erase
the old vault. A confirmed recovery requires a new sign-in.

### Identity from the terminal

The Rust binary `tutor-client-identity` can be used without a graphical interface.
Secrets are read **from stdin**, never from arguments or outputs.
Use a secrets manager or a private terminal (raw stdin input
may be visible in a terminal). Example, with the passphrase supplied by your secrets
tool:

```bash
cargo run --manifest-path client/src-tauri/Cargo.toml --no-default-features \
  --bin tutor-client-identity -- identity generate \
  --directory /chemin/prive/coffre --file /support-hors-ligne/identite.json
```

Subcommands: `generate` creates the key pair and can export/confirm with `--file`;
`exportpub` requires a confirmed backup; `backup --file FICHIER_NEUF` completes
an unsaved creation; `import --file FICHIER` restores a backup
(passphrase on stdin); `import --nsec` imports the private key read from stdin and then
requires a backup. A different identity never implicitly replaces
an existing vault. Each command displays only the status and the public keys.

Intentions and conversations are saved in `workspace.json`, in
the Tauri user data folder (`fr.aguiovanna.tutor`). They may
contain personal information. The vault file is separate. A
corruption is reported and kept for recovery; it is not erased
by an automatic reset.

To back up the local workspace, close Tutor and then copy this folder to a
private location. The DPAPI vault remains tied to the Windows user who
created it: copying the folder to another account does not automatically transfer
access. The server's learning data follows its own backup
and restore procedures.

## Development

Install Node.js 24, Rust 1.93 and the
[Tauri prerequisites for Windows](https://v2.tauri.app/start/prerequisites/), including
the MSVC C++ tools and WebView2. From this folder:

```powershell
npm ci
npm run tauri -- dev
```

To build the installer, prepare the agent resources and then generate the notices according to the
[distribution guide](docs/distribution-notices.md),
then run `npm run tauri -- build --bundles nsis`.

Native Rust commands handle the connection, processes and persistence.
The React/TypeScript interface calls only the Tauri bridge. The preview
browser displays the interface but does not launch an agent or a fake service.

From the repository root, the shared verification is:

```bash
bash scripts/verify-client.sh --install --real-adapter
```

It includes interaction tests, the web build, Rustfmt, Clippy, the native core
tests and a negotiation with the real adapter without any model call. Protocol
fixtures remain in `tests/` and are not packaged with the UI.
The [runtime qualification](docs/runtime-qualification.md) distinguishes native
and WSL trials from real model calls; synthetic tests do not on their own
qualify real tutoring.

The server's `local`, `hobby` and `institution` profiles remain available.
A connection change requires an explicit resume; a session from one account
cannot be replayed in another account or another institution.
