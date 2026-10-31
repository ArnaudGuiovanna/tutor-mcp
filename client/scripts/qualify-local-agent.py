#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
r"""Bounded live ACP/GGUF qualification; standard library only, no downloads.

Example (after building the native agent):
  python3 client/scripts/qualify-local-agent.py \
    --agent /absolute/client/local-agent/target/debug/tutor-local-agent \
    --model /absolute/model.gguf --output /absolute/report.json

Two arithmetic prompts and a cancellation prompt use fixed public French text.
The second arithmetic call reuses the process; it does not independently prove
model residency. --timeout is shared by hashing and ACP; shutdown has its own
bounded grace and kill/reap waits.
Responses are inspected in memory, NEVER copied into reports. Reports contain
allowlisted values, timings and streamed artifact fingerprints, not filenames,
hostnames, stderr, raw metadata or exception messages. Generation evidence is
behavioral, not independent attestation of an arbitrary executable's internals.
Fixtures must use qualify_protocol(); its reports can never qualify inference.
Live qualification requires the repository's pinned model-profile.json identity
(SHA256 AND byte count). Neither a custom manifest nor --model-sha256 can replace
that pin. A pass covers ordinary ACP on this host, never Windows/P2000 acceptance
or the native constrained-decoding test performed separately.
"""

import argparse
import asyncio
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import re
import signal
import struct
import sys
import tempfile
import time


MODEL_ID = "qwen3-1.7b-q4_k_m"
PINNED_MODEL_PROFILE = Path(__file__).resolve().parents[1] / "local-agent/model-profile.json"
ARITHMETIC = "Calcule 17 + 25. Réponds uniquement par le nombre, sans explication."
LONG_PROMPT = (
    "Écris une liste numérotée de 1 à 1000. Pour chaque nombre, rédige une phrase "
    "complète en français qui explique comment apprendre les additions. "
    "Commence directement par la liste et poursuis sans conclusion."
)
MAX_FRAME = 256 * 1024
MAX_WIRE_BYTES = 8 * 1024 * 1024
MAX_FRAMES = 4096
MAX_TEXT = 64 * 1024
HASH_CHUNK = 1024 * 1024


class QualificationError(Exception):
    """Only static codes created by this module may enter a report."""


def require(condition, code):
    if not condition:
        raise QualificationError(code)


def remaining(deadline):
    duration = deadline - time.monotonic()
    require(duration > 0, "qualification_timeout")
    return duration


def fingerprint(path, deadline):
    """Constant-memory SHA256 with a deadline checked between bounded reads."""
    digest = hashlib.sha256()
    with path.open("rb") as source:
        before = os.fstat(source.fileno())
        while True:
            remaining(deadline)
            block = source.read(HASH_CHUNK)
            if not block:
                break
            digest.update(block)
        after = os.fstat(source.fileno())
    require((before.st_size, before.st_mtime_ns) ==
            (after.st_size, after.st_mtime_ns), "artifact_changed_during_hash")
    return {"sha256": digest.hexdigest(), "bytes": after.st_size}


def protected_inputs(options):
    paths = [options.agent, options.model, PINNED_MODEL_PROFILE]
    if options.model_profile is not None:
        paths.append(options.model_profile)
    return {path.resolve() for path in paths}


def validate_options(options):
    for path in (options.agent, options.model):
        require(path.is_absolute() and path.is_file(), "absolute_file_required")
    require(os.access(options.agent, os.X_OK), "agent_not_executable")
    require(0 <= options.gpu_layers <= 36 and 1 <= options.threads <= 32
            and 1 <= options.batch <= 512 and 1 <= options.max_output <= 768,
            "invalid_hardware_limits")
    require(0 < options.timeout <= 3600 and 0 < options.cancel_timeout <= 30
            and 0 < options.shutdown_timeout <= 30, "invalid_time_limits")
    require(options.output.resolve() not in protected_inputs(options), "report_overwrites_input")
    if options.model_sha256 is not None:
        require(re.fullmatch(r"[0-9a-fA-F]{64}", options.model_sha256) is not None,
                "invalid_model_sha256")


