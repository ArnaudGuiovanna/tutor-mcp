# Tutor ACP 0.0.13 · Tutor-MCP v0.7.5

Resuming a course is prepared by the client before generation: the server
confirms the selection, provides the context and chooses the next activity.
Codex or Claude then writes a single reply, with no intermediate MCP calls for
this resumption.

## Resumption and pedagogical tracking

- The "Reprendre" (Resume) button and direct commands such as "Reprendre la
  formation Python" (Resume the Python course) use the same flow. An ambiguous name returns
  permitted choices; no course is chosen or joined arbitrarily.
- The engine chooses an introduction, an open exercise or session advice.
  Resuming no longer forces a quiz. The quiz and flashcards commands remain
  available.
- An exercise's prompt and rubric are frozen before display. The dedicated
  answer area submits the answer before grading; a conversation message or a
  hint request is not automatically submitted as an answer.
- An introduction is recorded as instruction after display, with no mastery
  gain. Recording retries keep their idempotency.
- Selection and review are checked after generation. A reply that has become
  incompatible stays hidden and cannot be acknowledged.
- Criterion identifiers taken from the curriculum are accepted in the rubric
  without having to rewrite them or restart its preparation.
- A new conversation whose first message is a resumption directly prepares the
  right ACP session, with no creation followed by a reload.

The client requires a server that understands the new preparation for this flow.
With an older server, the request is kept and a message asks for the server to
be updated; the client does not silently select another course.
This optimisation targets the Codex and Claude harnesses. It does not qualify
the experimental local inference.

## Distribution

The client targets Windows x64, with a native or WSL harness. The server ships
four archives: Linux and Windows, amd64 and arm64. New releases no longer
include Darwin archives; the CI matrix and the required checks are aligned with
this scope. Older releases remain unchanged.

The expected installer is `Tutor_ACP_0.0.13_windows_x64-setup.exe`.
The client's identifier and data locations remain unchanged.
The update requires no new database migration.
Publishing the release files does not update remote servers.

## Measurements and limits

After removing the initial reload, real Linux measurements give
19.5 s then 11.9 s for a Codex GPT 6.1 sol resumption with high reasoning,
and 14.9 s then 7.0 s with the `sonnet` / `xhigh` alias reported by Claude. No
model tool call was needed. Two warm corrections took
7.7 s and 6.7 s with Codex, 6.7 s and 5.2 s with Claude. These are native
measurements of a candidate build, without a Windows window, with two samples per scenario.

The target of a complete reply in one to five seconds remains **not met**.
External generation remains variable; no universal maximum delay is
guaranteed. The user's reasoning level and model are not downgraded.
CI tests verify functionality; they do not replace measurements with a real
model up to the reply being displayed in the Windows window.
See the [qualification protocol](qualification/acp-latency.md) for the matrix,
the verification of the effective settings and the local export of rendering
measurements.
