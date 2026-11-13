#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Manual local ACP + real Tutor stdio check, using disposable fixture data only.

Requires the built server/agent and the pinned GGUF; downloads nothing. The
server is always started with --local and a fresh private data directory. This
is not a qualification of a remote institution or of pedagogical assessment.
"""
import argparse
import asyncio
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import time
import uuid

spec = importlib.util.spec_from_file_location(
    "local_agent_probe", Path(__file__).with_name("qualify-local-agent.py"))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


def payload(result):
    probe.require(not result.get("isError"), "fixture_tool_failed")
    value = result.get("structuredContent")
    if value is None:
        texts = [item["text"] for item in result.get("content", []) if item.get("type") == "text"]
        probe.require(len(texts) == 1, "fixture_payload_invalid")
        value = json.loads(texts[0])
    probe.require(isinstance(value, dict), "fixture_payload_invalid")
    return value


async def run(options):
    env = {key: os.environ[key] for key in ("PATH", "SystemRoot", "WINDIR", "TEMP", "TMP", "TMPDIR", "LANG") if key in os.environ}
    conversation = str(uuid.uuid4())
    marker = "Potager " + uuid.uuid4().hex[:10]
    objective = "Cultiver " + uuid.uuid4().hex[:10]
    report = {"scope": "Real local Tutor stdio + ACP read/context checks, disposable fixture; no remote, coaching-quality or assessment qualification", "coachingQuality": "not_qualified", "checks": [], "qualified": False}
    with tempfile.TemporaryDirectory(prefix="tutor-context-") as root:
        root = Path(root)
        server_dir = root / "server"
        agent_dir = root / "agent"
        server_args = [str(options.server), "--local", "--data-dir", str(server_dir)]
        server = await asyncio.create_subprocess_exec(*server_args, env={**env, "TUTOR_CONVERSATION_ID": conversation}, stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL, limit=probe.MAX_FRAME, start_new_session=os.name != "nt")
        wire = probe.Wire(server)
        request_id = 0

        async def rpc(method, params):
            nonlocal request_id
            request_id += 1
            deadline = time.monotonic() + 30
            await wire.send(method, params, deadline, request_id)
            return await wire.response(request_id, deadline)

        try:
            await rpc("initialize", {"protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "local-context-fixture", "version": "1"}})
            await wire.send("notifications/initialized", {}, time.monotonic() + 10)
            payload(await rpc("tools/call", {"name": "update_learner_profile", "arguments": {"objective": objective, "device": "laptop", "language": "fr", "idempotency_key": str(uuid.uuid4())}}))
            payload(await rpc("tools/call", {"name": "init_domain", "arguments": {"name": marker, "concepts": ["sol"], "prerequisites": {}, "idempotency_key": str(uuid.uuid4())}}))
            training = payload(await rpc("tools/call", {"name": "get_conversation_context", "arguments": {"conversation_id": conversation}}))
            probe.require(training.get("name") == marker and training.get("conversation_id") == conversation, "fixture_training_not_confirmed")
        finally:
            await probe.stop_process(server, 10)

        agent = await asyncio.create_subprocess_exec(str(options.agent), "--model", str(options.model), "--data-dir", str(agent_dir), "--context", "2048", "--max-output", "192", "--threads", "4", "--batch", "128", env=env, stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL, limit=probe.MAX_FRAME, start_new_session=os.name != "nt")
        wire = probe.Wire(agent)
        try:
            await rpc("initialize", {"protocolVersion": 1, "clientCapabilities": {}})
            new = await rpc("session/new", {"cwd": str(root), "mcpServers": [{"name": "tutor", "command": str(options.server), "args": server_args[1:], "env": [{"name": "TUTOR_CONVERSATION_ID", "value": conversation}]}], "_meta": {"tutorLocal": {"owner": {"profileId": "fixture", "accountId": "fixture", "tenantId": "fixture", "serverId": "fixture"}}}})
            session = new["sessionId"]
            cases = [
                ("test", True, None, False),
                ("donne moi mon profil", True, objective, False),
                ("Recherche mon profil ou ma formation", True, marker, False),
                ("Que peux-tu faire ?", True, "coach", False),
                ("Quelle est ma formation active ?", True, marker, False),
                ("donne moi mon profil", False, "autorisation", False),
                ("Quel est le nom exact de la formation active ? Réponds en une phrase.", True, marker, True),
                ("Je débute en jardinage. Propose un premier pas concret et pose-moi une seule question.", True, None, True),
            ]
            for text, allowed, expected, generated in cases:
                request_id += 1
                deadline = time.monotonic() + 180
                start = time.monotonic()
                chunks, first, approvals = [], None, 0
                await wire.send("session/prompt", {"sessionId": session, "prompt": [{"type": "text", "text": text}], "_meta": {"tutorLocal": {"task": {"conversationId": conversation, "language": "fr", "trainingContext": training}}}}, deadline, request_id)
                while True:
                    message = await wire.receive(deadline)
                    if message.get("method") == "session/request_permission":
                        approvals += 1
                        probe.require(message["params"]["toolCall"]["rawInput"]["tool"] == "get_learner_state", "unexpected_tool_permission")
                        choices = message["params"]["options"]
                        choice = next(c for c in choices if c["kind"] == ("allow_once" if allowed else "reject_once"))
                        response = {"jsonrpc": "2.0", "id": message["id"], "result": {"outcome": {"outcome": "selected", "optionId": choice["optionId"]}}}
                        agent.stdin.write(json.dumps(response).encode() + b"\n")
                        await asyncio.wait_for(agent.stdin.drain(), probe.remaining(deadline))
                    elif message.get("method") == "session/update":
                        update = message["params"]["update"]
                        if update["sessionUpdate"] == "agent_message_chunk":
                            if first is None:
                                first = (time.monotonic() - start) * 1000
                            chunks.append(update["content"]["text"])
                    elif message.get("id") == request_id:
                        probe.require("error" not in message, "agent_prompt_failed")
                        result = message["result"]
                        break
                    else:
                        raise probe.QualificationError("unexpected_agent_message")
                answer = "".join(chunks)
                stats = result["_meta"]["tutor"]["generation"]
                probe.require(result["stopReason"] == "end_turn", "incomplete_answer")
                probe.require(expected is None or expected in answer, "expected_fact_missing")
                probe.require((stats["inputTokens"] > 0) == generated, "unexpected_generation")
                probe.require(approvals == (1 if "profil" in text else 0), "unexpected_permission_count")
                if not allowed:
                    probe.require(objective not in answer, "denied_profile_disclosed")
                report["checks"].append({"prompt": text, "permissionAllowed": allowed, "permissions": approvals, "firstTextMs": round(first, 2), "totalMs": round((time.monotonic() - start) * 1000, 2), "generated": generated, "generation": stats, "answer": answer})
        finally:
            await probe.stop_process(agent, 10)
    report["qualified"] = True
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("server", "agent", "model", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    options = parser.parse_args()
    for name in ("server", "agent", "model"):
        path = getattr(options, name).resolve(strict=True)
        if not path.is_file():
            parser.error(f"{name} must be a file")
        setattr(options, name, path)
    if options.output.exists():
        parser.error("output must not exist")
    report = asyncio.run(run(options))
    report["artifacts"] = {}
    for name in ("server", "agent", "model"):
        with getattr(options, name).open("rb") as stream:
            report["artifacts"][name] = hashlib.file_digest(stream, "sha256").hexdigest()
    with options.output.open("x", encoding="utf-8") as stream:
        json.dump(report, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
    print(json.dumps({"qualified": report["qualified"], "checks": len(report["checks"]), "report": str(options.output)}))


if __name__ == "__main__":
    main()
