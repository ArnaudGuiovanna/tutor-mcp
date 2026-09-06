#!/usr/bin/env python3
"""Publish an explicitly prepared task: staging commit/push, then main FF/push.

No auto-add, force push, reset, stash, credential or hook-trust bypass. This is
a maintainer workflow, not a sandbox/security boundary against other processes.
"""

import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


class FinishError(Exception):
    pass


def command(args, cwd, timeout=120):
    result = subprocess.run(args, cwd=cwd, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout, check=False)
    if result.returncode:
        raise FinishError(f"{args[0]} {args[1]} failed: {result.stderr.strip() or result.stdout.strip()}")
    return result.stdout.strip()


class Publisher:
    def __init__(self, cwd):
        self.root = Path(command(["git", "rev-parse", "--show-toplevel"], cwd)).resolve()
        self.git_dir = Path(self.git("rev-parse", "--absolute-git-dir"))
        self.intent = self.git_dir / "tutor-task-finish.json"
        self.receipt = self.git_dir / "tutor-task-last.json"

    def git(self, *args):
        return command(["git", *args], self.root)

    @contextlib.contextmanager
    def lock(self):
        # Common-dir lock also serializes linked worktrees using this helper.
        common = Path(self.git("rev-parse", "--path-format=absolute", "--git-common-dir"))
        with (common / "tutor-task-finish.lock").open("a") as handle:
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as exc:
                raise FinishError("another task publication is running") from exc
            yield

    @staticmethod
    def save(path, value):
        # Persist retry state atomically, independently of the worktree/index.
        with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as handle:
            temporary = Path(handle.name)
            json.dump(value, handle, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)

    def load(self):
        with self.intent.open() as handle:
            value = json.load(handle)
        required = {"version", "root", "base", "tree", "message", "files", "origin", "session_id", "commit", "verified"}
        if (not isinstance(value, dict) or set(value) != required
                or value.get("version") != 1 or value.get("root") != str(self.root)):
            raise FinishError("task intent belongs to another repository or version")
        if any(not isinstance(value[key], str) for key in ("base", "tree", "message", "origin", "session_id")):
            raise FinishError("invalid task intent")
        if value["commit"] is not None and not isinstance(value["commit"], str):
            raise FinishError("invalid task commit")
        return value

    def branch(self):
        return self.git("symbolic-ref", "--quiet", "--short", "HEAD")

    def no_operation(self):
        for name in ("MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "BISECT_START"):
            path = Path(self.git("rev-parse", "--git-path", name))
            if not path.is_absolute():
                path = self.root / path
            if path.exists():
                raise FinishError("finish the current Git operation before publishing")
        if self.git("ls-files", "--unmerged"):
            raise FinishError("unmerged index entries")

    def unchanged_worktree(self):
        # Untracked files are deliberately untouched, including personal notes.
        if self.git("diff", "--name-only", "--ignore-submodules=none"):
            raise FinishError("unstaged tracked changes remain; review the task file list")

    def ancestor(self, earlier, later):
        result = subprocess.run(["git", "merge-base", "--is-ancestor", earlier, later],
                                cwd=self.root, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        if result.returncode:
            raise FinishError(f"{earlier} is not an ancestor of {later}; reconcile branches explicitly")

    def remote(self):
        fetch = self.git("remote", "get-url", "--all", "origin").splitlines()
        push = self.git("remote", "get-url", "--push", "--all", "origin").splitlines()
        if len(fetch) != 1 or fetch != push:
            raise FinishError("origin must have one identical fetch/push destination")
        return fetch[0]

    def prepare(self, message, files, session_id):
        if self.intent.exists():
            raise FinishError("a task is already prepared; publish it or cancel its intent first")
        if self.branch() != "staging":
            raise FinishError("prepare tasks on staging, not main or a detached HEAD")
        self.no_operation()
        if self.git("diff", "--cached", "--name-only"):
            raise FinishError("the index is already staged; refusing to include another task's changes")
        if not message.strip() or len(message) > 500 or not files:
            raise FinishError("a commit message and explicit task files are required")
        if len(set(files)) != len(files):
            raise FinishError("duplicate task files")
        for name in files:
            path = Path(name)
            if path.is_absolute() or ".." in path.parts or ".git" in path.parts or not path.parts:
                raise FinishError("task paths must be explicit repository-relative files")
            if (self.root / path).is_dir():
                raise FinishError("directories are not task file lists")
            if not (self.root / path).parent.resolve().is_relative_to(self.root):
                raise FinishError("task path escapes the repository")
        # Literal pathspecs prevent names such as :(glob)* from broadening scope.
        self.git("--literal-pathspecs", "add", "--", *files)
        self.unchanged_worktree()
        self.git("diff", "--cached", "--check")
        if not self.git("diff", "--cached", "--name-only"):
            raise FinishError("no task changes to commit")
        self.save(self.intent, {
            "version": 1, "root": str(self.root), "base": self.git("rev-parse", "HEAD"),
            "tree": self.git("write-tree"), "message": message, "files": files,
            "origin": self.remote(), "session_id": session_id, "commit": None, "verified": False,
        })
        return {"prepared": True, "files": files}

    def verify(self):
        script = self.root / "scripts" / "verify-task.sh"
        result = subprocess.run(["bash", str(script)], cwd=self.root,
                                stdout=sys.stderr, stderr=sys.stderr, timeout=480, check=False)
        if result.returncode:
            raise FinishError("task verification failed; nothing new was published")

    def publish(self):
        if not self.intent.exists():
            return {"published": False, "reason": "no prepared task"}
        intent = self.load()
        self.no_operation()
        if self.branch() not in ("staging", "main"):
            raise FinishError("publication only supports staging and main")
        self.unchanged_worktree()
        if self.remote() != intent["origin"]:
            raise FinishError("origin changed after the task was prepared")
        head = self.git("rev-parse", "HEAD")
        if not intent["commit"] and head != intent["base"]:
            # Recover only a single exact commit made just before interruption.
            if (intent["verified"] is not True or self.branch() != "staging"
                    or len(self.git("rev-list", "--parents", "-n", "1", "HEAD").split()) != 2
                    or self.git("rev-parse", "HEAD^") != intent["base"]
                    or self.git("rev-parse", "HEAD^{tree}") != intent["tree"]
                    or self.git("log", "-1", "--format=%B") != intent["message"].strip()):
                raise FinishError("HEAD changed since preparation")
            intent["commit"] = head
            self.save(self.intent, intent)
        self.git("fetch", "origin")
        candidate = intent["commit"] or intent["base"]
        for ref in ("origin/main", "origin/staging", "main"):
            self.ancestor(ref, candidate)
        if not intent["commit"]:
            if self.branch() != "staging" or self.git("write-tree") != intent["tree"]:
                raise FinishError("prepared staging index changed")
            self.verify()
            self.unchanged_worktree()
            if self.git("rev-parse", "HEAD") != intent["base"] or self.git("write-tree") != intent["tree"]:
                raise FinishError("HEAD or index changed during verification")
            self.git("diff", "--cached", "--check")
            intent["verified"] = True
            self.save(self.intent, intent)
            self.git("commit", "-m", intent["message"])
            intent["commit"] = self.git("rev-parse", "HEAD")
            self.save(self.intent, intent)
        commit = intent["commit"]
        if (intent["verified"] is not True
                or len(self.git("rev-list", "--parents", "-n", "1", commit).split()) != 2
                or self.git("rev-parse", f"{commit}^{{tree}}") != intent["tree"]
                or self.git("rev-parse", f"{commit}^") != intent["base"]
                or self.git("rev-parse", "staging") != commit):
            raise FinishError("task commit/staging changed; no automatic publication")
        if self.git("diff", "--cached", "--name-only"):
            raise FinishError("new staged work exists; refusing to resume publication")
        self.unchanged_worktree()
        self.git("push", "origin", f"{commit}:refs/heads/staging")
        self.git("fetch", "origin")
        self.ancestor("origin/main", commit)
        self.ancestor("main", commit)
        self.git("switch", "main")
        self.git("merge", "--ff-only", commit)
        self.git("push", "origin", f"{commit}:refs/heads/main")
        refs = dict(line.split()[::-1] for line in self.git("ls-remote", "--heads", "origin", "main", "staging").splitlines())
        if any(refs.get(f"refs/heads/{branch}") != commit for branch in ("main", "staging")):
            raise FinishError("remote heads changed; inspect before declaring publication complete")
        self.git("switch", "staging")
        report = {"published": True, "commit": commit, "branches": ["staging", "main"]}
        self.save(self.receipt, report)
        self.intent.unlink()
        return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    prepare = sub.add_parser("prepare")
    prepare.add_argument("--message", required=True)
    prepare.add_argument("--session-id", default=os.environ.get("CODEX_THREAD_ID", ""))
    prepare.add_argument("files", nargs="+")
    sub.add_parser("publish")
    sub.add_parser("cancel")
    sub.add_parser("stop-hook")
    args = parser.parse_args(argv)
    hook = args.action == "stop-hook"
    event = {}
    try:
        cwd = Path.cwd()
        if hook:
            event = json.load(sys.stdin)
            if not isinstance(event, dict):
                raise FinishError("hook input must be an object")
            if event.get("hook_event_name") != "Stop" or event.get("stop_hook_active") or event.get("permission_mode") == "plan":
                print("{}")
                return 0
            # Never let event/transcript content select another checkout.
            if Path(event.get("cwd", "")).resolve() != cwd.resolve():
                raise FinishError("hook working directory does not match the event")
        publisher = Publisher(cwd)
        if args.action in ("publish", "stop-hook") and not publisher.intent.exists():
            # A read-only turn must not require write access even to a lock file.
            print(json.dumps({} if hook else {"published": False, "reason": "no prepared task"}))
            return 0
        with publisher.lock():
            if args.action == "prepare":
                result = publisher.prepare(args.message, args.files, args.session_id)
            elif args.action == "cancel":
                publisher.intent.unlink(missing_ok=True)
                result = {"cancelled": True, "index_untouched": True}
            else:
                if hook and publisher.intent.exists():
                    session_id = publisher.load().get("session_id")
                    if not session_id or session_id != event.get("session_id"):
                        print(json.dumps({"systemMessage": "Prepared task belongs to another or unknown session; no publication."}))
                        return 0
                result = publisher.publish()
        print(json.dumps({"systemMessage": json.dumps(result)} if hook and result.get("published") else ({} if hook else result)))
        return 0
    except (FinishError, OSError, ValueError, subprocess.TimeoutExpired) as exc:
        message = f"Task publication stopped: {exc}"
        if hook:
            print(json.dumps({"decision": "block", "reason": message + ". Inspect and resume explicitly; do not bypass checks."}))
            return 0
        print(message, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
