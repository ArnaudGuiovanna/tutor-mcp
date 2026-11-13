# Tutor MCP v0.7.1 · ACP Client 0.0.2

**Lifelong learning. Bringing learning communities to life.**

This delivery brings the new ACP client interface for Windows, aligned with the
brand guidelines provided for Tutor-MCP and tutor-mcp.dev.

## ACP Client 0.0.2

- New visual identity: midnight-blue background `#0C131F`, accent `#3D8BFF`,
  cursor wordmark, more compact navigation and library, new icon.
- Embedded Inter and JetBrains Mono fonts: the client uses them without
  downloading fonts from an external service.
- **Ctrl+K** command palette: accent-insensitive search, resuming a
  conversation, access to courses, the account and settings, theme selection.
  Arrow keys and Enter select; Esc closes; Tab stays within the dialog.
- Dark, light and system themes and reading preferences kept per account.
- Progress gauges adapted to the brand guidelines. The values come from the server
  and estimates remain distinct from evidence of mastery.

The client keeps the `fr.aguiovanna.tutor` identifier. Installing this version
over the existing one preserves the account folders, keys and exchanges.
Identity backups are still required for recovery.

Download: **`Tutor_ACP_0.0.2_windows_x64-setup.exe`**.
The application's product version really is **0.0.2**.
The Codex and Claude adapters and their runtimes remain included.

## Banner and documentation

The README banner uses the colours, wordmark and fonts of the new brand
guidelines. The fonts and their original notices are included in the SVG.
The README's organisation and the existing demo are preserved.
The sources of the new site provided in `ui/site` remain independent of this
client delivery; this release is not a web deployment.

## Server distribution

The six distributions of **Tutor MCP v0.7.1** are attached so that the
**Latest** release also keeps serving the Linux, macOS and Windows installers.
They contain their notices and come with the same notice supplements as
v0.7.0. No new migration has been added since v0.7.0.

`SHA256SUMS` covers every asset except itself. The qualification reports link
the files to the commit and the workflows used.

## Scope of the checks

The Windows reports cover the native launch of the embedded agents, direct MCP
persistence, WSL2, the installed application, the command palette, languages,
themes and reopening the same profile. The UI Automation profile is offline:
this check claims neither a model generation nor a manual Narrator audit. The
actual results and any workflow reruns are kept in the attached reports.
