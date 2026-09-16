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

from finish_task import FinishError, Publisher, github_repository, main


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

    def test_failed_ci_blocks_main_and_retry_reuses_the_commit(self):
        self.prepare()
        with patch.dict(os.environ, self.env):
            publisher = Publisher(self.repo)
            with patch.object(publisher, "wait_for_checks", side_effect=FinishError("CI failed")):
                with self.assertRaisesRegex(FinishError, "CI failed"):
                    publisher.publish()
            commit = self.git("rev-parse", "staging")
            self.assertEqual(self.heads(), [commit, self.base])
            self.assertEqual(publisher.load()["commit"], commit)
            receipt = {"commit": commit, "conclusion": "success"}
            with patch.object(publisher, "wait_for_checks", return_value=receipt) as gate:
                report = publisher.publish()
            gate.assert_called_once_with(commit)
        self.assertEqual(report["github_checks"], receipt)
        self.assertEqual(self.heads(), [commit] * 2)
        self.assertEqual((self.repo / ".git/verified").read_text(), "x")

    def test_staging_advance_during_ci_wait_blocks_main(self):
        self.prepare()

        def advance_remote(commit):
            clone = self.root / "concurrent"
            subprocess.run(["git", "clone", "-b", "staging", str(self.remote), str(clone)],
                           env=self.env, check=True, capture_output=True)
            self.git("config", "user.name", "Other", cwd=clone)
            self.git("config", "user.email", "other@example.invalid", cwd=clone)
            (clone / "other.txt").write_text("concurrent work\n")
            self.git("add", "other.txt", cwd=clone)
            self.git("commit", "-m", "concurrent task", cwd=clone)
            self.git("push", "origin", "staging", cwd=clone)
            return {"commit": commit, "conclusion": "success"}

        with patch.dict(os.environ, self.env):
            publisher = Publisher(self.repo)
            with patch.object(publisher, "wait_for_checks", side_effect=advance_remote):
                with self.assertRaisesRegex(FinishError, "staging or the index changed"):
                    publisher.publish()
        self.assertEqual(self.heads()[1], self.base)
        self.assertNotEqual(self.heads()[0], self.git("rev-parse", "staging"))
        self.assertTrue(publisher.intent.exists())

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


class GitHubCheckGateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="task-check-gate-")
        self.addCleanup(self.temp.cleanup)
        self.publisher = Publisher.__new__(Publisher)
        self.publisher.root = Path(self.temp.name)
        policy_dir = self.publisher.root / ".github"
        policy_dir.mkdir()
        self.policy = policy_dir / "required-checks.json"
        self.policy.write_text(json.dumps({"contexts": ["Test", "Govulncheck"], "app_id": 15368}))
        self.remote = patch.object(self.publisher, "remote", return_value="git@github.com:owner/repo.git")
        self.remote.start()
        self.addCleanup(self.remote.stop)

    @staticmethod
    def check(name, **overrides):
        return {"id": 1, "name": name, "head_sha": "candidate", "app": {"id": 15368},
                "status": "completed", "conclusion": "success", **overrides}

    def test_only_github_and_local_remotes_are_supported(self):
        for remote in ("git@github.com:owner/repo.git", "https://github.com/owner/repo.git",
                       "ssh://git@github.com/owner/repo"):
            self.assertEqual(github_repository(remote), "owner/repo")
        for remote in ("/tmp/repo.git", "../repo.git", "file:///tmp/repo.git"):
            self.assertIsNone(github_repository(remote))
        for remote in ("https://example.org/owner/repo", "git@elsewhere:owner/repo",
                       "https://github.com/owner/repo/extra", "https://github.com/owner"):
            with self.assertRaises(FinishError):
                github_repository(remote)

    def test_missing_running_wrong_commit_and_wrong_app_checks_wait(self):
        good = [self.check("Test"), self.check("Govulncheck")]
        pending = [self.check("Test", head_sha="older"),
                   self.check("Govulncheck", app={"id": 999})]
        running = [self.check("Test"), self.check("Govulncheck", status="in_progress", conclusion=None)]
        with (patch.object(self.publisher, "github_check_runs", side_effect=[[], pending, running, good]) as api,
              patch("finish_task.time.sleep") as sleep):
            receipt = self.publisher.wait_for_checks("candidate")
        self.assertEqual(receipt["conclusion"], "success")
        self.assertEqual(api.call_count, 4)
        self.assertEqual(sleep.call_count, 3)

    def test_failure_cancellation_and_skipped_checks_block(self):
        for conclusion in ("failure", "cancelled", "skipped", "neutral", "timed_out"):
            with self.subTest(conclusion=conclusion):
                checks = [self.check("Test"), self.check("Govulncheck", conclusion=conclusion)]
                with patch.object(self.publisher, "github_check_runs", return_value=checks):
                    with self.assertRaisesRegex(FinishError, "main was not advanced"):
                        self.publisher.wait_for_checks("candidate")

    def test_latest_rerun_replaces_previous_failure(self):
        checks = [self.check("Test", id=10), self.check("Test", id=2, conclusion="failure"),
                  self.check("Govulncheck")]
        with patch.object(self.publisher, "github_check_runs", return_value=checks):
            self.assertEqual(self.publisher.wait_for_checks("candidate")["conclusion"], "success")

    def test_missing_checks_time_out(self):
        with (patch.object(self.publisher, "github_check_runs", return_value=[]),
              patch("finish_task.time.monotonic", side_effect=[0, 3600])):
            with self.assertRaisesRegex(FinishError, "resume publish for the same commit"):
                self.publisher.wait_for_checks("candidate")

    def test_invalid_policy_cannot_disable_checks(self):
        for policy in ([], {}, {"contexts": [], "app_id": 15368},
                       {"contexts": ["Test", "Test"], "app_id": 15368},
                       {"contexts": ["Test"], "app_id": True}):
            self.policy.write_text(json.dumps(policy))
            with self.assertRaisesRegex(FinishError, "invalid required GitHub checks policy"):
                self.publisher.wait_for_checks("candidate")

    def test_check_run_pages_are_combined(self):
        pages = [{"check_runs": [self.check("Test")]}, {"check_runs": [self.check("Govulncheck")]}]
        with patch("finish_task.command", return_value=json.dumps(pages)) as command:
            self.assertEqual(len(self.publisher.github_check_runs("owner/repo", "candidate")), 2)
        self.assertIn("--paginate", command.call_args.args[0])


if __name__ == "__main__":
    unittest.main()
