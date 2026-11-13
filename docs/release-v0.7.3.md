# Tutor ACP 0.0.4 · Tutor MCP v0.7.3

This release adds visual activities in the conversation and a personal review
schedule, building on the existing pedagogical engine.

## ACP Client 0.0.4

- Typing `/` opens the choice between **Quiz** and **Flashcards**. The `/quiz` and
  `/flashcards` commands also accept a topic after their name.
- Quizzes offer clickable answers, a submit step and then an explained
  correction. The attempt and its rubric are prepared before the answer.
- Flashcards flip on click and collect a self-assessment:
  **"Pas retrouvé" (Did not recall)**, **"Retrouvé avec effort" (Recalled with effort)**,
  **"Retrouvé facilement" (Recalled easily)**. This self-report remains distinct
  from a verified answer in a quiz.
- **"Planning" (Schedule)**, in the sidebar, lets you enable reviews per course,
  choose their format and view the due dates computed by Tutor-MCP.
  **"Réviser maintenant" (Review now)** opens the chat and confirms the right course before
  starting the activity.
- Preferences are kept per account, connection, institution and course.
  Disabling a course in the schedule does not disable the server's pedagogical
  tracking.
- Answers are kept after closing and protected against double submission.
  Offline requests remain **"À envoyer" (Ready to send)**. Withdrawing a pending answer
  keeps a local receipt, with no server record and no new attempt.
- Invalid or interrupted activities do not reveal their correction.
  The question stays visible when cards open in small windows; mouse, keyboard,
  French/English and light/dark themes are supported.

## Windows installation

Download **`Tutor_ACP_0.0.4_windows_x64-setup.exe`** and install it over
the previous version. The `fr.aguiovanna.tutor` identifier and the existing
accounts, keys, conversations and settings are preserved. The Codex and Claude
adapters and their runtimes remain included.

## Distribution and verification

The six Tutor-MCP v0.7.3 server archives and their notices are attached to
keep the usual downloads of the **Latest** release. No server code or
migration changes since v0.7.2.

The required checks run on `staging` before that same commit is promoted to
`main`. The Windows workflow builds, installs and starts the client, and
verifies its native interface and the ACP adapters. The attached reports
identify the commit and the workflows that produced the files.

The visual qualification of the new modalities uses test data and covers both
themes, the keyboard and narrow widths. These checks are neither a tutoring
session with an authenticated AI provider nor an experimental validation of
learning gains.

`SHA256SUMS` covers every asset except itself. Usage details are available in
the [client guide](https://github.com/ArnaudGuiovanna/tutor-mcp/blob/v0.7.3/client/README.md).
