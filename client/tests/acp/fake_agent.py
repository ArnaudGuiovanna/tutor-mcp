#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Protocol fixture: independent process, genuine bidirectional stdio JSON-RPC."""

import json
import os
import subprocess
import sys
import time


def send(packet):
    sys.stdout.write(json.dumps(packet, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def result(request, value):
    send({"jsonrpc": "2.0", "id": request["id"], "result": value})


if hasattr(sys.stdin, "reconfigure"):
    sys.stdin.reconfigure(encoding="utf-8")
    sys.stdout.reconfigure(encoding="utf-8", newline="\n")

first = None
prompt = None
unsupported = None
ignored = 0
for line in sys.stdin:
    request = json.loads(line)
    method = request.get("method")
    if method == "test/first":
        first = request
    elif method == "test/second":
        result(request, {"order": "second"})
        result(first, first["params"])
    elif method == "session/prompt":
        prompt = request
        send({"jsonrpc": "2.0", "method": "session/update", "params": {
            "sessionId": "lesson-1", "update": {"sessionUpdate": "agent_message_chunk",
            "content": {"type": "text", "text": "Bonjour, élève."}}}})
        if request["params"].get("permission"):
            send({"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": {
                "sessionId": "lesson-1", "toolCall": {"toolCallId": "tool-1", "title": "Créer un parcours"},
                "options": [{"optionId": "allow-once", "name": "Autoriser", "kind": "allow_once"},
                            {"optionId": "reject-once", "name": "Refuser", "kind": "reject_once"}]}})
        else:
            result(request, {"stopReason": "end_turn"})
    elif method == "session/cancel":
        pass
    elif method == "test/unsupported":
        unsupported = request
        send({"jsonrpc": "2.0", "id": 7001, "method": "fs/read_text_file",
              "params": {"path": "/private/data"}})
    elif method == "test/ignore":
        ignored += 1
    elif method == "test/count":
        result(request, {"ignored": ignored})
    elif method == "test/block":
        result(request, {"blocking": True})
        time.sleep(60)
    elif method == "test/child":
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"],
                                 stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                 stderr=subprocess.DEVNULL)
        result(request, {"pid": child.pid})
    elif method == "test/crash":
        os._exit(17)
    elif method == "test/final_response":
        result(request, {"saved": True})
        os._exit(0)
    elif method == "test/invalid":
        sys.stdout.write("private-payload:invalid-json\n")
        sys.stdout.flush()
    elif method == "test/oversized":
        sys.stdout.write("private-payload" + "x" * (8 * 1024 * 1024) + "\n")
        sys.stdout.flush()
    elif method == "test/partial":
        sys.stdout.write('{"private-payload":')
        sys.stdout.flush()
        os._exit(1)
    elif method == "test/secret_error":
        sys.stderr.write("Bearer sk-private-provider-credential\n")
        sys.stderr.flush()
        send({"jsonrpc": "2.0", "id": request["id"], "error": {
            "code": -32000, "message": "API key sk-private-provider-credential is invalid",
            "data": {"token": "sk-private-provider-credential"}}})
    elif method is None and request.get("id") == "permission-1":
        outcome = request["result"]["outcome"]
        cancelled = outcome["outcome"] == "cancelled"
        result(prompt, {"stopReason": "cancelled" if cancelled else "end_turn",
                        "decision": "cancelled" if cancelled else outcome["optionId"]})
    elif method is None and request.get("id") == 7001:
        result(unsupported, {"clientError": request["error"]["code"]})
    elif "id" in request and method:
        result(request, request.get("params", {}))
