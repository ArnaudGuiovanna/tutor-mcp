# Supported distribution platforms

For new releases, the supported scope is:

| Product | Distributed targets | Required qualification |
| --- | --- | --- |
| Tutor server | Linux amd64/arm64 | Cross-builds with notices, native Linux persistence and permissions, SQLite/PostgreSQL, race, security and deployment gates |
| Tutor server, local stdio | Windows amd64/arm64 | Cross-builds with notices, native Windows persistence, permissions and installer |
| Tutor ACP desktop client | Windows amd64 | Windows build, installer, native and WSL harness integration |

The desktop client runs on Windows. A WSL harness is a subprocess environment
of that client; WSL does not imply a Linux desktop release. Codex and Claude
remain responsible for their authentication and billing. Live model latency
qualification is separate from CI handshakes and requires real provider calls.

`scripts/release-platforms.json` defines the server packaging targets. Its
consistency test checks the CI matrix and `.github/required-checks.json` against
that policy. Existing Linux/Windows, PostgreSQL, security, learning-policy and
deployment gates remain required.

macOS was inherited from the server's former distribution matrix, rather than
requested for the Windows client. New releases no longer produce Darwin
archives or claim macOS qualification. Historical releases and their notices
remain unchanged; use the installer from a historical source tag for those
artifacts. Source portability alone is not a supported-distribution claim.

Changing this file or the local check list does not change GitHub branch
protection. Publication must inspect the effective remote requirements and
explicitly reconcile them with the accepted platform policy. An obsolete
required macOS check must not be represented as passed. Resume publication only
after the required checks for the exact candidate commit have completed.