def profile_identity(path, *, pinned=False):
    try:
        with path.open("rb") as source:
            encoded = source.read(65537)
    except OSError:
        raise QualificationError("pinned_model_profile_unavailable" if pinned
                                 else "model_profile_unavailable") from None
    require(len(encoded) <= 65536, "invalid_model_profile")
    try:
        profile = json.loads(encoded)
    except (ValueError, UnicodeError):
        raise QualificationError("invalid_model_profile") from None
    require(isinstance(profile, dict) and profile.get("id") == MODEL_ID
            and isinstance(profile.get("gguf_sha256"), str)
            and re.fullmatch(r"[0-9a-f]{64}", profile["gguf_sha256"])
            and type(profile.get("gguf_bytes")) is int and profile["gguf_bytes"] > 0,
            "invalid_model_profile")
    return {"id": profile["id"], "sha256": profile["gguf_sha256"],
            "bytes": profile["gguf_bytes"]}


def inspect_artifacts(options, deadline, protocol_only):
    # Live runs fail closed if the checked-in pin is absent. A user-supplied
    # digest/manifest can add a check, but cannot redefine the approved model.
    pinned = None if protocol_only else profile_identity(PINNED_MODEL_PROFILE, pinned=True)
    supplied = profile_identity(options.model_profile) if options.model_profile is not None else None
    if pinned is not None and supplied is not None:
        require(supplied == pinned, "model_profile_conflicts_with_pin")
    if pinned is not None and options.model_sha256:
        require(options.model_sha256.lower() == pinned["sha256"], "model_sha256_conflicts_with_pin")
    expected = pinned or supplied
    with options.model.open("rb") as source:
        header = source.read(24)
    require(len(header) == 24, "invalid_gguf_header")
    magic, version, tensors, metadata = struct.unpack("<4sIQQ", header)
    require(magic == b"GGUF" and version in (2, 3) and tensors > 0 and metadata > 0,
            "invalid_gguf_header")
    if not protocol_only:
        with options.agent.open("rb") as source:
            signature = source.read(4)
        if os.name == "nt":
            native = options.agent.suffix.lower() == ".exe" and signature.startswith(b"MZ")
        elif sys.platform == "darwin":
            native = signature in (b"\xfe\xed\xfa\xce", b"\xce\xfa\xed\xfe",
                                   b"\xfe\xed\xfa\xcf", b"\xcf\xfa\xed\xfe",
                                   b"\xca\xfe\xba\xbe", b"\xbe\xba\xfe\xca",
                                   b"\xca\xfe\xba\xbf", b"\xbf\xba\xfe\xca")
        else:
            native = signature == b"\x7fELF"
        require(native, "native_executable_required")
    model = fingerprint(options.model, deadline)
    model.update({"profile": MODEL_ID, "gguf_version": version,
                  "header_only_validation": True, "expected_sha256_verified": None,
                  "manifest_verified": False, "pinned_identity_verified": False,
                  "pinned_sha256": None})
    if options.model_sha256:
        require(model["sha256"] == options.model_sha256.lower(), "model_sha256_mismatch")
        model["expected_sha256_verified"] = True
    if expected is not None:
        require(expected["sha256"] == model["sha256"] and expected["bytes"] == model["bytes"],
                "model_profile_mismatch")
        model["manifest_verified"] = True
    if pinned is not None:
        model["pinned_identity_verified"] = True
        model["pinned_sha256"] = pinned["sha256"]
    return {"agent": fingerprint(options.agent, deadline), "model": model}


def command(options, model_sha256=None):
    argv = [str(options.agent), "--model", str(options.model), "--context", "2048",
            "--max-output", str(options.max_output), "--gpu-layers", str(options.gpu_layers),
            "--threads", str(options.threads), "--batch", str(options.batch)]
    if model_sha256 is not None:
        argv.extend(["--sha256", model_sha256])
    return argv


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "invalid_json_frame")
        result[key] = value
    return result


