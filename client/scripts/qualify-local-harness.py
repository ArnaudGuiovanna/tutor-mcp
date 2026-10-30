#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Bounded native ACP -> MCP qualification, standard library only; no downloads.

Run after building the native agent, for example:
  python3 client/scripts/qualify-local-harness.py \
    --agent /absolute/tutor-local-agent --model /absolute/model.gguf \
    --output /absolute/harness-report.json --timeout 1800 --max-output 256

The mandatory repository GGUF pin and native executable checks are reused from
qualify-local-agent.py. One public French prompt uses a fresh durable session and
a loopback-only controlled HTTP MCP fixture: get_pending_alerts plus two real
catalogue distractors. This is deliberately NOT the full 68-tool discovery test.
Only the expected, read-only business call with exact host-bound arguments can
be approved. No real Tutor server is contacted. After process shutdown, the
SQLite journal must prove discovery, describe, one call, an explicit read_result
and inclusion of that exact selection in the final model context. The final
answer must contain the fixture's numerical fact; an unread reference cannot
pass. The synthetic alert's unpredictable marker exists only in its MCP result.

Reports contain allowlisted hashes, counts, timings and booleans, never raw
prompts, model actions, results, credentials, identifiers or private paths.
Evidence is behavioral, not independent attestation of the executable. CPU/GPU
settings describe requested execution, not measured GPU use. A pass qualifies
only this narrow fixture on the tested host, NOT real Tutor, pedagogical quality,
P2000 performance, installation or the full catalogue. Protocol fixtures enter
via qualify_protocol(); they can never qualify inference. --timeout (<=3600s)
covers hashing and the test; bounded cancellation/shutdown grace is additional.
After the child is reaped, even a failed prompt gets a separate, at-most-two-
second journal diagnostic pass before cleanup. Diagnostics are observations,
never qualification evidence: only fixed action/status enums, completion flags
and bounded counters are exported. Raw actions and all other payloads stay local.
"""

import asyncio
from contextlib import closing
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sqlite3
import sys
import tempfile
import time
import uuid


# Loading the shared helper must not create an unrelated __pycache__ artifact.
_spec = importlib.util.spec_from_file_location(
    "_local_agent_qualification", Path(__file__).with_name("qualify-local-agent.py"))
base = importlib.util.module_from_spec(_spec)
_bytecode = sys.dont_write_bytecode
try:
    sys.dont_write_bytecode = True
    _spec.loader.exec_module(base)
finally:
    sys.dont_write_bytecode = _bytecode

require = base.require
QualificationError = base.QualificationError
CATALOG = Path(__file__).resolve().parents[1] / "local-agent/fixtures/catalog-local.json"
TOOL = "get_pending_alerts"
TOOL_ID = "tutor/" + TOOL
DISTRACTORS = ("get_availability_model", "get_goal_relevance")
DOMAIN = "qualification-domain"
ARGUMENTS = {"domain_id": DOMAIN}
PROMPT = "Quelles sont mes alertes en attente ? Indique le message et l’heure de la révision."
FACT = re.compile(r"(?<!\d)17\s*(?:h(?:eures?)?|:)\s*42(?!\d)", re.IGNORECASE)
MAX_HTTP_HEADER = 16 * 1024
MAX_HTTP_BODY = 128 * 1024
MAX_HTTP_REQUESTS = 32
MAX_JOURNAL = 16 * 1024 * 1024
MAX_EVENTS = 512
MAX_DIAGNOSTIC_EVENTS = 32
MAX_DIAGNOSTIC_PAYLOAD = 192 * 1024
DIAGNOSTIC_SECONDS = 2
ACTION_TYPES = frozenset(("search", "describe", "call", "read_result", "list_servers", "answer"))
STOP_REASONS = frozenset(("end_turn", "max_tokens", "cancelled"))
# Explicit native codes from harness.rs, call_validation.rs and executor.rs.
# Never accept a prefix or copy an unknown code into a report.
ACTION_STATUS_CODES = frozenset((
    "action_not_allowed_in_phase",
    "action_too_large", "action_invalid", "action_incomplete", "query_invalid",
    "tool_id_invalid", "arguments_invalid", "result_pointer_invalid", "describe_required",
    "tool_not_found", "result_read_limit", "result_reference_unknown", "tool_result_failed",
    "result_payload_unavailable", "result_pointer_missing", "result_selection_too_large",
    "tool_call_limit", "mutations_disabled_in_read_only_pilot", "tool_call_already_attempted",
    "tool_operation_already_completed", "tool_binding_changed", "tool_not_in_live_catalog",
    "tool_contract_not_supported", "tool_schema_not_supported", "tool_arguments_invalid",
    "tool_field_needs_verified_evidence", "tool_field_conflicts_with_verified_evidence",
    "tool_evidence_invalid", "tool_evidence_unavailable", "tool_preconditions_not_proven",
    "tool_journal_unavailable", "tool_permission_denied", "tool_cancelled_before_dispatch",
    "tool_reconciliation_required", "tool_outcome_unknown",
))


def decode_json(encoded):
    try:
        return json.loads(encoded, object_pairs_hook=base.unique_object,
                          parse_constant=base.reject_constant)
    except (ValueError, UnicodeError, RecursionError):
        raise QualificationError("invalid_json") from None


def load_catalog(deadline):
    require(CATALOG.stat().st_size <= 4 * 1024 * 1024, "catalog_too_large")
    document = decode_json(CATALOG.read_bytes())
    require(isinstance(document, dict) and isinstance(document.get("tools"), list),
            "invalid_catalog")
    selected = []
    for name in (TOOL, *DISTRACTORS):
        matches = [t for t in document["tools"] if isinstance(t, dict) and t.get("name") == name]
        require(len(matches) == 1, "catalog_tool_missing_or_duplicate")
        entry = matches[0]
        require(isinstance(entry.get("inputSchema"), dict)
                and entry.get("annotations", {}).get("readOnlyHint") is True,
                "unexpected_catalog_contract")
        selected.append(entry)
    schema = selected[0]["inputSchema"]
    require(schema.get("type") == "object" and schema.get("additionalProperties") is False
            and schema.get("properties", {}).get("domain_id", {}).get("type") == "string"
            and set(schema.get("required", [])) <= {"domain_id"}, "unexpected_alert_schema")
    encoded = json.dumps(selected, sort_keys=True, ensure_ascii=False,
                         separators=(",", ":")).encode("utf-8")
    return selected, {"source": base.fingerprint(CATALOG, deadline),
                      "selection_sha256": hashlib.sha256(encoded).hexdigest(),
                      "source_tool_count": len(document["tools"]), "served_tool_count": 3}


def guard_output(options):
    protected = base.protected_inputs(options) | {
        CATALOG.resolve(), Path(__file__).resolve(),
        Path(__file__).with_name("test_qualify_local_harness.py").resolve(),
        Path(base.__file__).resolve(),
    }
    require(options.output.resolve() not in protected, "report_overwrites_input")
    if options.output.exists():
        require(not any(p.exists() and os.path.samefile(options.output, p) for p in protected),
                "report_overwrites_input")


class Fixture:
    """Strict bounded HTTP/1.1 fixture; no external URLs, redirects or logs."""

    def __init__(self, tools, deadline):
        self.tools, self.deadline = tools, deadline
        self.server = None
        self.tasks = set()
        self.writers = set()
        self.requests = 0
        self.bytes = 0
        self.calls = 0
        self.initialized = False
        self.ready = False
        self.lists = 0
        self.approved = False
        self.errors = []
        self.times = {}
        self.marker = "qualification-" + uuid.uuid4().hex
        self.result = {"content": [], "structuredContent": {
            "alerts": [{"message": "Révision prévue à 17 h 42. Code : " + self.marker}],
            "has_critical": False, "needs_domain_setup": False}, "isError": False}

    async def start(self):
        self.server = await asyncio.start_server(self.handle, "127.0.0.1", 0,
                                                limit=MAX_HTTP_HEADER)
        port = self.server.sockets[0].getsockname()[1]
        self.url = f"http://127.0.0.1:{port}/mcp"

    async def close(self):
        if self.server is not None:
            self.server.close()
            await self.server.wait_closed()
        for writer in tuple(self.writers):
            writer.close()
        for task in tuple(self.tasks):
            task.cancel()
        await asyncio.gather(*tuple(self.tasks), return_exceptions=True)

    def dispatch(self, message):
        require(isinstance(message, dict) and message.get("jsonrpc") == "2.0",
                "fixture_invalid_rpc")
        method, params = message.get("method"), message.get("params", {})
        require(isinstance(params, dict), "fixture_invalid_params")
        identifier = message.get("id")
        if method == "notifications/initialized":
            require("id" not in message and self.initialized and not self.ready,
                    "fixture_initialization_order")
            self.ready = True
            return None
        require(type(identifier) in (int, str), "fixture_invalid_rpc_id")
        if method == "initialize":
            require(not self.initialized and params.get("protocolVersion") == "2025-06-18",
                    "fixture_initialization_order")
            self.initialized = True
            self.times["mcp_initialize"] = time.monotonic()
            result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}},
                      "serverInfo": {"name": "tutor", "version": "qualification-fixture-1"}}
        elif method == "tools/list":
            require(self.ready and not params, "fixture_catalog_request_invalid")
            self.lists += 1
            self.times.setdefault("mcp_catalog", time.monotonic())
            result = {"tools": self.tools}
        elif method == "tools/call":
            # A model-selected distractor or a second call is never served.
            self.calls += 1
            require(self.ready and self.lists > 0 and self.approved and self.calls == 1,
                    "fixture_dispatch_without_unique_approval")
            require(set(params) == {"name", "arguments"} and params["name"] == TOOL
                    and params["arguments"] == ARGUMENTS, "fixture_unexpected_call")
            self.times["mcp_dispatch"] = time.monotonic()
            result = self.result
        else:
            raise QualificationError("fixture_unexpected_method")
        return {"jsonrpc": "2.0", "id": identifier, "result": result}

    async def handle(self, reader, writer):
        task = asyncio.current_task()
        self.tasks.add(task)
        self.writers.add(writer)
        try:
            require(len(self.tasks) <= 4, "fixture_connection_limit")
            deadline = min(self.deadline, time.monotonic() + 10)
            header = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), base.remaining(deadline))
            require(len(header) <= MAX_HTTP_HEADER, "fixture_header_too_large")
            lines = header.decode("ascii").split("\r\n")
            require(lines[0] == "POST /mcp HTTP/1.1", "fixture_unexpected_http_method")
            headers = {}
            for line in lines[1:-2]:
                key, sep, value = line.partition(":")
                key = key.lower()
                require(sep and key not in headers, "fixture_invalid_headers")
                headers[key] = value.strip()
            require("transfer-encoding" not in headers, "fixture_invalid_headers")
            length = headers.get("content-length", "")
            require(re.fullmatch(r"[0-9]{1,6}", length) is not None, "fixture_invalid_length")
            length = int(length)
            require(0 < length <= MAX_HTTP_BODY, "fixture_body_too_large")
            self.requests += 1
            self.bytes += len(header) + length
            require(self.requests <= MAX_HTTP_REQUESTS and self.bytes <= base.MAX_WIRE_BYTES,
                    "fixture_wire_budget_exceeded")
            body = await asyncio.wait_for(reader.readexactly(length), base.remaining(deadline))
            result = self.dispatch(decode_json(body))
            encoded = b"" if result is None else json.dumps(result, ensure_ascii=False).encode("utf-8")
            require(len(encoded) <= MAX_HTTP_BODY, "fixture_response_too_large")
            status = "202 Accepted" if result is None else "200 OK"
            writer.write((f"HTTP/1.1 {status}\r\nContent-Type: application/json\r\n"
                          f"Content-Length: {len(encoded)}\r\nConnection: close\r\n\r\n").encode("ascii")
                         + encoded)
            await asyncio.wait_for(writer.drain(), base.remaining(deadline))
        except QualificationError as error:
            self.errors.append(error.args[0])
        except (OSError, ValueError, UnicodeError, asyncio.IncompleteReadError,
                asyncio.LimitOverrunError, asyncio.TimeoutError):
            self.errors.append("fixture_http_error")
        finally:
            writer.close()
            try:
                await asyncio.wait_for(writer.wait_closed(), 1)
            except (OSError, asyncio.TimeoutError):
                pass
            self.writers.discard(writer)
            self.tasks.discard(task)


async def permission_response(wire, message, session_id, fixture, allowed, deadline):
    """Wire.send constructs requests; permission replies are exact RPC responses."""
    require(message.get("method") == "session/request_permission"
            and type(message.get("id")) is str and 0 < len(message["id"]) <= 128
            and "result" not in message and "error" not in message, "invalid_permission_request")
    params = message.get("params")
    require(isinstance(params, dict) and params.get("sessionId") == session_id,
            "session_id_mismatch")
    tool = params.get("toolCall")
    require(isinstance(tool, dict) and tool.get("title") == TOOL
            and tool.get("rawInput") == {"server": "tutor", "tool": TOOL, "arguments": ARGUMENTS}
            and tool.get("status") == "pending" and tool.get("kind") == "other"
            and params.get("_meta", {}).get("is_mcp_tool_approval") is True,
            "unexpected_permission_tool_or_arguments")
    call_id = tool.get("toolCallId")
    require(isinstance(call_id, str) and 0 < len(call_id) <= 96
            and message["id"] == "permission:" + call_id, "invalid_permission_call_id")
    choices = params.get("options")
    require(isinstance(choices, list) and len(choices) == 2
            and all(isinstance(c, dict) for c in choices)
            and {(c.get("optionId"), c.get("kind")) for c in choices}
            == {("allow", "allow_once"), ("reject", "reject_once")}, "invalid_permission_options")
    require(fixture.calls == 0 and not fixture.approved, "dispatch_before_permission")
    frame = {"jsonrpc": "2.0", "id": message["id"], "result": {
        "outcome": {"outcome": "selected", "optionId": "allow" if allowed else "reject"}}}
    encoded = json.dumps(frame).encode("utf-8") + b"\n"
    require(len(encoded) <= base.MAX_FRAME, "outgoing_frame_too_large")
    fixture.times["permission_response"] = time.monotonic()
    fixture.approved = allowed
    wire.process.stdin.write(encoded)
    await asyncio.wait_for(wire.process.stdin.drain(), base.remaining(deadline))
    return call_id


async def run_prompt(wire, session_id, host_context, fixture, options, allowed, deadline, progress):
    started = time.monotonic()
    progress.update({"prompt_started": started, "active": True})
    await wire.send("session/prompt", {"sessionId": session_id,
                    "prompt": [{"type": "text", "text": PROMPT}],
                    "_meta": {"tutorLocal": {"task": host_context}}}, deadline, 3)
    pieces, statuses = [], []
    text_bytes, permissions, call_id, first_text = 0, 0, None, None
    while True:
        message = await wire.receive(deadline)
        method = message.get("method")
        if method == "session/request_permission":
            permissions += 1
            require(permissions == 1, "multiple_permission_requests")
            call_id = await permission_response(wire, message, session_id, fixture, allowed, deadline)
        elif method == "session/update":
            require("id" not in message, "unexpected_agent_method")
            params = message.get("params")
            require(isinstance(params, dict) and params.get("sessionId") == session_id,
                    "session_id_mismatch")
            update = params.get("update")
            require(isinstance(update, dict), "invalid_session_update")
            kind = update.get("sessionUpdate")
            if kind == "agent_message_chunk":
                content = update.get("content")
                require(isinstance(content, dict) and content.get("type") == "text"
                        and isinstance(content.get("text"), str), "invalid_text_update")
                text = content["text"]
                text_bytes += len(text.encode("utf-8"))
                require(text_bytes <= base.MAX_TEXT, "text_budget_exceeded")
                if text and first_text is None:
                    first_text = time.monotonic()
                pieces.append(text)
            elif kind in ("tool_call", "tool_call_update"):
                require(len(statuses) < 8 and update.get("title") == TOOL,
                        "unexpected_tool_update")
                require(update.get("status") in ("pending", "in_progress", "completed", "failed")
                        and isinstance(update.get("toolCallId"), str), "invalid_tool_update")
                statuses.append((kind, update["toolCallId"], update["status"]))
            else:
                raise QualificationError("unexpected_update_type")
        elif method is not None:
            raise QualificationError("unexpected_agent_method")
        else:
            require(type(message.get("id")) is int and message["id"] == 3,
                    "response_id_mismatch")
            progress["active"] = False
            require("error" not in message and isinstance(message.get("result"), dict),
                    "agent_rpc_error")
            result = message["result"]
            require(result.get("stopReason") == "end_turn", "turn_not_completed")
            break
    require(permissions == 1 and call_id is not None, "permission_not_observed")
    expected = [("tool_call", call_id, "pending")]
    if allowed:
        expected += [("tool_call_update", call_id, "in_progress")]
    expected += [("tool_call_update", call_id, "completed" if allowed else "failed")]
    require(statuses == expected, "tool_update_sequence_mismatch")
    answer = "".join(pieces)
    require(bool(answer.strip()), "empty_final_answer")
    require(bool(FACT.search(answer)) if allowed else not FACT.search(answer), "answer_fact_mismatch")
    metrics = {"elapsed_ms": round((time.monotonic() - started) * 1000, 3),
               "first_text_ms": round((first_text - started) * 1000, 3) if first_text else None,
               "permission_count": permissions, "text_bytes": text_bytes,
               "chunks": len(pieces), "grounded_numeric_fact": bool(FACT.search(answer)),
               "tool_update_sequence_valid": True}
    return answer, call_id, result, metrics


def json_pointer(value, pointer):
    require(isinstance(pointer, str) and (pointer == "" or pointer.startswith("/")),
            "invalid_result_pointer")
    for part in pointer.split("/")[1:] if pointer else []:
        require(re.search(r"~(?![01])", part) is None, "invalid_result_pointer")
        key = part.replace("~1", "/").replace("~0", "~")
        if isinstance(value, dict):
            require(key in value, "result_pointer_missing")
            value = value[key]
        else:
            require(isinstance(value, list) and re.fullmatch(r"0|[1-9][0-9]*", key)
                    and len(key) <= 6 and int(key) < len(value), "result_pointer_missing")
            value = value[int(key)]
    return value


def diagnostic_enum(value, allowed):
    return value if isinstance(value, str) and value in allowed else "invalid"


def diagnostic_integer(value, maximum):
    return value if type(value) is int and 0 <= value <= maximum else None


def journal_diagnostics(data_dir, session_id):
    """Bounded best effort AFTER reaping; cannot change any validation outcome.

    Its independent deadline permits diagnostics after the qualification timeout.
    No user text, schemas, arguments, action raw text, identifiers, SQL errors or
    filenames are returned, including when the database/payload is malformed.
    """
    diagnostic = {"status": "unavailable", "reason": "journal_unavailable"}
    deadline = time.monotonic() + DIAGNOSTIC_SECONDS
    try:
        paths = data_dir.glob("*/*.sqlite3")
        path = next(paths, None)
        if path is None or next(paths, None) is not None:
            return diagnostic
        if (path.is_symlink() or path.parent.is_symlink() or path.stem != session_id
                or not path.resolve().is_relative_to(data_dir.resolve())
                or path.stat().st_size > MAX_JOURNAL):
            return diagnostic
        with closing(sqlite3.connect(path.as_uri() + "?mode=ro&immutable=1", uri=True, timeout=0.1)) as db:
            db.execute("PRAGMA query_only=ON")
            db.set_progress_handler(lambda: int(time.monotonic() >= deadline), 100)
            counts = {}
            for label, kind in (("model_actions", "harness_model_action"),
                                ("context_projections", "context_projection"),
                                ("action_statuses", "harness_action_status"),
                                ("turn_finishes", "turn_finished")):
                base.remaining(deadline)
                counts[label] = diagnostic_integer(db.execute(
                    "SELECT COUNT(*) FROM events WHERE kind=?", (kind,)).fetchone()[0], 1_000_000)
            rows = db.execute(
                "SELECT kind,length(payload),substr(payload,1,?),payload_sha256 FROM events "
                "WHERE kind IN ('harness_model_action','context_projection','harness_action_status',"
                "'turn_finished') ORDER BY sequence LIMIT ?",
                (MAX_DIAGNOSTIC_PAYLOAD + 1, MAX_DIAGNOSTIC_EVENTS + 1)).fetchall()
        diagnostic = {"status": "observed", "counts": counts, "actions": [],
                      "action_statuses": [], "projections": [], "turn_completions": [],
                      "truncated": len(rows) > MAX_DIAGNOSTIC_EVENTS, "invalid_payloads": 0}
        for kind, length, raw, digest in rows[:MAX_DIAGNOSTIC_EVENTS]:
            base.remaining(deadline)
            if type(length) is not int or length > MAX_DIAGNOSTIC_PAYLOAD:
                diagnostic["truncated"] = True
                diagnostic["invalid_payloads"] += 1
                continue
            if not isinstance(raw, bytes) or hashlib.sha256(raw).digest() != digest:
                diagnostic["invalid_payloads"] += 1
                continue
            try:
                value = decode_json(raw)
            except QualificationError:
                diagnostic["invalid_payloads"] += 1
                continue
            if kind == "turn_finished":
                diagnostic["turn_completions"].append(diagnostic_enum(value, ("complete", "interrupted", "failed")))
                continue
            if not isinstance(value, dict):
                diagnostic["invalid_payloads"] += 1
                continue
            if kind == "harness_model_action":
                action_type = "invalid"
                if isinstance(value.get("raw"), str):
                    try:
                        action = decode_json(value["raw"])
                        if isinstance(action, dict):
                            action_type = diagnostic_enum(action.get("action"), ACTION_TYPES)
                    except QualificationError:
                        pass  # Partial action: retain only the fixed invalid label.
                stop_recorded = "stop_reason" in value or "stopReason" in value
                diagnostic["actions"].append({
                    "action_type": action_type,
                    "complete": value.get("complete") if type(value.get("complete")) is bool else None,
                    "stop_reason": diagnostic_enum(value.get("stop_reason", value.get("stopReason")),
                                                   STOP_REASONS) if stop_recorded else None,
                })
            elif kind == "harness_action_status":
                diagnostic["action_statuses"].append(diagnostic_enum(value.get("code"), ACTION_STATUS_CODES))
            elif kind == "context_projection":
                diagnostic["projections"].append({
                    "input_tokens": diagnostic_integer(value.get("input_tokens"), 4096),
                    "output_reserved": diagnostic_integer(value.get("output_reserved"), 768),
                })
        return diagnostic
    except QualificationError:
        return {"status": "unavailable", "reason": "diagnostic_timeout"}
    except (sqlite3.Error, OSError, ValueError, TypeError, KeyError, RecursionError):
        return {"status": "unavailable", "reason": "journal_read_failed"}


def inspect_journal(data_dir, session_id, call_id, fixture, answer, result, options, allowed,
                    model_sha256, deadline):
    """Read only AFTER child reaping (the native journal holds an exclusive lock)."""
    paths = list(data_dir.glob("*/*.sqlite3"))
    require(len(paths) == 1, "durable_journal_missing_or_multiple")
    path = paths[0]
    require(not path.is_symlink() and path.stem == session_id
            and path.stat().st_size <= MAX_JOURNAL, "invalid_journal_file")
    # SQLite immutable/read-only avoids creating sidecars or changing the journal.
    with closing(sqlite3.connect(path.as_uri() + "?mode=ro&immutable=1", uri=True, timeout=1)) as db:
        db.execute("PRAGMA query_only=ON")
        db.set_progress_handler(lambda: int(time.monotonic() >= deadline), 1000)
        turns = db.execute("SELECT id,payload,payload_sha256 FROM turns LIMIT 2").fetchall()
        require(len(turns) == 1 and decode_json(turns[0][1]) == {"user": PROMPT}
                and hashlib.sha256(turns[0][1]).digest() == turns[0][2], "journal_user_mismatch")
        turn = turns[0][0]
        rows = db.execute("SELECT sequence,id,turn_id,kind,payload,payload_sha256 "
                          "FROM events ORDER BY sequence LIMIT ?", (MAX_EVENTS + 1,)).fetchall()
        require(0 < len(rows) <= MAX_EVENTS and sum(len(r[4]) for r in rows) <= 2 * 1024 * 1024,
                "journal_event_budget_exceeded")
        events = []
        for sequence, event_id, event_turn, kind, raw, digest in rows:
            require(hashlib.sha256(raw).digest() == digest, "journal_event_hash_mismatch")
            require(event_turn == turn or (event_turn is None and kind == "session_header"),
                    "journal_event_turn_mismatch")
            events.append((sequence, event_id, kind, decode_json(raw)))
        by_kind = lambda kind: [e for e in events if e[2] == kind]
        headers = by_kind("session_header")
        require(len(headers) == 1 and headers[0][3].get("session_id") == session_id
                and headers[0][3].get("model_sha256") == model_sha256,
                "journal_session_mismatch")
        require(len(by_kind("verified_host_context")) == 1, "journal_host_context_missing")
        require([e[3] for e in by_kind("turn_finished")] == ["complete"], "journal_turn_incomplete")
        require("".join(e[3]["text"] for e in by_kind("assistant_chunk")) == answer,
                "journal_answer_mismatch")
        searches = by_kind("harness_search")
        require(searches and all(isinstance(e[3].get("tools"), list) and len(e[3]["tools"]) <= 3
                                for e in searches), "journal_discovery_missing")
        require(any(t.get("id") == TOOL_ID for e in searches for t in e[3]["tools"]),
                "journal_expected_tool_not_discovered")
        contracts = by_kind("harness_selected_contract")
        require(len(contracts) == 1 and contracts[0][3].get("id") == TOOL_ID
                and contracts[0][3].get("input_schema") == fixture.tools[0]["inputSchema"],
                "journal_contract_mismatch")
        actions = []
        for e in by_kind("harness_model_action"):
            require(e[3].get("complete") is True and e[3].get("rejected_chunk_bytes") == 0,
                    "journal_incomplete_model_action")
            action = decode_json(e[3]["raw"])
            require(isinstance(action, dict) and action.get("action") in
                    ("search", "describe", "call", "read_result", "list_servers", "answer"),
                    "journal_invalid_model_action")
            actions.append((e[0], action))
        descriptions = [(seq, a) for seq, a in actions if a["action"] == "describe"]
        calls = [(seq, a) for seq, a in actions if a["action"] == "call"]
        require(1 <= len(actions) <= 7 and len(descriptions) == len(calls) == 1
                and descriptions[0][1].get("id") == calls[0][1].get("id") == TOOL_ID
                and searches[0][0] < descriptions[0][0] < contracts[0][0] < calls[0][0],
                "journal_action_sequence_mismatch")
        operations = db.execute("SELECT id,turn_id,tool,arguments,idempotency_key,mutating,state "
                                "FROM operations LIMIT 2").fetchall()
        require(len(operations) == 1, "journal_operation_count_mismatch")
        operation = operations[0]
        require(operation[:3] == (call_id, turn, TOOL_ID) and decode_json(operation[3]) == ARGUMENTS
                and operation[4:] == (None, 0, "completed" if allowed else "rejected"),
                "journal_operation_mismatch")
        transitions = db.execute("SELECT from_state,to_state FROM transitions WHERE operation_id=? "
                                 "ORDER BY sequence LIMIT 5", (call_id,)).fetchall()
        expected = [(None, "prepared"), ("prepared", "dispatched"), ("dispatched", "completed")] \
            if allowed else [(None, "prepared"), ("prepared", "rejected")]
        require(transitions == expected, "journal_dispatch_sequence_mismatch")
        receipts = db.execute("SELECT operation_id,payload FROM results LIMIT 2").fetchall()
        selections = by_kind("harness_result_selection")
        reads = [(seq, a) for seq, a in actions if a["action"] == "read_result"]
        if allowed:
            require(len(receipts) == 1 and receipts[0][0] == call_id
                    and decode_json(receipts[0][1]) == fixture.result, "journal_receipt_mismatch")
            require(reads and selections, "journal_explicit_read_missing")
            grounded = []
            for e in selections:
                selection = e[3]
                require(selection.get("operation_id") == call_id and selection.get("reference") == "result_1",
                        "journal_selection_reference_mismatch")
                pointer = selection.get("pointer")
                require(selection.get("value") == json_pointer(fixture.result["structuredContent"], pointer)
                        and any(calls[0][0] < seq < e[0] and a.get("reference") == "result_1"
                                and a.get("pointer") == pointer for seq, a in reads),
                        "journal_selection_mismatch")
                encoded = json.dumps(selection["value"], ensure_ascii=False)
                if FACT.search(encoded) and fixture.marker in encoded:
                    grounded.append(e)
            require(grounded, "journal_selected_fact_missing")
        else:
            require(not receipts and not selections and not reads, "denied_call_has_receipt_or_read")
            grounded = []
        projections = by_kind("context_projection")
        require(len(projections) == len(actions) + 1 and len(projections) <= 8,
                "journal_projection_count_mismatch")
        for e in projections:
            audit = e[3]
            require(type(audit.get("input_tokens")) is int
                    and 0 < audit["input_tokens"] <= 4096 - options.max_output - 256
                    and type(audit.get("output_reserved")) is int
                    and audit["output_reserved"] == options.max_output, "journal_context_budget_exceeded")
        if allowed:
            require(any(e[0] < projections[-1][0] and e[1] in projections[-1][3].get("kept_ids", [])
                        for e in grounded), "journal_final_context_did_not_read_fact")
        stats = result.get("_meta", {}).get("tutor", {}).get("generation", {})
        require(isinstance(stats, dict) and all(type(stats.get(k)) is int
                for k in ("inputTokens", "outputTokens", "elapsedMs")), "generation_metadata_missing")
        require(stats["inputTokens"] == sum(e[3]["input_tokens"] for e in projections)
                and 0 < stats["outputTokens"] <= len(projections) * options.max_output
                and 0 <= stats["elapsedMs"] <= 3_600_000, "generation_metadata_invalid")
        metrics = {"event_count": len(events), "model_decisions": len(actions),
                   "model_invocations": len(projections), "discovery_count": len(searches),
                   "described_tools": len(contracts), "explicit_result_reads": len(reads),
                   "operation_count": len(operations), "exact_result_receipts": len(receipts),
                   "max_input_tokens": max(e[3]["input_tokens"] for e in projections),
                   "input_tokens_sum": stats["inputTokens"], "output_tokens_sum": stats["outputTokens"],
                   "generation_elapsed_ms": stats["elapsedMs"], "context_budgets_valid": True,
                   "final_context_contains_selected_fact": bool(grounded)}
    return metrics, base.fingerprint(path, deadline)


def new_report(options, protocol_only, allowed):
    report = base.new_report(options, protocol_only)
    report.update({"qualification_scope": "controlled_three_tool_mcp_fixture_on_tested_host_only",
                   "real_tutor_qualified": False, "pedagogy_qualified": False,
                   "inference_status": "not_tested" if protocol_only else "not_observed",
                   "permission_policy": "allow_expected_read_once" if allowed else "deny_protocol_fixture",
                   "not_covered": ["real_tutor_server", "pedagogical_quality", "full_catalogue_discovery",
                                   "windows_p2000_acceptance", "gpu_utilization", "installation"],
                   "prompt_fixture": "public_fr_pending_alerts_v1",
                   "validations": {k: "not_run" for k in ("configuration", "artifacts", "fixture", "initialize",
                                                          "session", "prompt", "dispatch", "shutdown", "journal")}})
    report.pop("prompts", None)
    report["host"]["gpu_execution_verified"] = False
    report["host"]["scope"] = "execution_host_only"
    report["metrics"]["stage_ms"] = {}
    return report


async def cancel_active(wire, session_id, timeout):
    started = time.monotonic()
    deadline = started + timeout
    observed = False
    try:
        await wire.send("session/cancel", {"sessionId": session_id}, deadline)
        while True:
            message = await wire.receive(deadline)
            if message.get("id") == 3 and "method" not in message:
                observed = message.get("result", {}).get("stopReason") == "cancelled"
                break
    except (QualificationError, OSError, asyncio.TimeoutError, ValueError, TypeError):
        pass
    return {"requested": True, "cancelled_reply_observed": observed,
            "elapsed_ms": round((time.monotonic() - started) * 1000, 3)}


async def _qualify(options, protocol_command=None, allowed=True):
    protocol_only = protocol_command is not None
    report = new_report(options, protocol_only, allowed)
    started = stage_started = time.monotonic()
    deadline = started + options.timeout
    stage = "configuration"
    process = wire = fixture = temporary = session_id = data_dir = None
    progress = {}
    completed = None

    def next_stage(name):
        nonlocal stage, stage_started
        report["metrics"]["stage_ms"][stage] = round((time.monotonic() - stage_started) * 1000, 3)
        stage, stage_started = name, time.monotonic()

    try:
        base.validate_options(options)
        guard_output(options)
        report["validations"][stage] = "passed"
        next_stage("artifacts")
        report["artifacts"] = base.inspect_artifacts(options, deadline, protocol_only)
        report["artifacts"]["qualifier"] = base.fingerprint(Path(__file__), deadline)
        report["artifacts"]["shared_helper"] = base.fingerprint(Path(base.__file__), deadline)
        tools, catalog_identity = load_catalog(deadline)
        report["artifacts"]["catalog"] = catalog_identity
        report["validations"][stage] = "passed"
        next_stage("fixture")
        fixture = Fixture(tools, deadline)
        await fixture.start()
        temporary = tempfile.TemporaryDirectory(prefix="tutor-harness-qualification-")
        data_dir = Path(temporary.name) / "sessions"
        argv = base.command(options, report["artifacts"]["model"]["pinned_sha256"]) \
            if not protocol_only else list(protocol_command)
        argv += ["--data-dir", str(data_dir)]
        report["validations"][stage] = "passed"
        next_stage("initialize")
        process = await asyncio.wait_for(asyncio.create_subprocess_exec(
            *argv, stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL, cwd=temporary.name, limit=base.MAX_FRAME,
            start_new_session=os.name != "nt"), base.remaining(deadline))
        wire = base.Wire(process)
        handshake = min(deadline, time.monotonic() + 15)
        await wire.send("initialize", {"protocolVersion": 1, "clientCapabilities": {},
                        "clientInfo": {"name": "tutor-harness-qualification", "version": "1"}}, handshake, 1)
        initialized = await wire.response(1, handshake)
        require(type(initialized.get("protocolVersion")) is int and initialized["protocolVersion"] == 1,
                "protocol_version_mismatch")
        require(initialized.get("authMethods") == [], "unexpected_auth_methods")
        capabilities = initialized.get("agentCapabilities", {})
        require(isinstance(capabilities, dict) and capabilities.get("loadSession") is True
                and capabilities.get("mcpCapabilities", {}).get("http") is True,
                "durable_http_capability_missing")
        info = initialized.get("agentInfo", {})
        require(isinstance(info, dict) and info.get("name") == "tutor-local-agent", "unexpected_runtime")
        version = info.get("version")
        require(isinstance(version, str) and re.fullmatch(r"\d{1,5}\.\d{1,5}\.\d{1,5}", version),
                "invalid_runtime_version")
        report["runtime"]["version"] = version
        report["validations"][stage] = "passed"
        next_stage("session")
        conversation = str(uuid.uuid4())
        owner = {"profileId": "qualification", "accountId": "synthetic-learner",
                 "tenantId": "synthetic-tenant", "serverId": "controlled-loopback-fixture"}
        context = {"conversationId": conversation, "language": "fr", "trainingContext": {
            "conversation_id": conversation, "training_id": "qualification-training", "domain_id": DOMAIN,
            "revision": "1", "selection_version": 1, "name": "Qualification synthétique"}}
        await wire.send("session/new", {"cwd": temporary.name,
                        "mcpServers": [{"type": "http", "name": "tutor", "url": fixture.url,
                                        "headers": [{"name": "Tutor-Conversation-ID", "value": conversation}]}],
                        "_meta": {"tutorLocal": {"owner": owner}}}, deadline, 2)
        session = await wire.response(2, deadline)
        session_id = session.get("sessionId")
        require(isinstance(session_id, str) and str(uuid.UUID(session_id)) == session_id, "invalid_session_id")
        choices = session.get("configOptions")
        require(isinstance(choices, list) and any(isinstance(c, dict) and c.get("id") == "model"
                and c.get("currentValue") == base.MODEL_ID for c in choices), "model_profile_mismatch")
        report["validations"][stage] = "passed"
        next_stage("prompt")
        completed = await run_prompt(wire, session_id, context, fixture, options, allowed, deadline, progress)
        report["metrics"]["prompt"] = completed[3]
        report["validations"][stage] = "passed"
        next_stage("dispatch")
        require(not fixture.errors and fixture.calls == int(allowed), "fixture_dispatch_failed")
        report["validations"][stage] = "passed"
    except QualificationError as error:
        report["errors"].append(error.args[0])
        report["validations"][stage] = "failed"
    except asyncio.TimeoutError:
        report["errors"].append("qualification_timeout")
        report["validations"][stage] = "failed"
    except (OSError, ValueError, TypeError, KeyError, RuntimeError, RecursionError):
        report["errors"].append("local_execution_error")
        report["validations"][stage] = "failed"
    finally:
        next_stage("shutdown")
        if wire is not None:
            if progress.get("active") and session_id is not None:
                report["cancellation"] = await cancel_active(wire, session_id, options.cancel_timeout)
            report["metrics"]["wire"] = {"frames": wire.frames, "bytes": wire.bytes}
        if process is not None:
            report["shutdown"] = await base.stop_process(process, options.shutdown_timeout)
            report["validations"]["shutdown"] = "passed" if (
                report["shutdown"]["graceful_eof"] and report["shutdown"]["reaped"]) else "failed"
        if fixture is not None:
            await fixture.close()
            report["metrics"]["mcp"] = {"http_requests": fixture.requests, "bytes_received": fixture.bytes,
                                         "catalog_requests": fixture.lists, "tool_calls": fixture.calls,
                                         "errors": len(fixture.errors)}
            report["metrics"]["milestone_ms"] = {
                key: round((value - progress["prompt_started"]) * 1000, 3)
                for key, value in fixture.times.items() if "prompt_started" in progress}
            if fixture.errors:
                report["validations"]["dispatch"] = "failed"
                report["errors"].append("fixture_protocol_failed")
        if completed is not None and report["validations"]["shutdown"] == "passed":
            next_stage("journal")
            try:
                answer, call_id, result, _ = completed
                metrics, identity = inspect_journal(data_dir, session_id, call_id, fixture, answer,
                                                    result, options, allowed,
                                                    report["artifacts"]["model"]["sha256"], deadline)
                report["metrics"]["journal"] = metrics
                report["artifacts"]["journal"] = identity
                report["validations"]["journal"] = "passed"
            except QualificationError as error:
                report["errors"].append(error.args[0])
                report["validations"]["journal"] = "failed"
            except (sqlite3.Error, OSError, ValueError, TypeError, KeyError, RecursionError):
                report["errors"].append("journal_inspection_failed")
                report["validations"]["journal"] = "failed"
        report["metrics"]["stage_ms"][stage] = round((time.monotonic() - stage_started) * 1000, 3)
        diagnostic_started = time.monotonic()
        if data_dir is not None and session_id is not None and report.get("shutdown", {}).get("reaped") is True:
            diagnostic = journal_diagnostics(data_dir, session_id)
        else:
            diagnostic = {"status": "not_inspected", "reason": "no_session_or_process_not_reaped"}
        # Diagnostics never set a validation to passed and never remove an error.
        report["diagnostics"] = {"journal": diagnostic}
        report["metrics"]["stage_ms"]["journal_diagnostics"] = round(
            (time.monotonic() - diagnostic_started) * 1000, 3)
        if temporary is not None:
            try:
                temporary.cleanup()
            except OSError:
                report["errors"].append("temporary_cleanup_failed")
    report["metrics"]["total_ms"] = round((time.monotonic() - started) * 1000, 3)
    report["protocol_passed"] = all(v == "passed" for v in report["validations"].values()) and not report["errors"]
    report["qualified"] = bool(not protocol_only and allowed and report["protocol_passed"]
                               and report.get("artifacts", {}).get("model", {}).get("pinned_identity_verified"))
    if report["qualified"]:
        report["inference_status"] = "observed_native_generation_and_journal_actions"
    return report


async def qualify(options):
    return await _qualify(options)


async def qualify_protocol(options, argv, *, permission="allow"):
    """Fixture-only test API. Never produces qualified=True or inference claims."""
    require(permission in ("allow", "deny"), "invalid_fixture_permission")
    return await _qualify(options, protocol_command=argv, allowed=permission == "allow")


def parse_options(argv=None):
    # Same option names and bounds as the helper; allow time for multiple model
    # invocations in this scenario and enough output for a constrained action.
    parser = base.SafeParser(description=__doc__, formatter_class=base.argparse.RawDescriptionHelpFormatter)
    for flag in ("agent", "model", "output"):
        parser.add_argument("--" + flag, type=Path, required=True)
    for flag, default in (("gpu-layers", 0), ("threads", 2), ("batch", 32), ("max-output", 256)):
        parser.add_argument("--" + flag, type=int, default=default)
    for flag, default in (("timeout", 1800), ("cancel-timeout", 10), ("shutdown-timeout", 5)):
        parser.add_argument("--" + flag, type=base.finite_number, default=default)
    parser.add_argument("--model-sha256", help="Additional digest; cannot override the repository model pin")
    parser.add_argument("--model-profile", type=Path,
                        help="Additional manifest; must match the mandatory repository pin")
    return parser.parse_args(argv)


def main(argv=None):
    options = parse_options(argv)
    try:
        guard_output(options)
        report = asyncio.run(qualify(options))
        base.write_report(report, options.output)
    except KeyboardInterrupt:
        print("qualification_interrupted", file=sys.stderr)
        return 130
    except (OSError, ValueError, QualificationError):
        print("report_write_failed", file=sys.stderr)
        return 2
    print("qualification_passed" if report["qualified"] else "qualification_failed")
    return 0 if report["qualified"] else 1


if __name__ == "__main__":
    sys.exit(main())
