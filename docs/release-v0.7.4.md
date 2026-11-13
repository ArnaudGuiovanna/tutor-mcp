# Tutor ACP 0.0.12 · Tutor-MCP v0.7.4

This release brings together the Windows client and the server, which prepare
the pedagogical context before generation by Codex or Claude. It becomes the
**Latest** release for the installer and the server archives.

## ACP Client 0.0.12

- Consolidated context: selected course, relevant concepts, difficulties,
  recent observations, active session and the server's decision for an activity.
- ACP sessions are reused and model settings are kept. The harness handles
  authentication and billing; the client imposes no additional API key or
  subscription choice.
- Quiz preparation, answer submission and assessment recording are performed
  by the native client, with server validation before display.
- An interrupted recording is resumed with the same key, without a new
  generation or a duplicate pedagogical observation.
- An already recorded reply stays complete if Stop arrives just afterwards.
- Background memory consolidation, with priority given to the conversation,
  when shared narrative storage and permissions are available.

The workspace, themes, attached text files, sharing and conversation titles
from previous versions are preserved. The local agent remains present with its
earlier experimental scope; it does not use the new prepared flow intended for
the Codex and Claude harnesses.

## Installation and server

Download **`Tutor_ACP_0.0.12_windows_x64-setup.exe`** and install it over
the previous version. The application identifier remains `fr.aguiovanna.tutor`;
profiles, conversations and settings stay in their usual location. The Codex
and Claude adapters and their runtimes are included.

Server **v0.7.4** adds `prepare_pedagogical_turn`, reduces the tool schemas and
keeps authority over decisions, attempts and progress. Consolidation uses
bounded batches, leases and version checks.
The SQLite and PostgreSQL migrations are additive.

The six server archives and their notices are attached. Also update the MCP
server to enable the consolidated context; an older server keeps the client's
legacy flow. Publishing this release does not automatically deploy an
institution's server.

## Verification and limits

The 19 required GitHub checks must pass on the published commit. The Windows
workflow builds, installs and starts the application, and verifies the adapters,
the WSL2 flow, the keyboard, themes and settings persistence.
The attached reports identify the commit, the workflows and the delivered files.
`SHA256SUMS` covers every asset except itself.

Context preparation was measured locally on synthetic data.
The target of **one to five seconds for a complete reply** is not yet
qualified with real models on Windows and WSL. No silent downgrade of the
model or its reasoning level is applied.
