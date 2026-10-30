# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Protocol fixtures only. These tests never run a model or qualify inference."""

import asyncio
from contextlib import redirect_stderr, redirect_stdout
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import struct
import sys
import tempfile
import time
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "qualify_local_agent", Path(__file__).with_name("qualify-local-agent.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)

SECRET = "PRIVATE_TOKEN_MUST_NOT_APPEAR_IN_REPORT"
FIXTURE = r'''
import json, pathlib, sys, time
mode, marker = sys.argv[1:]
secret = "PRIVATE_TOKEN_MUST_NOT_APPEAR_IN_REPORT"
session = "fixture-session"
pending = None
methods = []
def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)
def response(identifier, result):
    emit({"jsonrpc":"2.0", "id":identifier, "result":result})
def chunk(text, wrong=False):
    emit({"jsonrpc":"2.0", "method":"session/update", "params":{
        "sessionId": "wrong-session" if wrong else session, "update":{
            "sessionUpdate":"agent_message_chunk", "content":{"type":"text", "text":text}}}})
def result():
    value = {"stopReason":"end_turn", "_meta":{"tutor":{"generation":{
        "inputTokens":45, "outputTokens":2, "elapsedMs":12, "secret":secret}}, "private":secret}}
    if mode == "missing-stats": value.pop("_meta")
    if mode == "bad-stats": value["_meta"]["tutor"]["generation"]["outputTokens"] = True
    return value
sys.stderr.write(secret + "\n")
sys.stderr.flush()
for line in sys.stdin:
    request = json.loads(line)
    method = request["method"]
    methods.append(method)
    identifier = request.get("id")
    params = request["params"]
    if method == "initialize":
        if mode == "timeout": time.sleep(30)
        if mode == "oversized":
            print("x" * 300000 + secret, flush=True)
            continue
        if mode == "malformed":
            print(secret, flush=True)
            continue
        if mode == "eof": sys.exit(0)
        if mode == "error":
            emit({"jsonrpc":"2.0","id":identifier,"error":{"code":-32603,"message":secret}})
            continue
        if mode == "flood":
            for _ in range(5000):
                emit({"jsonrpc":"2.0","method":"session/update","params":{}})
            continue
        assert params["protocolVersion"] == 1
        response(identifier + 1 if mode == "wrong-id" else identifier, {
            "protocolVersion":2 if mode == "wrong-version" else 1,
            "authMethods":[{"id":secret}] if mode == "auth" else [],
            "agentInfo":{"name":"tutor-local-agent", "version":secret if mode == "secret-version" else "0.1.0"}})
    elif method == "session/new":
        assert params["mcpServers"] == []
        assert pathlib.Path(params["cwd"]).is_dir()
        response(identifier, {"sessionId":session,"configOptions":[{
            "id":"model","currentValue":"qwen3-4b-instruct-2507-q4_k_m"}]})
    elif method == "session/prompt":
        text = params["prompt"][0]["text"]
        assert params["sessionId"] == session
        if "17 + 25" in text:
            if mode == "text-limit":
                chunk("x" * 66000 + secret)
            elif mode == "secret-text":
                chunk(secret)
            elif mode == "wrong-session":
                chunk("42", wrong=True)
            elif mode == "wrong-answer":
                chunk("43")
            else:
                chunk("4")
                chunk("2")
            response(identifier, result())
        else:
            assert "1000" in text
            pending = identifier
            chunk("1. Apprenons les additions. ")
            if mode == "finished-before-cancel":
                response(pending, result())
                pending = None
    elif method == "session/cancel":
        assert "id" not in request
        if mode == "ignore-cancel": time.sleep(30)
        if pending is not None:
            response(pending, {"stopReason":"cancelled"})
            pending = None
if mode == "ignore-eof": time.sleep(30)
pathlib.Path(marker).write_text(json.dumps(methods), encoding="utf-8")
'''


class QualificationTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="tutor qualification élève 日本語 ")
        self.root = Path(self.temporary.name)
        self.model = self.root / "modèle 'quoted' $(touch injected); &.gguf"
        # An intentionally fake GGUF header, never treated as model inference.
        self.model.write_bytes(struct.pack("<4sIQQ", b"GGUF", 3, 1, 1) + b"fixture only")
        self.fixture = self.root / "agent fixture.py"
        self.fixture.write_text(FIXTURE, encoding="utf-8")
        self.marker = self.root / "eof.json"
        self.output = self.root / "report.json"
        self.options = runner.parse_options([
            "--agent", str(Path(sys.executable).resolve()), "--model", str(self.model),
            "--output", str(self.output), "--timeout", "5", "--cancel-timeout", "0.15",
            "--shutdown-timeout", "0.15"])

    def tearDown(self):
        self.temporary.cleanup()

    async def qualify(self, mode="normal"):
        report = await runner.qualify_protocol(
            self.options, [sys.executable, "-u", str(self.fixture), mode, str(self.marker)])
        self.assertEqual(report["scope"], "protocol_only")
        self.assertEqual(report["inference_status"], "not_tested")
        self.assertFalse(report["qualified"])
        self.assertFalse(report["target_hardware_qualified"])
        self.assertEqual(report["qualification_scope"], "ordinary_acp_on_tested_host_only")
        self.assertEqual(report["host"]["scope"], "execution_host_only")
        self.assertEqual(report["host"]["requested_compute"], "cpu")
        encoded = json.dumps(report)
        self.assertNotIn(SECRET, encoded)
        self.assertNotIn(str(self.root), encoded)
        self.assertNotIn(FIXTURE, encoded)
        return report

    async def test_fake_process_exercises_cold_warm_cancel_and_graceful_eof_only(self):
        report = await self.qualify()
        self.assertTrue(report["protocol_passed"], report)
        self.assertEqual(report["runtime"], {"name":"tutor-local-agent", "version":"0.1.0"})
        self.assertEqual(report["artifacts"]["model"]["sha256"],
                         hashlib.sha256(self.model.read_bytes()).hexdigest())
        for key in ("arithmetic", "warm_arithmetic"):
            metrics = report["metrics"][key]
            self.assertTrue(metrics["arithmetic_correct"])
            self.assertIsNotNone(metrics["first_text_ms"])
            self.assertEqual(metrics["generation"]["output_tokens"], 2)
            self.assertEqual(metrics["chunks"], 2)
        cancellation = report["metrics"]["cancellation"]
        self.assertTrue(cancellation["cancel_sent"])
        self.assertEqual(cancellation["stop_reason"], "cancelled")
        self.assertTrue(cancellation["within_two_seconds"])
        self.assertEqual(report["shutdown"],
                         {"graceful_eof":True, "forced":False, "reaped":True, "exit_code":0})
        self.assertEqual(json.loads(self.marker.read_text()), [
            "initialize", "session/new", "session/prompt", "session/prompt",
            "session/prompt", "session/cancel"])
        self.assertFalse((self.root / "injected").exists())

    async def test_frames_rpc_errors_and_agent_metadata_never_leak(self):
        for mode, code in [
            ("oversized", "frame_too_large"), ("malformed", "invalid_json_frame"),
            ("error", "agent_rpc_error"), ("wrong-id", "response_id_mismatch"),
            ("wrong-version", "protocol_version_mismatch"),
            ("secret-version", "invalid_runtime_version"), ("auth", "unexpected_auth_methods"),
            ("wrong-session", "session_id_mismatch"), ("text-limit", "text_budget_exceeded"),
            ("eof", "unexpected_eof"), ("flood", "unexpected_session_update"),
        ]:
            with self.subTest(mode=mode):
                report = await self.qualify(mode)
                self.assertIn(code, report["errors"])
                self.assertFalse(report["protocol_passed"])
                self.assertTrue(report["shutdown"]["reaped"])

    async def test_text_and_unvalidated_token_metadata_are_not_evidence(self):
        for mode, validation in [("secret-text", "arithmetic"), ("wrong-answer", "arithmetic"),
                                 ("missing-stats", "generation_metadata"),
                                 ("bad-stats", "generation_metadata")]:
            with self.subTest(mode=mode):
                report = await self.qualify(mode)
                self.assertEqual(report["validations"][validation], "failed")
                self.assertFalse(report["protocol_passed"])
                self.assertTrue(report["shutdown"]["reaped"])

    async def test_a_finished_response_cannot_be_claimed_as_successful_cancellation(self):
        report = await self.qualify("finished-before-cancel")
        self.assertEqual(report["validations"]["cancellation"], "failed")
        self.assertFalse(report["protocol_passed"])

    async def test_timeouts_and_ignored_eof_force_reap_with_failed_validation(self):
        for mode in ("timeout", "ignore-cancel", "ignore-eof"):
            with self.subTest(mode=mode):
                self.options.timeout = 0.4 if mode == "timeout" else 5
                started = time.monotonic()
                report = await self.qualify(mode)
                self.assertLess(time.monotonic() - started, 4)
                self.assertFalse(report["protocol_passed"])
                self.assertTrue(report["shutdown"]["reaped"])
                self.assertTrue(report["shutdown"]["forced"])
                if mode != "ignore-eof":
                    self.assertIn("qualification_timeout", report["errors"])

    async def test_wrong_digest_and_bad_header_fail_before_spawning(self):
        self.options.model_sha256 = "0" * 64
        with mock.patch.object(asyncio, "create_subprocess_exec") as spawn:
            report = await self.qualify()
        spawn.assert_not_called()
        self.assertIn("model_sha256_mismatch", report["errors"])
        self.options.model_sha256 = None
        self.model.write_bytes(b"GGUF")
        report = await self.qualify()
        self.assertIn("invalid_gguf_header", report["errors"])

    async def test_explicit_manifest_requires_exact_digest_size_and_identity(self):
        profile = {"id":runner.MODEL_ID, "gguf_bytes":self.model.stat().st_size,
                   "gguf_sha256":hashlib.sha256(self.model.read_bytes()).hexdigest()}
        self.options.model_profile = self.root / "profile.json"
        self.options.model_profile.write_text(json.dumps(profile))
        report = await self.qualify()
        self.assertTrue(report["artifacts"]["model"]["manifest_verified"])
        self.assertFalse(report["artifacts"]["model"]["pinned_identity_verified"])
        profile["gguf_bytes"] += 1
        self.options.model_profile.write_text(json.dumps(profile))
        report = await self.qualify()
        self.assertIn("model_profile_mismatch", report["errors"])

    def fixture_profile(self, filename="profile.json", **overrides):
        profile = {"id":runner.MODEL_ID, "gguf_bytes":self.model.stat().st_size,
                   "gguf_sha256":hashlib.sha256(self.model.read_bytes()).hexdigest()}
        profile.update(overrides)
        path = self.root / filename
        path.write_text(json.dumps(profile))
        return path

    async def test_live_run_cannot_replace_missing_pin_with_a_digest_or_custom_manifest(self):
        self.options.model_sha256 = hashlib.sha256(self.model.read_bytes()).hexdigest()
        self.options.model_profile = self.fixture_profile()
        with mock.patch.object(runner, "PINNED_MODEL_PROFILE", self.root / "missing-pin.json"), \
             mock.patch.object(asyncio, "create_subprocess_exec") as spawn:
            report = await runner.qualify(self.options)
        spawn.assert_not_called()
        self.assertIn("pinned_model_profile_unavailable", report["errors"])
        self.assertFalse(report["qualified"])
        self.assertFalse(report["target_hardware_qualified"])
        self.assertEqual(report["inference_status"], "not_observed")

    async def test_live_run_cannot_repin_an_arbitrary_file_with_cli_overrides(self):
        pinned = self.fixture_profile("pin.json", gguf_sha256="a" * 64)
        supplied = self.fixture_profile("override.json")
        digest = hashlib.sha256(self.model.read_bytes()).hexdigest()
        for override_digest, override_profile, expected in [
            (digest, None, "model_sha256_conflicts_with_pin"),
            (None, supplied, "model_profile_conflicts_with_pin"),
            (None, None, "model_profile_mismatch"),
        ]:
            with self.subTest(expected=expected):
                self.options.model_sha256 = override_digest
                self.options.model_profile = override_profile
                with mock.patch.object(runner, "PINNED_MODEL_PROFILE", pinned), \
                     mock.patch.object(asyncio, "create_subprocess_exec") as spawn:
                    report = await runner.qualify(self.options)
                spawn.assert_not_called()
                self.assertIn(expected, report["errors"])
                self.assertFalse(report["qualified"])
                self.assertFalse(report["target_hardware_qualified"])

    async def test_live_pin_requires_exact_bytes_even_when_digest_matches(self):
        pinned = self.fixture_profile("pin.json", gguf_bytes=self.model.stat().st_size + 1)
        with mock.patch.object(runner, "PINNED_MODEL_PROFILE", pinned), \
             mock.patch.object(asyncio, "create_subprocess_exec") as spawn:
            report = await runner.qualify(self.options)
        spawn.assert_not_called()
        self.assertIn("model_profile_mismatch", report["errors"])
        self.assertFalse(report["qualified"])

    def test_live_artifact_gate_passes_the_pinned_digest_not_an_unreviewed_local_digest(self):
        # Pure artifact-gate test using an explicitly patched fixture pin. No
        # subprocess/model runs and no live qualification report is produced.
        pinned = self.fixture_profile("pin.json")
        with mock.patch.object(runner, "PINNED_MODEL_PROFILE", pinned):
            artifacts = runner.inspect_artifacts(self.options, time.monotonic() + 5, False)
        model = artifacts["model"]
        self.assertTrue(model["pinned_identity_verified"])
        expected = json.loads(pinned.read_text())["gguf_sha256"]
        self.assertEqual(runner.command(self.options, model["pinned_sha256"])[-2:],
                         ["--sha256", expected])

    def test_native_command_keeps_unicode_paths_separate_and_bounds_defaults(self):
        argv = runner.command(self.options, "a" * 64)
        self.assertEqual(argv, [str(self.options.agent), "--model", str(self.model),
                               "--context", "4096", "--max-output", "128", "--gpu-layers", "0",
                               "--threads", "2", "--batch", "32", "--sha256", "a" * 64])
        self.assertNotIn("-c", argv)
        for key, bad in (("max_output", 769), ("gpu_layers", 37), ("batch", 513),
                         ("threads", 0), ("timeout", 0), ("cancel_timeout", 31)):
            old = getattr(self.options, key)
            setattr(self.options, key, bad)
            with self.assertRaises(runner.QualificationError):
                runner.validate_options(self.options)
            setattr(self.options, key, old)

    async def test_atomic_report_contains_no_transcript_and_never_overwrites_model(self):
        report = await self.qualify()
        runner.write_report(report, self.output)
        self.assertEqual(json.loads(self.output.read_text()), report)
        self.assertNotIn("Apprenons", self.output.read_text())
        self.options.output = self.model
        with self.assertRaises(runner.QualificationError):
            runner.validate_options(self.options)

    def test_streamed_fingerprint_reads_bounded_chunks(self):
        source = io.BytesIO(b"a" * (runner.HASH_CHUNK * 3 + 42))
        sizes = []
        class Source:
            def __enter__(self): return self
            def __exit__(self, *_args): pass
            def fileno(self): return 0
            def read(self, size):
                sizes.append(size)
                return source.read(size)
        stamp = mock.Mock(st_size=len(source.getvalue()), st_mtime_ns=1)
        with mock.patch.object(Path, "open", return_value=Source()), \
             mock.patch.object(runner.os, "fstat", return_value=stamp):
            digest = runner.fingerprint(Path("unused"), time.monotonic() + 5)
        self.assertEqual(digest["sha256"], hashlib.sha256(source.getvalue()).hexdigest())
        self.assertEqual(set(sizes), {runner.HASH_CHUNK})
        self.assertGreater(len(sizes), 3)

    async def test_json_frame_count_and_duplicate_keys_are_bounded(self):
        process = mock.Mock()
        process.stdout = asyncio.StreamReader(limit=runner.MAX_FRAME)
        wire = runner.Wire(process)
        process.stdout.feed_data(b'{"jsonrpc":"2.0","id":1,"id":2}\n')
        with self.assertRaisesRegex(runner.QualificationError, "invalid_json_frame"):
            await wire.receive(time.monotonic() + 1)
        wire.frames = runner.MAX_FRAMES
        process.stdout.feed_data(b'{"jsonrpc":"2.0","id":1,"result":{}}\n')
        with self.assertRaisesRegex(runner.QualificationError, "wire_budget_exceeded"):
            await wire.receive(time.monotonic() + 1)

    def test_invalid_cli_is_redacted_and_non_finite_timeouts_are_rejected(self):
        for args in (["--unknown", SECRET], ["--agent", SECRET, "--timeout", "nan"]):
            output = io.StringIO()
            with redirect_stderr(output), self.assertRaises(SystemExit):
                runner.parse_options(args)
            self.assertNotIn(SECRET, output.getvalue())
        with self.assertRaises(ValueError):
            runner.finite_number("inf")

    def test_cli_refuses_input_output_alias_without_launching_any_agent(self):
        args = ["--agent", str(self.options.agent), "--model", str(self.model),
                "--output", str(self.model)]
        original = self.model.read_bytes()
        with redirect_stderr(io.StringIO()), redirect_stdout(io.StringIO()), \
             mock.patch.object(asyncio, "create_subprocess_exec") as spawn:
            self.assertEqual(runner.main(args), 2)
        spawn.assert_not_called()
        self.assertEqual(self.model.read_bytes(), original)

    def test_reports_cannot_overwrite_the_authoritative_pin_or_additional_manifest(self):
        pinned = self.fixture_profile("pin.json")
        self.options.model_profile = self.fixture_profile("additional.json")
        with mock.patch.object(runner, "PINNED_MODEL_PROFILE", pinned):
            for path in (pinned, self.options.model_profile):
                self.options.output = path
                with self.assertRaisesRegex(runner.QualificationError, "report_overwrites_input"):
                    runner.validate_options(self.options)


if __name__ == "__main__":
    unittest.main()