def reject_constant(_value):
    raise QualificationError("invalid_json_frame")


class Wire:
    def __init__(self, process):
        self.process = process
        self.bytes = 0
        self.frames = 0

    async def send(self, method, params, deadline, request_id=None):
        frame = {"jsonrpc": "2.0", "method": method, "params": params}
        if request_id is not None:
            frame["id"] = request_id
        encoded = json.dumps(frame, ensure_ascii=False).encode("utf-8") + b"\n"
        require(len(encoded) <= MAX_FRAME, "outgoing_frame_too_large")
        self.process.stdin.write(encoded)
        await asyncio.wait_for(self.process.stdin.drain(), remaining(deadline))

    async def receive(self, deadline):
        try:
            line = await asyncio.wait_for(self.process.stdout.readline(), remaining(deadline))
        except ValueError:
            raise QualificationError("frame_too_large") from None
        require(bool(line), "unexpected_eof")
        require(len(line) <= MAX_FRAME, "frame_too_large")
        require(line.endswith(b"\n"), "unterminated_frame")
        self.bytes += len(line)
        self.frames += 1
        require(self.bytes <= MAX_WIRE_BYTES and self.frames <= MAX_FRAMES,
                "wire_budget_exceeded")
        try:
            message = json.loads(line, object_pairs_hook=unique_object,
                                 parse_constant=reject_constant)
        except (ValueError, UnicodeError, RecursionError):
            raise QualificationError("invalid_json_frame") from None
        require(isinstance(message, dict) and message.get("jsonrpc") == "2.0",
                "invalid_rpc_frame")
        return message

    async def response(self, request_id, deadline, on_update=None):
        while True:
            message = await self.receive(deadline)
            if "method" in message:
                require(message.get("method") == "session/update" and "id" not in message,
                        "unexpected_agent_method")
                require(on_update is not None, "unexpected_session_update")
                await on_update(message.get("params"))
                continue
            require(type(message.get("id")) is int and message["id"] == request_id,
                    "response_id_mismatch")
            require("error" not in message, "agent_rpc_error")
            require(isinstance(message.get("result"), dict), "invalid_result")
            return message["result"]


def generation_metadata(result, max_output):
    try:
        raw = result["_meta"]["tutor"]["generation"]
        values = [raw[key] for key in ("inputTokens", "outputTokens", "elapsedMs")]
    except (KeyError, TypeError):
        return None
    if not all(type(value) is int for value in values):
        return None
    inputs, outputs, elapsed = values
    if not (0 < inputs <= 2048 - max_output - 256
            and 0 < outputs <= max_output and 0 <= elapsed <= 3_600_000):
        return None
    return {"input_tokens": inputs, "output_tokens": outputs, "elapsed_ms": elapsed}


