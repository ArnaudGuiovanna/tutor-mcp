#!/usr/bin/env python3
"""Measure the real MCP server on disposable data, without calling a model.

This qualifies server overhead and the assessment protocol, not provider
latency, pedagogical quality of generated prose, or desktop rendering.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import select
import subprocess
import tempfile
import time


def compact(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()


def measure(server, samples):
    env = {k: v for k, v in os.environ.items() if k in ("PATH", "HOME", "LANG", "LC_ALL")}
    with tempfile.TemporaryDirectory(prefix="tutor-context-") as directory:
        with open(Path(directory) / "stderr.log", "wb") as stderr:
            process = subprocess.Popen([str(server), "--local", "--data-dir", directory],
                                       stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                       stderr=stderr, cwd=directory, env=env)
            buffer = b""
            sequence = 0

            def request(method, params):
                nonlocal sequence, buffer
                sequence += 1
                process.stdin.write(compact({"jsonrpc": "2.0", "id": sequence,
                                            "method": method, "params": params}) + b"\n")
                process.stdin.flush()
                deadline = time.monotonic() + 30
                while time.monotonic() < deadline:
                    while b"\n" in buffer:
                        line, buffer = buffer.split(b"\n", 1)
                        if not line:
                            continue
                        response = json.loads(line)
                        if response.get("id") == sequence:
                            if "error" in response:
                                raise RuntimeError(f"{method}: protocol failure")
                            return response["result"]
                    if not select.select([process.stdout], [], [], max(0, deadline-time.monotonic()))[0]:
                        break
                    chunk = os.read(process.stdout.fileno(), 65536)
                    if not chunk:
                        raise RuntimeError("MCP process closed")
                    buffer += chunk
                    if len(buffer) > 4 * 1024 * 1024:
                        raise RuntimeError("MCP response too large")
                raise TimeoutError(method)

            def call(name, args):
                result = request("tools/call", {"name": name, "arguments": args,
                                                "_meta": {"tutor/conversation_id": "qualification"}})
                if result.get("isError"):
                    raise RuntimeError(f"{name}: server rejected the synthetic protocol scenario")
                if "structuredContent" in result:
                    return result["structuredContent"]
                return json.loads(next(c["text"] for c in result["content"] if c["type"] == "text"))

            try:
                request("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                       "clientInfo": {"name": "context-qualification", "version": "1"}})
                process.stdin.write(compact({"jsonrpc": "2.0", "method": "notifications/initialized"}) + b"\n")
                process.stdin.flush()
                tools, cursor = [], None
                while True:
                    page = request("tools/list", {"cursor": cursor} if cursor else {})
                    tools.extend(page["tools"])
                    cursor = page.get("nextCursor")
                    if not cursor:
                        break
                call("init_domain", {"name": "Synthetic addition", "concepts": ["addition"],
                                    "prerequisites": {}, "personal_goal": "Calculate integer sums",
                                    "idempotency_key": "qualification-domain",
                                    "details": {"addition": {"label": "Addition", "description": "Calculate integer sums", "level": "foundation"}}})
                contexts, sizes = [], []
                for _ in range(samples):
                    start = time.perf_counter()
                    packet = call("prepare_pedagogical_turn", {"mode": "context"})
                    contexts.append((time.perf_counter()-start)*1000)
                    sizes.append(len(compact(packet)))
                    assert packet["schema_version"] == "pedagogical-turn/v1"
                    assert packet["conversation"]["domain_id"]
                    assert "activity" not in packet and not packet["provenance"]["model_updated"]
                start = time.perf_counter()
                activity = call("prepare_pedagogical_turn", {"mode": "activity", "idempotency_key": "qualification-activity"})
                activity_ms = (time.perf_counter()-start)*1000
                replay = call("prepare_pedagogical_turn", {"mode": "activity", "idempotency_key": "qualification-activity"})
                decision = activity["activity"]
                assert replay["activity"]["decision_id"] == decision["decision_id"]
                assert "consolidation_request" not in decision
                domain = activity["conversation"]["domain_id"]
                chosen = decision["activity"]
                rubric = {"criteria": [{"id": "correct", "description": "The sum is correct", "max_score": 1}], "passing_score": 1, "answer_key": "5"}
                frozen = call("prepare_assessment_attempt", {
                    "idempotency_key": "qualification-prepare", "domain_id": domain,
                    "concept": chosen["concept"], "session_id": decision["session_id"],
                    "decision_id": decision["decision_id"], "activity_id": "qualification-question", "activity_version": 1,
                    "activity_type": chosen["type"], "observable": "Calculate integer sums",
                    "task_text": "What is 2 + 3?", "rubric_json": json.dumps(rubric),
                    "outcome_ids": [o["id"] for o in (decision["pedagogical_contract"]["competency"].get("outcomes") or [])]})
                attempt = frozen["attempt_id"]
                submitted = call("submit_assessment_attempt", {"idempotency_key": "qualification-submit", "attempt_id": attempt, "learner_response": "5"})
                assert submitted["status"] == "submitted"
                grade = {"idempotency_key": "qualification-grade", "domain_id": domain, "concept": chosen["concept"], "session_id": decision["session_id"],
                         "attempt_id": attempt, "success": True, "activity_type": chosen["type"], "response_time_seconds": 2, "confidence": .9,
                         "evaluation_method": "host_llm", "evaluator_id": "synthetic-protocol-fixture",
                         "rubric_score_json": json.dumps({"criteria_scores": [{"id": "correct", "score": 1, "max_score": 1, "evidence": "The response is 5"}],
                                                          "total": 1, "max_total": 1, "summary": "Correct sum", "confidence": .9})}
                recorded = call("record_interaction", grade)
                call("record_interaction", grade)
                assert recorded["assessment_status"] == "evaluated" and recorded["derived_success"]
                assert not recorded["evaluation_trusted"]
                final = call("prepare_pedagogical_turn", {"mode": "context", "attempt_id": attempt})
                assert final["attempt"]["status"] == "evaluated"
                assert len(final["recent_observations"]) == 1, "retry duplicated observation"
                ordered = sorted(contexts)
                return {"schema": "pedagogical-context-measurement/v1", "serverSha256": hashlib.sha256(server.read_bytes()).hexdigest(),
                        "scope": "Isolated Linux stdio + SQLite, synthetic assessment, no model or UI", "modelCalls": 0,
                        "providerLatencyQualified": False, "pedagogicalProseQualified": False,
                        "catalog": {"tools": len(tools), "bytes": len(compact(tools)), "inputSchemaBytes": sum(len(compact(t.get("inputSchema", {}))) for t in tools)},
                        "context": {"samples": samples, "p50Ms": ordered[len(ordered)//2], "p95Ms": ordered[math.ceil(.95*len(ordered))-1], "maxMs": max(contexts), "maxBytes": max(sizes)},
                        "activityMs": activity_ms, "activityRetryPreservedDecision": True,
                        "assessmentFrozenSubmittedRecorded": True, "observationCountAfterRetry": 1}
            finally:
                process.stdin.close()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", required=True, type=Path)
    parser.add_argument("--samples", type=int, default=50)
    args = parser.parse_args()
    if not 2 <= args.samples <= 1000:
        parser.error("samples must be 2..1000")
    print(json.dumps(measure(args.server.resolve(strict=True), args.samples), indent=2))
