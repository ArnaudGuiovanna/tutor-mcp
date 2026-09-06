# Repository workflow

The maintainer requests publication at the end of every completed implementation
task: commit/push to `staging`, then fast-forward merge/push to `main`.

- Work on `staging`. Before starting with a clean tracked worktree, fetch `origin`
  and reconcile `staging` with `main` by fast-forward only. Stop on divergence;
  never force-push, discard changes, or manufacture a merge resolution.
- For a completed change, review the diff and identify the exact task-owned
  files. Preserve unrelated edits, untracked notes and credentials. Do not use
  `git add .` or `git add -A` to prepare a publication.
- Run `python3 scripts/finish_task.py prepare --message "type: description" --
  path/to/file ...`, with actual explicit file paths (not directories). This
  records the exact staged tree and the current Codex session when available.
- Run `python3 scripts/finish_task.py publish` before the final response. It runs
  the verification script, commits on `staging`, pushes `staging`, fast-forwards
  `main`, pushes `main`, checks the remote heads and returns to `staging`.
  Request normal execution approval if Git/network/test permissions require it.
- The Codex `Stop` hook is a fallback for an explicitly prepared task in the
  same session, not permission to publish every dirty file on every turn.
- Read-only tasks, incomplete work, questions and explicit "do not publish"
  requests must not prepare a task. A later user opt-out overrides this default;
  cancel a pending intent with `python3 scripts/finish_task.py cancel` (the index
  is left intact). Report blockers instead of bypassing checks or hook trust.
- After a failed publication, inspect its state and use `publish` to resume the
  same commit. Do not create a replacement commit just because a push failed.
- Report the commit and the observed remote/CI state. Publication is not a
  deployment, and local verification is not a claim that GitHub CI has passed.

See [task publication](docs/task-publication.md) for setup and recovery.