async def prompt(wire, session_id, request_id, text, deadline, options, cancel=False):
    started = time.monotonic()
    first_text = None
    cancelled_at = None
    pieces = []
    text_bytes = 0
    chunks = 0
    response_deadline = deadline

    async def update(params):
        nonlocal first_text, cancelled_at, chunks, text_bytes, response_deadline
        require(isinstance(params, dict) and params.get("sessionId") == session_id,
                "session_id_mismatch")
        body = params.get("update")
        require(isinstance(body, dict), "invalid_session_update")
        require(body.get("sessionUpdate") == "agent_message_chunk", "unexpected_update_type")
        content = body.get("content")
        require(isinstance(content, dict) and content.get("type") == "text"
                and isinstance(content.get("text"), str), "invalid_text_update")
        piece = content["text"]
        try:
            text_bytes += len(piece.encode("utf-8"))
        except UnicodeError:
            raise QualificationError("invalid_text_update") from None
        require(text_bytes <= MAX_TEXT, "text_budget_exceeded")
        chunks += 1
        pieces.append(piece)
        if piece and first_text is None:
            first_text = time.monotonic()
            if cancel:
                cancelled_at = time.monotonic()
                response_deadline = min(deadline, cancelled_at + options.cancel_timeout)
                await wire.send("session/cancel", {"sessionId": session_id}, response_deadline)

    await wire.send("session/prompt", {"sessionId": session_id,
                    "prompt": [{"type": "text", "text": text}]}, deadline, request_id)
    # The cancellation deadline is installed as soon as the first real text
    # chunk is observed; do not reset timeouts for each subsequent notification.
    while True:
        message = await wire.receive(response_deadline)
        if "method" in message:
            require(message.get("method") == "session/update" and "id" not in message,
                    "unexpected_agent_method")
            await update(message.get("params"))
            continue
        require(type(message.get("id")) is int and message["id"] == request_id,
                "response_id_mismatch")
        require("error" not in message, "agent_rpc_error")
        result = message.get("result")
        require(isinstance(result, dict), "invalid_result")
        break
    finished = time.monotonic()
    stop = result.get("stopReason")
    require(stop in ("end_turn", "max_tokens", "cancelled"), "invalid_stop_reason")
    metrics = {
        "first_text_ms": round((first_text - started) * 1000, 3) if first_text else None,
        "elapsed_ms": round((finished - started) * 1000, 3),
        "text_bytes": text_bytes, "chunks": chunks, "stop_reason": stop,
        "generation": generation_metadata(result, options.max_output),
    }
    if cancel:
        metrics["cancel_sent"] = cancelled_at is not None
        metrics["cancel_response_ms"] = (
            round((finished - cancelled_at) * 1000, 3) if cancelled_at else None)
        metrics["within_two_seconds"] = (
            finished - cancelled_at <= 2 if cancelled_at else False)
    else:
        # Only the public expected result is persisted, never the generated text.
        metrics["arithmetic_correct"] = "".join(pieces).strip() == "42"
    return metrics


async def stop_process(process, timeout):
    """EOF first, then bounded termination; always await/reap our child."""
    shutdown = {"graceful_eof": False, "forced": False, "reaped": False, "exit_code": None}
    async def discard_stdout():
        while await process.stdout.read(65536):
            pass

    # Continue draining after a rejected frame so a full pipe cannot prevent
    # EOF/exit. No bytes are retained, parsed or written to the report.
    drain = asyncio.create_task(discard_stdout())
    process.stdin.close()
    try:
        await asyncio.wait_for(process.stdin.wait_closed(), min(timeout, 1))
    except (OSError, asyncio.TimeoutError):
        pass
    try:
        await asyncio.wait_for(process.wait(), timeout)
        shutdown["graceful_eof"] = process.returncode == 0
    except asyncio.TimeoutError:
        shutdown["forced"] = True
        try:
            if os.name == "nt":
                process.kill()
            else:
                os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        try:
            await asyncio.wait_for(process.wait(), timeout)
        except asyncio.TimeoutError:
            drain.cancel()
            await asyncio.gather(drain, return_exceptions=True)
            return shutdown
    shutdown["reaped"] = process.returncode is not None
    shutdown["exit_code"] = process.returncode
    try:
        await asyncio.wait_for(drain, min(timeout, 1))
    except (OSError, asyncio.TimeoutError):
        pass
    return shutdown


def new_report(options, protocol_only):
    return {
        "schema_version": 1,
        "scope": "protocol_only" if protocol_only else "live_gguf",
        "qualified": False, "protocol_passed": False,
        "qualification_scope": "ordinary_acp_on_tested_host_only",
        "target_hardware_qualified": False,
        "not_covered": ["windows_p2000_acceptance", "native_constrained_decoding"],
        "inference_status": "not_tested" if protocol_only else "not_observed",
        "evidence_limit": "behavior_and_artifact_fingerprints_not_independent_attestation",
        "started_at": datetime.now(timezone.utc).isoformat(),
        "host": {"scope": "execution_host_only", "system": platform.system(),
                 "machine": platform.machine(), "python": platform.python_version(),
                 "requested_compute": "cpu" if options.gpu_layers == 0 else "gpu_offload"},
        "configuration": {"context": 2048, "max_output": options.max_output,
                          "gpu_layers": options.gpu_layers, "threads": options.threads,
                          "batch": options.batch, "timeout_seconds": options.timeout,
                          "cancel_timeout_seconds": options.cancel_timeout},
        "runtime": {"name": "tutor-local-agent", "version": None},
        "prompts": ["public_fr_arithmetic_17_plus_25_v1", "public_fr_long_list_v1"],
        "validations": {key: "not_run" for key in (
            "configuration", "artifacts", "initialize", "session", "arithmetic",
            "generation_metadata", "warm_arithmetic", "warm_generation_metadata",
            "cancellation", "shutdown")},
        "metrics": {}, "errors": [],
    }


