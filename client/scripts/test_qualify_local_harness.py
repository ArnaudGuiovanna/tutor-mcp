# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Protocol fixtures only: no model execution, downloads or inference claims."""

import asyncio
from contextlib import redirect_stderr, redirect_stdout
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import struct
import sys
import tempfile
import time
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "qualify_local_harness", Path(__file__).with_name("qualify-local-harness.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)

SECRET = "PRIVATE_FIXTURE_DATA_NEVER_REPORT"

# This deliberately scripted ACP peer exercises actual loopback HTTP and the
# journal inspector, not LLM inference. Its SQLite shape matches the production
# journal columns consumed by the inspector. No native library is imported.
FIXTURE = r'''
import hashlib, json, pathlib, sqlite3, sys, time, urllib.request, uuid
mode, model = sys.argv[1:3]
data = pathlib.Path(sys.argv[sys.argv.index("--data-dir") + 1])
secret = "PRIVATE_FIXTURE_DATA_NEVER_REPORT"
session, turn, operation = str(uuid.uuid4()), str(uuid.uuid4()), str(uuid.uuid4())
tool, tool_id = "get_pending_alerts", "tutor/get_pending_alerts"
arguments = {"domain_id":"qualification-domain"}
url = None
pending = None
db = None
projections = 0
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
def encode(value):
    return json.dumps(value, ensure_ascii=False, separators=(",",":"), sort_keys=True).encode()
def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)
def reply(identifier, result):
    emit({"jsonrpc":"2.0","id":identifier,"result":result})
def update(value):
    emit({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":value}})
def tool_update(status, initial=False):
    update({"sessionUpdate":"tool_call" if initial else "tool_call_update", "toolCallId":operation,
            "title":tool,"kind":"other","status":status})
def event(kind, payload, parent=turn):
    identifier = str(uuid.uuid4())
    raw = encode(payload)
    db.execute("INSERT INTO events(id,turn_id,kind,payload,payload_sha256) VALUES(?,?,?,?,?)",
               (identifier,parent,kind,raw,hashlib.sha256(raw).digest()))
    return identifier
def project(kept=()):
    global projections
    projections += 1
    event("context_projection", {"input_tokens":100,"output_reserved":256,
                                 "kept_ids":list(kept),"omitted":[]})
def action(payload):
    project()
    event("harness_model_action", {"raw":encode(payload).decode(),"complete":True,"rejected_chunk_bytes":0})
def mcp(method, params, identifier=1):
    message = {"jsonrpc":"2.0","method":method,"params":params}
    if identifier is not None: message["id"] = identifier
    request = urllib.request.Request(url, data=encode(message), headers={"Content-Type":"application/json"})
    with opener.open(request, timeout=3) as response:
        raw = response.read(128*1024)
    return json.loads(raw)["result"] if raw else None
def create_journal(context):
    global db
    directory = data / "fixture-owner"
    directory.mkdir(parents=True)
    db = sqlite3.connect(directory / (session + ".sqlite3"))
    db.executescript(''' + '"""' + r'''
      CREATE TABLE turns(id TEXT,payload BLOB,payload_sha256 BLOB);
      CREATE TABLE events(sequence INTEGER PRIMARY KEY,id TEXT,turn_id TEXT,kind TEXT,payload BLOB,payload_sha256 BLOB);
      CREATE TABLE operations(id TEXT,turn_id TEXT,tool TEXT,arguments BLOB,idempotency_key TEXT,mutating INTEGER,state TEXT);
      CREATE TABLE transitions(sequence INTEGER PRIMARY KEY,operation_id TEXT,from_state TEXT,to_state TEXT);
      CREATE TABLE results(operation_id TEXT,payload BLOB);
    ''' + '"""' + r''')
    event("session_header", {"version":1,"session_id":session,
          "model_sha256":hashlib.sha256(pathlib.Path(model).read_bytes()).hexdigest()}, parent=None)
    event("verified_host_context", context)
def transition(previous, new):
    db.execute("INSERT INTO transitions(operation_id,from_state,to_state) VALUES(?,?,?)", (operation,previous,new))
def finish(allowed):
    global db
    if allowed:
        tool_update("in_progress")
        result = mcp("tools/call", {"name":tool,"arguments":arguments}, 3)
        if mode == "duplicate-call": mcp("tools/call", {"name":tool,"arguments":arguments}, 4)
        transition("prepared","dispatched")
        transition("dispatched","completed")
        db.execute("UPDATE operations SET state='completed'")
        receipt = result if mode != "wrong-receipt" else {"content":[],"structuredContent":{"forged":True}}
        db.execute("INSERT INTO results VALUES(?,?)", (operation,encode(receipt)))
        tool_update("completed")
        selected = None
        if mode != "unread-reference":
            action({"action":"read_result","reference":"result_1","pointer":"/alerts/0/message"})
            selected = event("harness_result_selection", {"reference":"result_1","operation_id":operation,
                             "pointer":"/alerts/0/message","value":result["structuredContent"]["alerts"][0]["message"]})
        action({"action":"answer"})
        project([selected] if selected and mode != "omitted-selection" else [])
        answer = result["structuredContent"]["alerts"][0]["message"] + " " + secret
        if mode == "wrong-answer": answer = "Référence result_1 reçue. " + secret
    else:
        transition("prepared","rejected")
        db.execute("UPDATE operations SET state='rejected'")
        tool_update("failed")
        event("harness_action_status", {"code":"tool_permission_denied"})
        project()
        answer = "La consultation des alertes a été refusée. " + secret
        if mode == "denied-hallucination": answer += " Révision prévue à 17 h 42."
    event("assistant_chunk", {"text":answer})
    event("turn_finished", "complete")
    db.commit()
    if mode == "bad-event-hash": db.execute("UPDATE events SET payload_sha256=? WHERE kind='harness_model_action'", (b'x'*32,))
    if mode == "context-overflow":
        raw = encode({"input_tokens":4000,"output_reserved":256,"kept_ids":[],"omitted":[]})
        db.execute("UPDATE events SET payload=?,payload_sha256=? WHERE kind='context_projection'",
                   (raw,hashlib.sha256(raw).digest()))
    db.commit()
    db.close()
    db = None
    update({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":answer}})
    stats = {"inputTokens":projections*100,"outputTokens":projections*12,"elapsedMs":20,"private":secret}
    if mode == "bad-stats": stats["outputTokens"] = True
    reply(pending, {"stopReason":"end_turn","_meta":{"tutor":{"generation":stats}}})

sys.stderr.write(secret+"\n")
for line in sys.stdin:
    request = json.loads(line)
    method, params = request.get("method"), request.get("params", {})
    if method == "initialize":
        if mode == "oversized": print("x"*300000+secret,flush=True); continue
        if mode == "malformed": print(secret,flush=True); continue
        if mode == "timeout": time.sleep(30)
        if mode == "rpc-error":
            emit({"jsonrpc":"2.0","id":request["id"],"error":{"code":-32000,"message":secret}}); continue
        reply(request["id"], {"protocolVersion":1,"authMethods":[],
              "agentCapabilities":{"loadSession":True,"mcpCapabilities":{"http":True}},
              "agentInfo":{"name":"tutor-local-agent","version":"0.1.0"}})
    elif method == "session/new":
        assert len(params["mcpServers"]) == 1 and params["_meta"]["tutorLocal"]["owner"]
        url = params["mcpServers"][0]["url"]
        assert url.startswith("http://127.0.0.1:")
        reply(request["id"], {"sessionId":session,"configOptions":[{"id":"model",
              "currentValue":"qwen3-4b-instruct-2507-q4_k_m"}]})
    elif method == "session/prompt":
        pending = request["id"]
        if mode in ("cancel", "ignore-cancel"): continue
        create_journal(params["_meta"]["tutorLocal"]["task"])
        prompt = params["prompt"][0]["text"]
        assert "17" not in prompt and "42" not in prompt
        raw = encode({"user":prompt})
        db.execute("INSERT INTO turns VALUES(?,?,?)", (turn,raw,hashlib.sha256(raw).digest()))
        mcp("initialize", {"protocolVersion":"2025-06-18","capabilities":{}}, 1)
        mcp("notifications/initialized", {}, None)
        tools = mcp("tools/list", {}, 2)["tools"]
        assert len(tools) == 3
        contract = next(t for t in tools if t["name"] == tool)
        event("harness_search", {"query":prompt,"tools":[{"id":"tutor/"+t["name"],"name":t["name"]} for t in tools]})
        if mode in ("no-permission", "partial-no-permission", "partial-crash", "partial-timeout", "diagnostic-flood"):
            if mode == "no-permission":
                action({"action":"answer","private":secret})
            else:
                project()
                event("harness_model_action", {"raw":'{"action":"call","private":"'+secret,
                      "complete":False,"stop_reason":"max_tokens","rejected_chunk_bytes":0})
                event("harness_action_status", {"code":"action_incomplete","private":secret})
                if mode in ("partial-no-permission", "diagnostic-flood"):
                    for _ in range(64 if mode == "diagnostic-flood" else 1):
                        event("harness_model_action", {"raw":encode({"action":secret,"arguments":{"private":secret}}).decode(),
                              "complete":True,"stop_reason":secret,"rejected_chunk_bytes":0})
                    event("harness_action_status", {"code":"action_incomplete_"+secret})
                    event("context_projection", {"input_tokens":secret,"output_reserved":{"private":secret},"kept_ids":[secret]})
            if mode in ("partial-crash", "partial-timeout"):
                db.commit()
                if mode == "partial-crash": db.close(); db = None; sys.exit(7)
                continue
            project()
            answer = "La lecture des alertes n’a pas abouti. " + secret
            event("assistant_chunk", {"text":answer})
            event("turn_finished", "complete")
            db.commit()
            db.close()
            db = None
            update({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":answer}})
            reply(pending, {"stopReason":"end_turn","_meta":{"tutor":{"generation":{
                  "inputTokens":projections*100,"outputTokens":projections*12,"elapsedMs":20}}}})
            continue
        action({"action":"describe","id":tool_id})
        event("harness_selected_contract", {"id":tool_id,"description":contract["description"],"input_schema":contract["inputSchema"]})
        action({"action":"call","id":tool_id,"arguments":{}})
        db.execute("INSERT INTO operations VALUES(?,?,?,?,?,?,?)", (operation,turn,tool_id,encode(arguments),None,0,"prepared"))
        transition(None,"prepared")
        db.commit()
        tool_update("pending", True)
        raw_input = {"server":"tutor","tool":tool,"arguments":arguments}
        if mode == "wrong-permission-tool": raw_input["tool"] = "delete_domain"
        if mode == "wrong-permission-args": raw_input["arguments"] = {"domain_id":secret}
        emit({"jsonrpc":"2.0","method":"session/request_permission","id":"permission:"+operation,
              "params":{"sessionId":session,"_meta":{"is_mcp_tool_approval":True},
              "toolCall":{"toolCallId":operation,"title":tool,"kind":"other","status":"pending","rawInput":raw_input},
              "options":[{"optionId":"allow","kind":"allow_once","name":"Autoriser"},
                         {"optionId":"reject","kind":"reject_once","name":"Refuser"}]}})
    elif method == "session/cancel":
        if mode == "ignore-cancel": continue
        reply(pending, {"stopReason":"cancelled"})
    elif method is None:
        assert request["id"] == "permission:"+operation and set(request) == {"jsonrpc","id","result"}
        finish(request["result"]["outcome"] == {"outcome":"selected","optionId":"allow"})
if db: db.close()
if mode == "ignore-eof": time.sleep(30)
'''


class HarnessQualificationTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="harness-protocol-test-")
        self.root = Path(self.temporary.name)
        self.fixture = self.root / "agent.py"
        self.fixture.write_text(FIXTURE, encoding="utf-8")
        self.model = self.root / "fixture.gguf"
        self.model.write_bytes(struct.pack("<4sIQQ", b"GGUF", 3, 1, 1) + b"protocol-only")
        self.options = runner.parse_options([
            "--agent", str(Path(sys.executable).resolve()), "--model", str(self.model),
            "--output", str(self.root / "report.json"), "--timeout", "8",
            "--cancel-timeout", "0.25", "--shutdown-timeout", "0.25"])

    def tearDown(self):
        self.temporary.cleanup()

    async def qualify(self, mode="normal", permission="allow"):
        report = await runner.qualify_protocol(self.options,
            [sys.executable, "-B", "-u", str(self.fixture), mode, str(self.model)], permission=permission)
        self.assertEqual(report["scope"], "protocol_only")
        self.assertFalse(report["qualified"])
        self.assertEqual(report["inference_status"], "not_tested")
        for flag in ("target_hardware_qualified", "real_tutor_qualified", "pedagogy_qualified"):
            self.assertFalse(report[flag])
        encoded = json.dumps(report)
        for private in (SECRET, str(self.root), "127.0.0.1:", runner.PROMPT,
                        "Révision prévue", "\"raw\":", "\"arguments\":"):
            self.assertNotIn(private, encoded)
        return report

    async def test_allow_real_http_and_exact_journal_but_never_inference(self):
        report = await self.qualify()
        self.assertTrue(report["protocol_passed"], report)
        self.assertEqual(report["metrics"]["mcp"]["tool_calls"], 1)
        self.assertEqual(report["metrics"]["journal"]["exact_result_receipts"], 1)
        self.assertEqual(report["metrics"]["journal"]["explicit_result_reads"], 1)
        self.assertTrue(report["metrics"]["journal"]["final_context_contains_selected_fact"])
        self.assertTrue(report["metrics"]["prompt"]["grounded_numeric_fact"])
        self.assertEqual(report["artifacts"]["catalog"]["served_tool_count"], 3)
        self.assertGreater(report["artifacts"]["catalog"]["source_tool_count"], 3)
        self.assertEqual(report["artifacts"]["model"]["sha256"], hashlib.sha256(self.model.read_bytes()).hexdigest())
        self.assertFalse(report["artifacts"]["model"]["pinned_identity_verified"])
        self.assertEqual(report["host"]["requested_compute"], "cpu")
        self.assertTrue(report["shutdown"]["graceful_eof"])
        self.assertTrue(report["shutdown"]["reaped"])
        times = report["metrics"]["milestone_ms"]
        self.assertLessEqual(times["mcp_catalog"], times["permission_response"])
        self.assertLessEqual(times["permission_response"], times["mcp_dispatch"])
        runner.base.write_report(report, self.options.output)
        self.assertEqual(json.loads(self.options.output.read_text()), report)

    async def test_deny_has_no_dispatch_receipt_or_selected_result(self):
        report = await self.qualify(permission="deny")
        self.assertTrue(report["protocol_passed"], report)
        self.assertEqual(report["metrics"]["mcp"]["tool_calls"], 0)
        self.assertEqual(report["metrics"]["journal"]["exact_result_receipts"], 0)
        self.assertEqual(report["metrics"]["journal"]["explicit_result_reads"], 0)
        self.assertFalse(report["metrics"]["prompt"]["grounded_numeric_fact"])

    async def test_failed_prompt_without_permission_retains_only_safe_diagnostics(self):
        for mode in ("no-permission", "partial-no-permission"):
            with self.subTest(mode=mode):
                report = await self.qualify(mode)
                self.assertIn("permission_not_observed", report["errors"])
                self.assertEqual(report["validations"]["prompt"], "failed")
                self.assertEqual(report["validations"]["journal"], "not_run")
                self.assertFalse(report["protocol_passed"])
                self.assertEqual(report["metrics"]["mcp"]["tool_calls"], 0)
                diagnostic = report["diagnostics"]["journal"]
                self.assertEqual(diagnostic["status"], "observed")
                self.assertEqual(diagnostic["turn_completions"], ["complete"])
                self.assertFalse(diagnostic["truncated"])
                if mode == "no-permission":
                    self.assertEqual(diagnostic["actions"], [{"action_type":"answer", "complete":True, "stop_reason":None}])
                    self.assertEqual(diagnostic["counts"]["context_projections"], 2)
                    self.assertEqual(diagnostic["projections"], [{"input_tokens":100,"output_reserved":256}] * 2)
                else:
                    self.assertEqual(diagnostic["actions"], [
                        {"action_type":"invalid","complete":False,"stop_reason":"max_tokens"},
                        {"action_type":"invalid","complete":True,"stop_reason":"invalid"}])
                    self.assertEqual(diagnostic["action_statuses"], ["action_incomplete", "invalid"])
                    self.assertEqual(diagnostic["projections"][1], {"input_tokens":None,"output_reserved":None})
                # Round-trip the artifact as well: no secret may be serialized.
                runner.base.write_report(report, self.options.output)
                self.assertNotIn(SECRET, self.options.output.read_text())

    async def test_partial_journal_diagnosed_after_crash_or_expired_deadline(self):
        for mode in ("partial-crash", "partial-timeout"):
            with self.subTest(mode=mode):
                self.options.timeout = 1 if mode == "partial-timeout" else 8
                report = await self.qualify(mode)
                self.assertFalse(report["protocol_passed"])
                self.assertTrue(report["shutdown"]["reaped"])
                diagnostic = report["diagnostics"]["journal"]
                self.assertEqual(diagnostic["status"], "observed")
                self.assertEqual(diagnostic["actions"], [
                    {"action_type":"invalid","complete":False,"stop_reason":"max_tokens"}])
                self.assertEqual(diagnostic["action_statuses"], ["action_incomplete"])
                self.assertEqual(diagnostic["turn_completions"], [])
                if mode == "partial-crash":
                    self.assertEqual(report["shutdown"]["exit_code"], 7)
                    self.assertEqual(report["validations"]["shutdown"], "failed")
                else:
                    self.assertIn("qualification_timeout", report["errors"])
                    self.assertTrue(report["cancellation"]["cancelled_reply_observed"])

    async def test_diagnostic_event_limit_is_explicit_and_does_not_leak(self):
        report = await self.qualify("diagnostic-flood")
        self.assertFalse(report["protocol_passed"])
        diagnostic = report["diagnostics"]["journal"]
        self.assertEqual(diagnostic["status"], "observed")
        self.assertTrue(diagnostic["truncated"])
        self.assertEqual(diagnostic["counts"]["model_actions"], 65)
        self.assertLessEqual(sum(len(diagnostic[key]) for key in (
            "actions", "projections", "action_statuses", "turn_completions")), runner.MAX_DIAGNOSTIC_EVENTS)

    async def test_answer_without_read_or_final_evidence_cannot_pass(self):
        for mode, error in (("unread-reference", "journal_explicit_read_missing"),
                            ("omitted-selection", "journal_final_context_did_not_read_fact"),
                            ("wrong-receipt", "journal_receipt_mismatch"),
                            ("wrong-answer", "answer_fact_mismatch")):
            with self.subTest(mode=mode):
                report = await self.qualify(mode)
                self.assertFalse(report["protocol_passed"])
                self.assertIn(error, report["errors"])
                self.assertTrue(report["shutdown"]["reaped"])

    async def test_permission_checks_business_name_and_exact_host_arguments(self):
        for mode in ("wrong-permission-tool", "wrong-permission-args"):
            with self.subTest(mode=mode):
                report = await self.qualify(mode)
                self.assertIn("unexpected_permission_tool_or_arguments", report["errors"])
                self.assertEqual(report["metrics"]["mcp"]["tool_calls"], 0)
                self.assertTrue(report["cancellation"]["cancelled_reply_observed"])
                self.assertTrue(report["shutdown"]["reaped"])

    async def test_invalid_journal_or_aggregate_token_metadata_fails(self):
        for mode, error in (("bad-event-hash", "journal_event_hash_mismatch"),
                            ("context-overflow", "journal_context_budget_exceeded"),
                            ("bad-stats", "generation_metadata_missing")):
            with self.subTest(mode=mode):
                report = await self.qualify(mode)
                self.assertIn(error, report["errors"])
                self.assertFalse(report["protocol_passed"])

    async def test_protocol_frame_bounds_and_raw_errors_do_not_leak(self):
        for mode, error in (("oversized", "frame_too_large"), ("malformed", "invalid_json_frame"),
                            ("rpc-error", "agent_rpc_error")):
            with self.subTest(mode=mode):
                report = await self.qualify(mode)
                self.assertIn(error, report["errors"])
                self.assertTrue(report["shutdown"]["reaped"])

    async def test_repeated_call_and_denial_hallucination_fail(self):
        report = await self.qualify("duplicate-call")
        self.assertFalse(report["protocol_passed"])
        self.assertIn("fixture_protocol_failed", report["errors"])
        self.assertEqual(report["metrics"]["mcp"]["tool_calls"], 2)
        report = await self.qualify("denied-hallucination", permission="deny")
        self.assertIn("answer_fact_mismatch", report["errors"])

    async def test_timeout_sends_cancel_and_reaps(self):
        self.options.timeout = 0.5
        for mode, cancelled in (("cancel", True), ("ignore-cancel", False)):
            with self.subTest(mode=mode):
                started = time.monotonic()
                report = await self.qualify(mode)
                self.assertLess(time.monotonic() - started, 3)
                self.assertIn("qualification_timeout", report["errors"])
                self.assertEqual(report["cancellation"]["cancelled_reply_observed"], cancelled)
                self.assertTrue(report["shutdown"]["reaped"])
                self.assertFalse(report["protocol_passed"])

    async def test_ignored_eof_and_initialization_timeout_force_bounded_reap(self):
        for mode in ("ignore-eof", "timeout"):
            with self.subTest(mode=mode):
                self.options.timeout = 8 if mode == "ignore-eof" else 0.5
                started = time.monotonic()
                report = await self.qualify(mode)
                self.assertLess(time.monotonic() - started, 3)
                self.assertFalse(report["protocol_passed"])
                self.assertTrue(report["shutdown"]["forced"])
                self.assertTrue(report["shutdown"]["reaped"])

    async def test_live_pin_cannot_be_replaced_by_a_fixture_digest(self):
        self.options.model_sha256 = hashlib.sha256(self.model.read_bytes()).hexdigest()
        with mock.patch.object(asyncio, "create_subprocess_exec") as spawn:
            report = await runner.qualify(self.options)
        spawn.assert_not_called()
        self.assertIn("model_sha256_conflicts_with_pin", report["errors"])
        self.assertFalse(report["qualified"])

    async def test_missing_pin_rejects_live_before_launch(self):
        with mock.patch.object(runner.base, "PINNED_MODEL_PROFILE", self.root / "absent.json"), \
                mock.patch.object(asyncio, "create_subprocess_exec") as spawn:
            report = await runner.qualify(self.options)
        spawn.assert_not_called()
        self.assertIn("pinned_model_profile_unavailable", report["errors"])

    async def test_gpu_setting_is_requested_not_a_qualification(self):
        self.options.gpu_layers = 8
        report = await self.qualify()
        self.assertTrue(report["protocol_passed"], report)
        self.assertEqual(report["host"]["requested_compute"], "gpu_offload")
        self.assertFalse(report["host"]["gpu_execution_verified"])

    def test_output_guards_include_sources_symlinks_and_hardlinks(self):
        for path in (self.model, runner.CATALOG, Path(runner.__file__), Path(runner.base.__file__)):
            self.options.output = path
            with self.assertRaisesRegex(runner.QualificationError, "report_overwrites_input"):
                runner.guard_output(self.options)
        alias = self.root / "alias.json"
        os.link(self.model, alias)
        self.options.output = alias
        with self.assertRaisesRegex(runner.QualificationError, "report_overwrites_input"):
            runner.guard_output(self.options)

    def test_timeouts_are_bounded_and_help_describes_this_qualification(self):
        for duration in (0, 3601, float("inf"), float("nan")):
            self.options.timeout = duration
            with self.assertRaises(runner.QualificationError):
                runner.base.validate_options(self.options)
        output = io.StringIO()
        with redirect_stdout(output), self.assertRaises(SystemExit) as exit_status:
            runner.parse_options(["--help"])
        self.assertEqual(exit_status.exception.code, 0)
        self.assertIn("three", output.getvalue().lower().replace("3", "three"))
        self.assertIn("read_result", output.getvalue())
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            runner.parse_options(["--timeout", "nan"])

    async def test_fixture_does_not_serve_unapproved_or_wrong_calls(self):
        fixture = runner.Fixture([], time.monotonic() + 2)
        fixture.ready = True
        fixture.lists = 1
        with self.assertRaisesRegex(runner.QualificationError, "fixture_dispatch_without_unique_approval"):
            fixture.dispatch({"jsonrpc":"2.0","id":1,"method":"tools/call",
                              "params":{"name":runner.TOOL,"arguments":runner.ARGUMENTS}})
        fixture = runner.Fixture([], time.monotonic() + 2)
        fixture.ready = fixture.approved = True
        fixture.lists = 1
        with self.assertRaisesRegex(runner.QualificationError, "fixture_unexpected_call"):
            fixture.dispatch({"jsonrpc":"2.0","id":1,"method":"tools/call",
                              "params":{"name":"delete_domain","arguments":runner.ARGUMENTS}})


if __name__ == "__main__":
    unittest.main()
