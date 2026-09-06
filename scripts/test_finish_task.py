"""Offline integration tests: all pushes target disposable local bare repos."""

import contextlib
import fcntl
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from finish_task import Publisher, main


SCRIPT = Path(__file__).with_name("finish_task.py").resolve()


class TaskPublicationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="task-publish-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.remote = self.root / "origin.git"
        self.env = {**os.environ, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                    "PYTHONDONTWRITEBYTECODE": "1", "GIT_TERMINAL_PROMPT": "0"}
        subprocess.run(["git", "init", "--bare", "-b", "main", str(self.remote)], env=self.env, check=True, capture_output=True)
        subprocess.run(["git", "init", "-b", "main", str(self.repo)], env=self.env, check=True, capture_output=True)
        self.git("config", "user.name", "Task Tests")
        self.git("config", "user.email", "tests@example.invalid")
        self.git("config", "commit.gpgsign", "false")
        self.git("remote", "add", "origin", str(self.remote))
        self.write("work.txt", "before\n")
        self.write("delete.txt", "tracked deletion fixture\n")
        self.write("scripts/verify-task.sh", '#!/bin/sh\nprintf x >> "$(git rev-parse --git-dir)/verified"\n')
        self.git("add", ".")
        self.git("commit", "-m", "initial")
        self.base = self.git("rev-parse", "HEAD")
        self.git("switch", "-c", "staging")
        self.git("push", "origin", "main", "staging")

    def write(self, name, content):
        target = self.repo / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)

    def git(self, *args, cwd=None):
        result = subprocess.run(["git", *args], cwd=cwd or self.repo, env=self.env,
                                text=True, capture_output=True, check=True)
        return result.stdout.strip()

    def invoke(self, *args, event=None, ok=True):
        result = subprocess.run([sys.executable, str(SCRIPT), *args], cwd=self.repo,
                                env=self.env, text=True, input=json.dumps(event) if event is not None else None,
                                capture_output=True, timeout=30)
        if ok:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout)
        return result

    def prepare(self, *files):
        self.write("work.txt", "after\n")
        return self.invoke("prepare", "--message", "feat: finished task", "--session-id", "session-test", "--", *(files or ("work.txt",)))

    def heads(self):
        return [self.git("rev-parse", name, cwd=self.remote) for name in ("staging", "main")]

    def test_publish_explicit_files_preserves_notes_and_returns_to_staging(self):
        self.write("HANDOFF.md", "personal note\n")
        self.write("new file.txt", "new content\n")
        (self.repo / "delete.txt").unlink()
        self.prepare("work.txt", "new file.txt", "delete.txt")
        result = json.loads(self.invoke("publish").stdout)
        self.assertTrue(result["published"])
        self.assertEqual(self.heads(), [result["commit"]] * 2)
        self.assertEqual(self.git("branch", "--show-current"), "staging")
        self.assertEqual(self.git("rev-parse", "HEAD^"), self.base)
        self.assertEqual(self.git("ls-files", "HANDOFF.md"), "")
        self.assertEqual((self.repo / "HANDOFF.md").read_text(), "personal note\n")
        self.assertEqual(self.git("ls-files", "delete.txt"), "")
        self.assertFalse((self.repo / ".git/tutor-task-finish.json").exists())
        self.assertFalse(json.loads(self.invoke("publish").stdout)["published"])

    def test_existing_staged_work_is_not_absorbed(self):
        self.write("work.txt", "user staged change\n")
        self.git("add", "work.txt")
        before = self.git("write-tree")
        self.invoke("prepare", "--message", "task", "work.txt", ok=False)
        self.assertEqual(self.git("write-tree"), before)
        self.assertEqual(self.heads(), [self.base] * 2)

    def test_directories_and_pathspecs_cannot_expand_scope(self):
        for name in (".", "scripts", "../outside", "/tmp/outside", ".git/config", ":(glob)*"):
            with self.subTest(path=name):
                self.invoke("prepare", "--message", "task", "--", name, ok=False)
                self.assertEqual(self.git("diff", "--cached", "--name-only"), "")

    def test_main_is_not_a_preparation_branch(self):
        self.git("switch", "main")
        self.write("work.txt", "after\n")
        self.invoke("prepare", "--message", "task", "work.txt", ok=False)
        self.assertEqual(self.heads(), [self.base] * 2)

    def test_changed_index_blocks_publication(self):
        self.prepare()
        self.write("work.txt", "changed after preparation\n")
        self.git("add", "work.txt")
        self.invoke("publish", ok=False)
        self.assertEqual(self.heads(), [self.base] * 2)

    def test_unstaged_tracked_work_blocks_publication(self):
        self.prepare()
        self.write("delete.txt", "unrelated user edit\n")
        self.invoke("publish", ok=False)
        self.assertEqual(self.heads(), [self.base] * 2)

    def test_failed_verification_does_not_commit_or_push(self):
        self.write("scripts/verify-task.sh", "#!/bin/sh\nexit 1\n")
        self.prepare("work.txt", "scripts/verify-task.sh")
        self.invoke("publish", ok=False)
        self.assertEqual(self.git("rev-parse", "HEAD"), self.base)
        self.assertEqual(self.heads(), [self.base] * 2)

    def test_verification_cannot_silently_change_the_candidate(self):
        self.write("scripts/verify-task.sh", "#!/bin/sh\nprintf changed > work.txt\ngit add work.txt\n")
        self.prepare("work.txt", "scripts/verify-task.sh")
        self.invoke("publish", ok=False)
        self.assertEqual(self.heads(), [self.base] * 2)

    def test_remote_divergence_is_not_overwritten(self):
        self.prepare()
        clone = self.root / "other"
        subprocess.run(["git", "clone", str(self.remote), str(clone)], env=self.env, check=True, capture_output=True)
        self.git("config", "user.name", "Other", cwd=clone)
        self.git("config", "user.email", "other@example.invalid", cwd=clone)
        (clone / "other.txt").write_text("concurrent work\n")
        self.git("add", "other.txt", cwd=clone)
        self.git("commit", "-m", "concurrent work", cwd=clone)
        self.git("push", "origin", "main", cwd=clone)
        concurrent = self.git("rev-parse", "HEAD", cwd=clone)
        self.invoke("publish", ok=False)
        self.assertEqual(self.heads(), [self.base, concurrent])

    def test_retry_after_main_push_failure_reuses_the_same_commit(self):
        hook = self.remote / "hooks/update"
        hook.write_text('#!/bin/sh\nif [ "$1" = refs/heads/main ] && [ -f reject-main ]; then exit 1; fi\n')
        hook.chmod(0o700)
        marker = self.remote / "reject-main"
        marker.touch()
        self.prepare()
        self.invoke("publish", ok=False)
        commit = self.git("rev-parse", "staging")
        self.assertEqual(self.heads(), [commit, self.base])
        marker.unlink()
        self.invoke("publish")
        self.assertEqual(self.heads(), [commit] * 2)
        self.assertEqual((self.repo / ".git/verified").read_text(), "x")

    def test_manual_commit_after_prepare_does_not_bypass_verification(self):
        self.prepare()
        self.git("commit", "-m", "feat: finished task")
        self.invoke("publish", ok=False)
        self.assertEqual(self.heads(), [self.base] * 2)

    def test_stop_only_publishes_ready_work_in_the_same_session(self):
        event = {"hook_event_name": "Stop", "cwd": str(self.repo), "session_id": "session-test"}
        self.assertEqual(json.loads(self.invoke("stop-hook", event=event).stdout), {})
        self.prepare()
        for ignored in ({**event, "stop_hook_active": True}, {**event, "session_id": "other"},
                        {**event, "hook_event_name": "SubagentStop"}, {**event, "permission_mode": "plan"}):
            self.invoke("stop-hook", event=ignored)
            self.assertEqual(self.heads(), [self.base] * 2)
        response = json.loads(self.invoke("stop-hook", event=event).stdout)
        self.assertIn("systemMessage", response)
        self.assertEqual(self.heads(), [self.git("rev-parse", "HEAD")] * 2)

    def test_read_only_stop_never_opens_a_write_lock(self):
        event = {"hook_event_name": "Stop", "cwd": str(self.repo), "session_id": "session-test"}
        with (patch.dict(os.environ, self.env),
              patch("finish_task.Path.cwd", return_value=self.repo),
              patch("sys.stdin", io.StringIO(json.dumps(event))),
              contextlib.redirect_stdout(io.StringIO()) as output,
              patch.object(Publisher, "lock", side_effect=AssertionError("unexpected write lock"))):
            self.assertEqual(main(["stop-hook"]), 0)
        self.assertEqual(json.loads(output.getvalue()), {})

    def test_stop_failure_requests_one_continuation_without_publishing(self):
        self.prepare()
        self.write("work.txt", "changed\n")
        event = {"hook_event_name": "Stop", "cwd": str(self.repo), "session_id": "session-test"}
        response = json.loads(self.invoke("stop-hook", event=event).stdout)
        self.assertEqual(response["decision"], "block")
        self.assertEqual(self.heads(), [self.base] * 2)
        response = json.loads(self.invoke("stop-hook", event={**event, "stop_hook_active": True}).stdout)
        self.assertEqual(response, {})

    def test_cancel_preserves_staged_work(self):
        self.prepare()
        before = self.git("write-tree")
        self.invoke("cancel")
        self.assertEqual(self.git("write-tree"), before)
        self.assertFalse((self.repo / ".git/tutor-task-finish.json").exists())

    def test_concurrent_publication_is_blocked(self):
        self.prepare()
        with (self.repo / ".git/tutor-task-finish.lock").open("a") as handle:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.invoke("publish", ok=False)
        self.assertEqual(self.heads(), [self.base] * 2)

    def test_interruption_after_commit_recovers_verified_candidate(self):
        self.prepare()
        with patch.dict(os.environ, self.env), contextlib.redirect_stderr(sys.stdout):
            publisher = Publisher(self.repo)
            original = publisher.save

            def interrupted_save(path, value):
                if path == publisher.intent and value.get("commit"):
                    raise OSError("simulated interruption after Git commit")
                original(path, value)

            with patch.object(publisher, "save", side_effect=interrupted_save):
                with self.assertRaises(OSError):
                    publisher.publish()
        commit = self.git("rev-parse", "HEAD")
        self.assertNotEqual(commit, self.base)
        self.invoke("publish")
        self.assertEqual(self.heads(), [commit] * 2)
        self.assertEqual((self.repo / ".git/verified").read_text(), "x")


if __name__ == "__main__":
    unittest.main()