async def _qualify(options, protocol_command=None):
    protocol_only = protocol_command is not None
    report = new_report(options, protocol_only)
    started = time.monotonic()
    deadline = started + options.timeout
    process = None
    stage = "configuration"
    temporary = None
    try:
        validate_options(options)
        report["validations"][stage] = "passed"
        stage = "artifacts"
        report["artifacts"] = inspect_artifacts(options, deadline, protocol_only)
        report["validations"][stage] = "passed"
        temporary = tempfile.TemporaryDirectory(prefix="tutor-local-qualification-")
        argv = (command(options, report["artifacts"]["model"]["pinned_sha256"])
                if not protocol_only else list(protocol_command))
        stage = "initialize"
        process = await asyncio.wait_for(asyncio.create_subprocess_exec(
            *argv, stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL, cwd=temporary.name, limit=MAX_FRAME,
            start_new_session=os.name != "nt"), remaining(deadline))
        wire = Wire(process)
        handshake_deadline = min(deadline, time.monotonic() + 15)
        await wire.send("initialize", {"protocolVersion": 1, "clientCapabilities": {},
                        "clientInfo": {"name": "tutor-local-qualification", "version": "1"}},
                        handshake_deadline, 1)
        initialized = await wire.response(1, handshake_deadline)
        require(type(initialized.get("protocolVersion")) is int
                and initialized["protocolVersion"] == 1, "protocol_version_mismatch")
        require(initialized.get("authMethods") == [], "unexpected_auth_methods")
        info = initialized.get("agentInfo")
        require(isinstance(info, dict) and info.get("name") == "tutor-local-agent",
                "unexpected_runtime")
        version = info.get("version")
        require(isinstance(version, str) and re.fullmatch(r"\d{1,5}\.\d{1,5}\.\d{1,5}", version),
                "invalid_runtime_version")
        report["runtime"]["version"] = version
        report["validations"][stage] = "passed"
        stage = "session"
        await wire.send("session/new", {"cwd": temporary.name, "mcpServers": []}, deadline, 2)
        session = await wire.response(2, deadline)
        session_id = session.get("sessionId")
        require(isinstance(session_id, str) and 0 < len(session_id) <= 128,
                "invalid_session_id")
        choices = session.get("configOptions")
        require(isinstance(choices, list) and any(
            isinstance(choice, dict) and choice.get("id") == "model"
            and choice.get("currentValue") == MODEL_ID for choice in choices),
            "model_profile_mismatch")
        report["validations"][stage] = "passed"
        stage = "arithmetic"
        arithmetic = await prompt(wire, session_id, 3, ARITHMETIC, deadline, options)
        report["metrics"]["arithmetic"] = arithmetic
        report["validations"][stage] = (
            "passed" if arithmetic["arithmetic_correct"] and arithmetic["stop_reason"] == "end_turn"
            else "failed")
        report["validations"]["generation_metadata"] = (
            "passed" if arithmetic["generation"] is not None else "failed")
        if (not protocol_only and report["artifacts"]["model"]["pinned_identity_verified"]
                and report["validations"][stage] == "passed"
                and arithmetic["generation"] is not None):
            report["inference_status"] = "observed_native_generation"
        stage = "warm_arithmetic"
        warm = await prompt(wire, session_id, 4, ARITHMETIC, deadline, options)
        report["metrics"]["warm_arithmetic"] = warm
        report["validations"][stage] = (
            "passed" if warm["arithmetic_correct"] and warm["stop_reason"] == "end_turn"
            else "failed")
        report["validations"]["warm_generation_metadata"] = (
            "passed" if warm["generation"] is not None else "failed")
        stage = "cancellation"
        cancellation = await prompt(wire, session_id, 5, LONG_PROMPT, deadline, options, cancel=True)
        report["metrics"]["cancellation"] = cancellation
        report["validations"][stage] = (
            "passed" if cancellation["cancel_sent"] and cancellation["stop_reason"] == "cancelled"
            else "failed")
        report["metrics"]["wire"] = {"frames": wire.frames, "bytes": wire.bytes}
    except QualificationError as error:
        report["errors"].append(error.args[0])
        report["validations"][stage] = "failed"
    except asyncio.TimeoutError:
        report["errors"].append("qualification_timeout")
        report["validations"][stage] = "failed"
    except (OSError, ValueError, TypeError, RuntimeError):
        report["errors"].append("local_execution_error")
        report["validations"][stage] = "failed"
    finally:
        if process is not None:
            report["shutdown"] = await stop_process(process, options.shutdown_timeout)
            report["validations"]["shutdown"] = (
                "passed" if report["shutdown"]["graceful_eof"] and report["shutdown"]["reaped"]
                else "failed")
        if temporary is not None:
            try:
                temporary.cleanup()
            except OSError:
                report["errors"].append("temporary_cleanup_failed")
    report["metrics"]["total_ms"] = round((time.monotonic() - started) * 1000, 3)
    report["protocol_passed"] = all(value == "passed" for value in report["validations"].values())
    report["qualified"] = (not protocol_only and report["protocol_passed"]
                           and report.get("artifacts", {}).get("model", {}).get(
                               "pinned_identity_verified") is True
                           and report["inference_status"] == "observed_native_generation"
                           and not report["errors"])
    return report


