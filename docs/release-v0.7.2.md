# Tutor ACP 0.0.3 · Tutor MCP v0.7.2

This release delivers the polish of the ACP client for Windows: a unified window,
more comfortable reading and a consolidated connection flow.

## ACP Client 0.0.3

- Window without a native title bar, with custom controls to minimise,
  maximise, restore and close. Dragging, double-clicking and resizing
  remain available.
- Harmonised spacing, margins, buttons and fields; reworked hierarchy of headings,
  paragraphs, lists, quotes and tables.
- Code with line wrapping and faithful copying, copy confirmation and tables
  adapted to small windows.
- An input box that grows with the message, examples that set up the conversation,
  subtle transitions and animated activity that follows the agent's events.
  Reduced-motion preferences still take precedence.
- **"Connexion et outils" (Connection and tools)** groups Tutor, the required identity, the AI agent
  and persistent MCP permissions. Established connections collapse into summaries.
- Choosing an agent no longer restores an old Tutor address. Changing the
  general permission rule keeps the exceptions defined per tool.
- Text size previews immediately and is saved when the slider is released or
  after the keyboard action.

Download **`Tutor_ACP_0.0.3_windows_x64-setup.exe`** and install it over
the previous version. The product version really is **0.0.3**.
The `fr.aguiovanna.tutor` identifier and the data locations are preserved:
accounts, keys, conversations and settings stay in place. The Codex and Claude
adapters and their runtimes are included, as in the previous version.

## Distribution and verification

The six Tutor MCP v0.7.2 archives are attached to keep the server downloads of
the **Latest** release. The server code and its migrations are unchanged since
v0.7.1; this delivery concerns the desktop client.

The attached reports identify the commit and the workflows that built the
files. The Windows check actually installs the client, exercises the window
controls, keyboard navigation, languages and themes, then verifies the
settings after closing and reopening. It uses an offline test profile: it is
neither a session with an authenticated model nor a full manual Narrator
audit.

`SHA256SUMS` covers every asset except itself. The dependency notices are
included in the distributions. The interface details are described in the
[client notes](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/v0.7.2/client/docs/desktop-experience.md).