async def qualify(options):
    return await _qualify(options)


async def qualify_protocol(options, argv):
    """Test/fixture API: overriding argv permanently labels evidence protocol_only."""
    return await _qualify(options, protocol_command=argv)


def write_report(report, output):
    # Atomic, owner-readable report; paths/errors from the OS are never embedded.
    name = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=output.parent,
                                         prefix=".local-agent-report-", delete=False) as file:
            name = file.name
            json.dump(report, file, ensure_ascii=False, indent=2, allow_nan=False)
            file.write("\n")
            file.flush()
            os.fsync(file.fileno())
        os.replace(name, output)
        name = None
    finally:
        if name is not None:
            Path(name).unlink(missing_ok=True)


class SafeParser(argparse.ArgumentParser):
    def error(self, _message):
        self.exit(2, "invalid_arguments\n")


def finite_number(text):
    value = float(text)
    if not math.isfinite(value):
        raise ValueError("finite_number_required")
    return value


def parse_options(argv=None):
    parser = SafeParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--agent", type=Path, required=True)
    parser.add_argument("--model", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--gpu-layers", type=int, default=0)
    parser.add_argument("--threads", type=int, default=2)
    parser.add_argument("--batch", type=int, default=32)
    parser.add_argument("--max-output", type=int, default=128)
    parser.add_argument("--timeout", type=finite_number, default=180)
    parser.add_argument("--cancel-timeout", type=finite_number, default=10)
    parser.add_argument("--shutdown-timeout", type=finite_number, default=5)
    parser.add_argument("--model-sha256", help="Additional expected digest; cannot override the repository model pin")
    parser.add_argument("--model-profile", type=Path,
                        help="Additional manifest; live runs require its identity to match the mandatory repository pin")
    return parser.parse_args(argv)


def main(argv=None):
    options = parse_options(argv)
    # Guard output/input aliases even when qualification fails before launch.
    try:
        require(options.output.resolve() not in protected_inputs(options),
                "report_overwrites_input")
        report = asyncio.run(qualify(options))
        write_report(report, options.output)
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
