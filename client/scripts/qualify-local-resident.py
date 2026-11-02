#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Development observations of real local Tutor + ACP/GGUF residency.

Review API:
  options = parse_options([...])
  report = await run(options)       # actual processes/inference, schedule explicitly
  main([...])                      # same, exclusive new JSON report

After reserving the execution host for this campaign, example:
  python3 -B client/scripts/qualify-local-resident.py probe --repo /path/to/repo \
    --agent /absolute/tutor-local-agent --server /tmp/tutor-ux-server \
    --model /absolute/Qwen3-1.7B-Q4_K_M.gguf --output /tmp/resident-new.json
Optional: --welcome-turns 100 (100 direct Rust welcomes, only ONE extra LLM turn).

Fixed same binary, paired copied fixture databases, bothOff versus bothOn,
one agent process per variant, one inference at a time. No downloads, institution
data, CPU/GPU adoption, held-out corpus, pedagogy or Windows/P2000 qualification.
Reports first TEXT, never first USEFUL. No p95, retuning, answer shortening or
rerun-on-failure. Requested flags and metadata are not independent attestation.

The private _relay subcommand forwards the real server's stdio verbatim and
counts explicit initialize/tools-list/call frames. It records NO arguments,
credentials or paths. Absent initializeTotalMs/catalogPages remain null in the
agent DTO; observed wire counts are separate, not invented wrapper counters.
"""

import argparse
import asyncio
from collections import Counter
from contextlib import closing
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import shutil
import sqlite3
import sys
import tempfile
import threading
import time
import uuid

sys.dont_write_bytecode = True
MODEL_SHA = 'b139949c5bd74937ad8ed8c8cf3d9ffb1e99c866c823204dc42c0d91fa181897'
MODEL_BYTES = 1107409472
ENV_KEYS = ('PATH', 'LANG', 'SystemRoot', 'WINDIR', 'TEMP', 'TMP', 'TMPDIR')
PROFILE_PROMPT = 'donne moi mon profil'
NAME_PROMPT = 'Quel est le nom exact de la formation active ? Réponds uniquement par ce nom.'
MCP_FRAME_LIMIT = 8 * 1024 * 1024
AUDIT_LIMIT = 1024 * 1024
OWNER = {'profileId': 'resident-fixture', 'accountId': 'resident-fixture', 'tenantId': 'resident-fixture', 'serverId': 'resident-fixture'}
VARIANTS = {'bothOff': ('off', 'off'), 'bothOn': ('on', 'on')}


def helpers(repo):
    path = repo / 'client/scripts/qualify-local-context.py'
    spec = importlib.util.spec_from_file_location('resident_context_probe', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module, module.probe


def safe_env(conversation=None):
    result = {key: os.environ[key] for key in ENV_KEYS if key in os.environ}
    if conversation is not None:
        result['TUTOR_CONVERSATION_ID'] = conversation
    return result


def environment():
    result = {'platform': sys.platform, 'logicalCpus': os.cpu_count()}
    if hasattr(os, 'getloadavg'):
        result['loadavg'] = list(os.getloadavg())
    try:
        row = next(x for x in Path('/proc/self/cgroup').read_text().splitlines() if x.startswith('0::'))
        folder = Path('/sys/fs/cgroup') / row.split('::', 1)[1].lstrip('/')
        if not (folder / 'cpu.stat').is_file():
            folder = Path('/sys/fs/cgroup')
        keys = {'usage_usec', 'user_usec', 'system_usec', 'nr_throttled', 'throttled_usec'}
        result['cgroupCpu'] = {key: int(value) for key, value in (line.split() for line in (folder / 'cpu.stat').read_text().splitlines()) if key in keys}
        result['cgroupCpuMax'] = (folder / 'cpu.max').read_text().strip()
    except (OSError, StopIteration, ValueError):
        result['cgroupCpu'] = None
    return result


def add_check(target, name, condition):
    target.setdefault('checks', []).append({'name': name, 'passed': bool(condition)})


def number(value):
    return type(value) in (int, float) and math.isfinite(value) and value >= 0


def checked_runtime(value, probe):
    """Allowlist content-free DTO; unexpected fields fail instead of leaking."""
    probe.require(type(value) is dict, 'runtime_observation_missing')
    probe.require(set(value) <= {'schemaVersion', 'componentScope', 'inference', 'mcp', 'mcpConnection', 'workerElapsedMs'}, 'runtime_fields_changed')
    probe.require(value.get('schemaVersion') == 1 and value.get('componentScope') == 'native_agent', 'runtime_version_changed')
    sections = {
        'inference': {'schemaVersion', 'timingKind', 'elapsedMs', 'operations', 'phases', 'counts', 'firstTokenMs', 'firstEmissionMs', 'firstUsefulResponseMs', 'gpuSynchronizedMs', 'cpuTimeMs', 'lastOutcome'},
        'operations': {'countInputMs', 'generationMs'},
        'phases': {'modelOpenMs', 'modelHashMs', 'backendSetupMs', 'modelLoadMs', 'modelValidationMs', 'templateMs', 'tokenizationMs', 'contextAllocationMs', 'batchAllocationMs', 'prefillMs', 'samplerSetupMs', 'samplingMs', 'decodeMs', 'tokenConversionMs', 'emitMs', 'bridgeWaitMs', 'cacheMaintenanceMs'},
        'counts': {'countInputCalls', 'countedInputTokens', 'generationCalls', 'templateCalls', 'tokenizationCalls', 'inputTokens', 'reusedTokens', 'evaluatedInputTokens', 'outputTokens', 'modelLoadAttempts', 'modelLoadCount', 'contextAllocationAttempts', 'contextAllocations', 'failedOperations', 'cancelledOperations'},
        'mcp': {'schemaVersion', 'connectTotalMs', 'initializeTotalMs', 'catalogTotalMs', 'catalogPages', 'toolsCallTotalMs', 'closeTotalMs', 'catalogAttempts', 'catalogTransportSuccesses', 'catalogTransportFailures', 'catalogInterrupted', 'callAttempts', 'callTransportSuccesses', 'callTransportFailures', 'callInterrupted', 'toolResultErrors', 'closeAttempts', 'closeTransportSuccesses', 'closeTransportFailures', 'closeInterrupted'},
        'mcpConnection': {'schemaVersion', 'connectionReused', 'retained', 'catalogReused', 'catalog', 'restartRequired', 'priorCleanup'},
        'catalog': {'schemaVersion', 'cacheHits', 'cacheMisses', 'lastMissReason'},
    }
    def section(item, kind):
        if item is None:
            return
        probe.require(type(item) is dict and set(item) <= sections[kind], 'observation_fields_changed')
        if kind == 'catalog':
            probe.require(set(item) == sections[kind] and type(item['schemaVersion']) is int and item['schemaVersion'] == 1, 'catalog_observation_version_changed')
            probe.require(all(type(item[key]) is int and 0 <= item[key] < 2**64 for key in ('cacheHits', 'cacheMisses')), 'invalid_catalog_counter')
            reasons = {'disabled', 'cold', 'scope_changed', 'expired', 'real_notification', 'unreviewed_call', 'tool_error', 'unusable_transport', 'idle_notifications_unobservable', 'capability_missing', 'permission_boundary', 'fetch_changed', 'fetch_failure'}
            reason = item['lastMissReason']
            probe.require(reason is None or (type(reason) is str and reason in reasons), 'invalid_catalog_miss_reason')
            return
        for key, child in item.items():
            if key in ('operations', 'phases', 'counts'):
                section(child, key)
            elif key == 'catalog':
                section(child, 'catalog')
            elif key == 'priorCleanup':
                probe.require(type(child) is list and len(child) <= 32, 'invalid_cleanup_observation')
                for cleanup in child:
                    section(cleanup, 'mcp')
            elif isinstance(child, str):
                probe.require(child in {'host_wall_clock', 'counted', 'end_turn', 'max_tokens', 'cancelled', 'failed'}, 'observation_text_not_allowlisted')
            else:
                probe.require(child is None or type(child) is bool or number(child), 'invalid_observation_scalar')
    for kind in ('inference', 'mcp', 'mcpConnection'):
        section(value.get(kind), kind)
    probe.require(value.get('workerElapsedMs') is None or number(value['workerElapsedMs']), 'invalid_worker_duration')
    return value


def audit_counts(path, probe):
    if not path.exists():
        return Counter()
    probe.require(path.stat().st_size <= AUDIT_LIMIT, 'audit_budget_exceeded')
    counts = Counter()
    allowed = {'relay_start', 'relay_end', 'relay_error', 'initialize_request', 'initialize_success', 'catalog_request', 'catalog_page', 'profile_dispatch', 'unexpected_tool_dispatch'}
    for line in path.read_text().splitlines():
        item = json.loads(line)
        probe.require(set(item) == {'event', 'instance'} and item['event'] in allowed, 'audit_invalid')
        counts[item['event']] += 1
    return counts


def audit_delta(before, after):
    return {key: after[key] - before[key] for key in sorted(set(before) | set(after))}


async def stop_owned(process, probe):
    receipt = await probe.stop_process(process, 5)
    # A relay's child inherits its process group so a killed relay cannot leave
    # an orphan in a separate group. stop_process's killpg fallback assumes a
    # group leader; directly kill/reap this nonleader if graceful EOF failed.
    if not receipt['reaped']:
        try:
            process.kill()
        except ProcessLookupError:
            pass
        try:
            await asyncio.wait_for(process.wait(), 5)
        except asyncio.TimeoutError:
            pass
        receipt.update({'forced': True, 'reaped': process.returncode is not None, 'exit_code': process.returncode})
    return receipt


async def relay(options):
    """Private transparent real-server relay. No protocol payload enters audit."""
    _, probe = helpers(options.repo)
    instance = uuid.uuid4().hex
    def event(name):
        encoded = (json.dumps({'event': name, 'instance': instance}) + '\n').encode()
        fd = os.open(options.audit, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
        try:
            os.write(fd, encoded)
        finally:
            os.close(fd)
    process, tasks = None, []
    pending = {}
    queue = asyncio.Queue(maxsize=4)
    loop = asyncio.get_running_loop()
    def stdin_reader():
        try:
            while True:
                line = sys.stdin.buffer.readline(MCP_FRAME_LIMIT + 1)
                asyncio.run_coroutine_threadsafe(queue.put(line), loop).result(timeout=30)
                if not line or len(line) > MCP_FRAME_LIMIT:
                    return
        except Exception:
            return
    try:
        event('relay_start')
        process = await asyncio.create_subprocess_exec(str(options.server), '--local', '--data-dir', str(options.data_dir), env=safe_env(os.environ.get('TUTOR_CONVERSATION_ID')), stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL, limit=MCP_FRAME_LIMIT, start_new_session=False)
        threading.Thread(target=stdin_reader, daemon=True).start()
        async def incoming():
            while True:
                line = await queue.get()
                if not line:
                    process.stdin.close()
                    return
                probe.require(len(line) <= MCP_FRAME_LIMIT and line.endswith(b'\n'), 'relay_frame_invalid')
                value = json.loads(line)
                method = value.get('method')
                if 'id' in value and method in ('initialize', 'tools/list', 'tools/call'):
                    pending[value['id']] = method
                    if method == 'initialize':
                        event('initialize_request')
                    elif method == 'tools/list':
                        event('catalog_request')
                    else:
                        event('profile_dispatch' if value.get('params', {}).get('name') == 'get_learner_context' else 'unexpected_tool_dispatch')
                process.stdin.write(line)
                await process.stdin.drain()
        async def outgoing():
            while True:
                line = await process.stdout.readline()
                if not line:
                    return
                probe.require(len(line) <= MCP_FRAME_LIMIT and line.endswith(b'\n'), 'relay_frame_invalid')
                value = json.loads(line)
                method = pending.pop(value.get('id'), None)
                if method == 'initialize' and isinstance(value.get('result'), dict):
                    event('initialize_success')
                elif method == 'tools/list' and isinstance(value.get('result', {}).get('tools'), list):
                    event('catalog_page')
                sys.stdout.buffer.write(line)
                sys.stdout.buffer.flush()
        tasks = [asyncio.create_task(incoming()), asyncio.create_task(outgoing())]
        done, _ = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        for task in done:
            task.result()
    except (OSError, ValueError, RuntimeError, probe.QualificationError, asyncio.TimeoutError):
        event('relay_error')
    finally:
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        if process is not None:
            receipt = await stop_owned(process, probe)
            if not receipt['reaped']:
                event('relay_error')
        event('relay_end')


def expected_profile_arguments(training):
    """Reviewed live get_learner_context schema accepts the verified domain."""
    domain = training.get('domain_id')
    return {'domain_id': domain} if isinstance(domain, str) and domain else {}


class Acp:
    def __init__(self, process, probe, audit, deadline):
        self.process, self.probe, self.audit, self.deadline = process, probe, audit, deadline
        self.wire, self.identifier = probe.Wire(process), 0

    async def rpc(self, method, params, allow_error=False):
        self.identifier += 1
        deadline = min(self.deadline, time.monotonic() + 30)
        await self.wire.send(method, params, deadline, self.identifier)
        value = await self.wire.receive(deadline)
        self.probe.require('method' not in value and type(value.get('id')) is int and value['id'] == self.identifier, 'rpc_replayed_or_mismatched')
        if 'error' in value:
            self.probe.require(allow_error, 'rpc_failed')
            return {'errorCode': value['error'].get('code')}
        self.probe.require(type(value.get('result')) is dict, 'rpc_result_invalid')
        return value['result']

    async def prompt(self, session, training, text, label, allow, timeout, sink, session_label):
        self.identifier += 1
        deadline = min(self.deadline, time.monotonic() + timeout)
        row = {'case': label, 'sessionLabel': session_label, 'prompt': text, 'permissionAllowed': allow, 'permissions': 0, 'permissionOperationIds': [], 'permissionRpcIds': [], 'toolUpdates': [], 'observedDispatchesBeforePermission': [], 'firstTextMs': None, 'outcome': 'running', 'checks': [], 'environmentBefore': environment()}
        sink.append(row)  # Preserve partial evidence on interruption/errors too.
        before = audit_counts(self.audit, self.probe)
        started, chunks, tool_statuses = time.monotonic(), [], []
        try:
            await self.wire.send('session/prompt', {'sessionId': session, 'prompt': [{'type': 'text', 'text': text}], '_meta': {'tutorLocal': {'task': {'conversationId': training['conversation_id'], 'language': 'fr', 'trainingContext': training}}}}, deadline, self.identifier)
            while True:
                value = await self.wire.receive(deadline)
                method = value.get('method')
                if method == 'session/request_permission':
                    params = value.get('params', {})
                    raw = params.get('toolCall', {}).get('rawInput', {})
                    self.probe.require(params.get('sessionId') == session and raw.get('server') == 'tutor' and raw.get('tool') == 'get_learner_context' and raw.get('arguments') == expected_profile_arguments(training), 'permission_not_expected_read')
                    operation_id = params.get('toolCall', {}).get('toolCallId')
                    self.probe.require(type(operation_id) is str and 0 < len(operation_id) <= 128 and not any(ord(char) < 32 for char in operation_id) and value.get('id') == 'permission:' + operation_id, 'permission_operation_id_mismatch')
                    dispatched = audit_delta(before, audit_counts(self.audit, self.probe)).get('profile_dispatch', 0)
                    row['observedDispatchesBeforePermission'].append(dispatched)
                    self.probe.require(dispatched == 0, 'profile_dispatched_before_permission')
                    row['permissionOperationIds'].append(operation_id)
                    row['permissionRpcIds'].append(value['id'])
                    row['permissions'] += 1
                    self.probe.require(row['permissions'] == 1, 'multiple_permissions')
                    choice = next((x for x in params.get('options', []) if x.get('kind') == ('allow_once' if allow else 'reject_once')), None)
                    self.probe.require(choice is not None, 'permission_choice_missing')
                    frame = {'jsonrpc': '2.0', 'id': value['id'], 'result': {'outcome': {'outcome': 'selected', 'optionId': choice['optionId']}}}
                    self.process.stdin.write(json.dumps(frame).encode() + b'\n')
                    await asyncio.wait_for(self.process.stdin.drain(), self.probe.remaining(deadline))
                elif method == 'session/update':
                    self.probe.require(value.get('params', {}).get('sessionId') == session, 'wrong_update_session')
                    update = value['params'].get('update', {})
                    if update.get('sessionUpdate') == 'agent_message_chunk':
                        content = update.get('content', {})
                        self.probe.require(content.get('type') == 'text' and isinstance(content.get('text'), str), 'invalid_agent_chunk')
                        piece = content['text']
                        if piece and row['firstTextMs'] is None:
                            row['firstTextMs'] = (time.monotonic() - started) * 1000
                        chunks.append(piece)
                        self.probe.require(sum(len(p.encode()) for p in chunks) <= self.probe.MAX_TEXT, 'answer_budget_exceeded')
                    else:
                        self.probe.require(update.get('sessionUpdate') in ('tool_call', 'tool_call_update') and update.get('title') == 'get_learner_context', 'unexpected_update')
                        operation_id = update.get('toolCallId')
                        self.probe.require(type(operation_id) is str and 0 < len(operation_id) <= 128, 'tool_update_operation_id_missing')
                        row['toolUpdates'].append({'operationId': operation_id, 'status': update.get('status')})
                        tool_statuses.append(update.get('status'))
                else:
                    self.probe.require(method is None and type(value.get('id')) is int and value['id'] == self.identifier, 'unexpected_agent_message')
                    if 'error' in value:
                        row['rpcErrorCode'] = value['error'].get('code')
                        meta = value['error'].get('data', {}).get('_meta', {})
                        row['outcome'] = 'rpc_error'
                    else:
                        body = value.get('result', {})
                        row['stopReason'] = body.get('stopReason')
                        meta = body.get('_meta', {})
                        stats = meta.get('tutor', {}).get('generation')
                        self.probe.require(type(stats) is dict and set(stats) == {'inputTokens', 'outputTokens', 'elapsedMs'} and all(type(stats[k]) is int and stats[k] >= 0 for k in stats), 'generation_metadata_invalid')
                        self.probe.require(stats['inputTokens'] <= 2048 and stats['outputTokens'] <= 96, 'generation_limits_invalid')
                        row['generation'] = stats
                        projection = meta.get('tutor', {}).get('context')
                        self.probe.require(projection is None or type(projection) is dict and set(projection) <= {'inputTokens', 'includedRecords', 'omittedRecords'} and all(type(v) is int and v >= 0 for v in projection.values()), 'context_metadata_changed')
                        row['context'] = projection
                        row['outcome'] = 'completed'
                    row['observation'] = checked_runtime(meta.get('tutor', {}).get('runtime'), self.probe)
                    break
        except asyncio.TimeoutError:
            row['outcome'], row['error'] = 'timeout', 'turn_timeout'
        except self.probe.QualificationError as error:
            row['outcome'], row['error'] = 'failed', error.args[0]
        except asyncio.CancelledError:
            row['outcome'], row['error'] = 'interrupted', 'turn_interrupted'
            raise
        except (OSError, ValueError, KeyError, TypeError, RuntimeError):
            row['outcome'], row['error'] = 'failed', 'turn_local_execution_error'
        finally:
            row['totalMs'] = (time.monotonic() - started) * 1000
            row['answerSynthetic'] = ''.join(chunks)  # Whole answer; no shortening/repair.
            row['toolStatuses'] = tool_statuses
            row['wireObservedDelta'] = audit_delta(before, audit_counts(self.audit, self.probe))
            row['environmentAfter'] = environment()
        return row


async def seed_fixture(options, directory, probe, context, conversation, name, objective, report):
    process = await asyncio.create_subprocess_exec(str(options.server), '--local', '--data-dir', str(directory), env=safe_env(conversation), stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL, limit=probe.MAX_FRAME, start_new_session=os.name != 'nt')
    wire, identifier = probe.Wire(process), 0
    async def rpc(method, params):
        nonlocal identifier
        identifier += 1
        deadline = min(report['_deadline'], time.monotonic() + 30)
        await wire.send(method, params, deadline, identifier)
        return await wire.response(identifier, deadline)
    try:
        await rpc('initialize', {'protocolVersion': '2025-03-26', 'capabilities': {}, 'clientInfo': {'name': 'resident-local-fixture', 'version': '1'}})
        await wire.send('notifications/initialized', {}, min(report['_deadline'], time.monotonic() + 10))
        if objective is not None:
            context.payload(await rpc('tools/call', {'name': 'update_learner_profile', 'arguments': {'objective': objective, 'device': 'laptop', 'language': 'fr', 'idempotency_key': str(uuid.uuid4())}}))
        context.payload(await rpc('tools/call', {'name': 'init_domain', 'arguments': {'name': name, 'concepts': ['sol'], 'prerequisites': {}, 'idempotency_key': str(uuid.uuid4())}}))
        training = context.payload(await rpc('tools/call', {'name': 'get_conversation_context', 'arguments': {'conversation_id': conversation}}))
        probe.require(training.get('name') == name and training.get('conversation_id') == conversation, 'fixture_context_not_confirmed')
        return training
    finally:
        receipt = await stop_owned(process, probe)
        report.setdefault('fixtureShutdowns', []).append(receipt)
        probe.require(receipt['reaped'] and not receipt['forced'], 'fixture_shutdown_failed')


def server_config(options, directory, conversation, audit):
    return {'name': 'tutor', 'command': sys.executable, 'args': ['-B', str(Path(__file__).resolve()), '_relay', '--repo', str(options.repo), '--server', str(options.server), '--data-dir', str(directory), '--audit', str(audit)], 'env': [{'name': 'TUTOR_CONVERSATION_ID', 'value': conversation}]}


def session_params(root, servers):
    return {'cwd': str(root), 'mcpServers': servers, '_meta': {'tutorLocal': {'owner': OWNER}}}


def check_generated(row, name, other_name, cache_on, expected_reuse=None):
    obs = row.get('observation') or {}
    counts = (obs.get('inference') or {}).get('counts') or {}
    stats = row.get('generation') or {}
    add_check(row, 'end_turn', row.get('stopReason') == 'end_turn')
    add_check(row, 'confirmed_name_grounded', name in row['answerSynthetic'] and other_name not in row['answerSynthetic'])
    row['checks'][-1]['category'] = 'development_model_output'
    add_check(row, 'exact_name_only_without_repair', row['answerSynthetic'].strip() == name)
    row['checks'][-1]['category'] = 'development_model_output'
    add_check(row, 'no_tools_or_permissions', row['permissions'] == 0 and row['wireObservedDelta'].get('profile_dispatch', 0) == 0)
    add_check(row, 'one_generation', counts.get('generationCalls') == 1)
    add_check(row, 'same_logical_input_as_stats', counts.get('inputTokens') == stats.get('inputTokens') and type(stats.get('inputTokens')) is int)
    add_check(row, 'exact_context_budget_with_output_and_margin', type(stats.get('inputTokens')) is int and 0 < stats['inputTokens'] and stats['inputTokens'] + 96 + 256 <= 2048)
    reused, evaluated = counts.get('reusedTokens'), counts.get('evaluatedInputTokens')
    add_check(row, 'input_equals_reused_plus_evaluated', type(reused) is int and type(evaluated) is int and counts.get('inputTokens') == reused + evaluated)
    add_check(row, 'one_resident_model_load', counts.get('modelLoadCount') == 1)
    add_check(row, 'no_prefix_reuse_when_disabled', cache_on or reused == 0)
    if expected_reuse is not None:
        add_check(row, 'expected_prefix_reuse' if expected_reuse else 'binding_change_reuses_zero_tokens', type(reused) is int and (reused > 0 if expected_reuse else reused == 0))
    add_check(row, 'first_useful_not_claimed', (obs.get('inference') or {}).get('firstUsefulResponseMs') is None)


def check_profile(row, objective, allowed, reused):
    obs = row.get('observation') or {}
    mcp, connection = obs.get('mcp') or {}, obs.get('mcpConnection') or {}
    delta = row['wireObservedDelta']
    add_check(row, 'one_real_permission', row['permissions'] == 1)
    add_check(row, 'expected_grounding_or_denial', objective in row['answerSynthetic'] if allowed else objective not in row['answerSynthetic'])
    add_check(row, 'dispatch_matches_permission', delta.get('profile_dispatch', 0) == int(allowed) and mcp.get('callAttempts') == int(allowed))
    add_check(row, 'fresh_catalog_every_lease', delta.get('catalog_request', 0) >= 1 and delta.get('catalog_page', 0) >= 1 and type(mcp.get('catalogAttempts')) is int and mcp['catalogAttempts'] >= 1)
    add_check(row, 'connection_reused_metadata', type(connection.get('connectionReused')) is bool and connection['connectionReused'] == reused)
    add_check(row, 'catalog_not_claimed_reused', connection.get('catalogReused') is False)
    add_check(row, 'no_unexpected_tool_dispatch', delta.get('unexpected_tool_dispatch', 0) == 0)
    add_check(row, 'no_model_generation_for_profile', (row.get('generation') or {}).get('inputTokens') == 0 and (row.get('generation') or {}).get('outputTokens') == 0)
    add_check(row, 'end_turn', row.get('stopReason') == 'end_turn')


def inspect_journal(root, sessions, rows, welcome_turns, probe):
    """Only after agent and its transports are stopped; SQLite read-only."""
    report = {'checks': [], 'sessions': {}}
    files = list(root.glob('*/*.sqlite3'))
    add_check(report, 'one_journal_per_created_session', len(files) == len(sessions))
    for label, (session, conversation) in sessions.items():
        matching = [p for p in files if p.stem == session]
        probe.require(len(matching) == 1 and matching[0].stat().st_size <= 32 * 1024 * 1024, 'journal_missing_or_oversized')
        with closing(sqlite3.connect(matching[0].as_uri() + '?mode=ro&immutable=1', uri=True)) as db:
            db.execute('PRAGMA query_only=ON')
            deadline = time.monotonic() + 10
            db.set_progress_handler(lambda: int(time.monotonic() > deadline), 1000)
            turns = db.execute('SELECT id,payload,payload_sha256 FROM turns ORDER BY sequence LIMIT 160').fetchall()
            events = db.execute('SELECT turn_id,kind,payload,payload_sha256 FROM events ORDER BY sequence LIMIT 8193').fetchall()
            probe.require(len(events) <= 8192, 'journal_event_budget')
            probe.require(all(hashlib.sha256(payload).digest() == digest for _, payload, digest in turns), 'turn_digest_invalid')
            probe.require(all(hashlib.sha256(payload).digest() == digest for _, _, payload, digest in events), 'event_digest_invalid')
            decoded = [(turn, kind, json.loads(payload)) for turn, kind, payload, _ in events]
            expected = [r for r in rows if r['sessionLabel'] == label and r['outcome'] == 'completed']
            add_check(report, label + '_all_turns_durable_not_just_64', len(turns) == len(expected))
            add_check(report, label + '_users_and_answers_exact', len(turns) == len(expected) and all(json.loads(turn[1]) == {'user': row['prompt']} and ''.join(value['text'] for owner, kind, value in decoded if owner == turn[0] and kind == 'assistant_chunk') == row['answerSynthetic'] for turn, row in zip(turns, expected)))
            add_check(report, label + '_one_complete_commit_each', all([value for owner, kind, value in decoded if owner == turn[0] and kind == 'turn_finished'] == ['complete'] for turn in turns))
            bindings = [v for _, k, v in decoded if k == 'tutor_conversation_binding']
            add_check(report, label + '_journal_bound_to_conversation', len(bindings) == 1 and bindings[0].get('conversation_id') == conversation)
            headers = [v for _, k, v in decoded if k == 'session_header']
            add_check(report, label + '_journal_model_identity', len(headers) == 1 and headers[0].get('model_sha256') == MODEL_SHA)
            operations = db.execute('SELECT state,mutating FROM operations').fetchall()
            add_check(report, label + '_no_mutations', all(mutating == 0 for _, mutating in operations))
            add_check(report, label + '_no_mutations_or_unknown_calls', all(mutating == 0 and state in ('completed', 'rejected') for state, mutating in operations))
            completed, rejected = sum(state == 'completed' for state, _ in operations), sum(state == 'rejected' for state, _ in operations)
            add_check(report, label + '_dispatch_and_denial_receipts', (completed, rejected) == ((2, 1) if label == 'A' else (1, 0)))
            welcomes = sum(json.loads(payload).get('user') == 'bonjour' for _, payload, _ in turns)
            if label == 'A' and welcome_turns:
                add_check(report, 'all_100_direct_welcomes_stored', welcomes == 100 and len(turns) > 100)
                direct = [r for r in expected if r['prompt'] == 'bonjour']
                add_check(report, 'all_100_welcomes_generated_zero_tokens', len(direct) == 100 and all((r.get('generation') or {}).get('inputTokens') == 0 and (r.get('generation') or {}).get('outputTokens') == 0 for r in direct))
                add_check(report, 'post_100_generation_recorded', any(r['case'] == 'name_after_100_direct_welcomes' for r in expected))
            projections = [value for _, kind, value in decoded if kind == 'context_projection']
            add_check(report, label + '_all_journal_projections_fit_exact_budget', bool(projections) and all(type(p.get('input_tokens')) is int and p['input_tokens'] > 0 and p.get('output_reserved') == 96 and p['input_tokens'] + p['output_reserved'] + 256 <= 2048 for p in projections))
            generated_rows = [(turn, row) for turn, row in zip(turns, expected) if (row.get('generation') or {}).get('inputTokens', 0) > 0]
            add_check(report, label + '_generated_inputs_match_journal_projection', bool(generated_rows) and all([p['input_tokens'] for owner, kind, p in decoded if owner == turn[0] and kind == 'context_projection'] == [row['generation']['inputTokens']] for turn, row in generated_rows))
            aliases = [value.get('evidence_aliases') for _, kind, value in decoded if kind == 'context_projection' and value.get('evidence_aliases')]
            add_check(report, label + '_stable_required_alias_observed', bool(aliases) and all(any(e.get('alias') == 'required_0' for e in a.get('entries', [])) for a in aliases))
            report['sessions'][label] = {'storedTurns': len(turns), 'storedWelcomes': welcomes, 'completedReadOperations': completed, 'rejectedOperations': rejected, 'events': len(events)}
    return report


async def run_variant(options, label, root, seed, trainings, objective, context, probe, report):
    prefix, reuse = VARIANTS[label]
    folder, data, audit = root / label, root / label / 'agent', root / label / 'wire.jsonl'
    folder.mkdir()
    server_data = folder / 'server'
    shutil.copytree(seed, server_data)
    result = {'variant': label, 'requested': {'prefixCache': prefix == 'on', 'reuseMcp': reuse == 'on'}, 'cases': [], 'rebinds': [], 'checks': [], 'errors': [], 'environmentBefore': environment()}
    report['variants'].append(result)
    process, acp, active_session = None, None, None
    sessions = {}
    try:
        argv = [str(options.agent), '--model', str(options.model), '--data-dir', str(data), '--sha256', MODEL_SHA, '--context', '2048', '--max-output', '96', '--threads', '4', '--batch', '128', '--gpu-layers', '0', '--prefix-cache', prefix, '--reuse-mcp', reuse]
        process = await asyncio.wait_for(asyncio.create_subprocess_exec(*argv, env=safe_env(), cwd=folder, stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL, limit=probe.MAX_FRAME, start_new_session=os.name != 'nt'), 30)
        acp = Acp(process, probe, audit, report['_deadline'])
        initialized = await acp.rpc('initialize', {'protocolVersion': 1, 'clientCapabilities': {}})
        probe.require(initialized.get('protocolVersion') == 1 and initialized.get('authMethods') == [], 'agent_initialize_invalid')
        configs = {key: [server_config(options, server_data, training['conversation_id'], audit)] for key, training in trainings.items()}
        new = await acp.rpc('session/new', session_params(folder, configs['A']))
        active_session = new['sessionId']
        sessions['A'] = (active_session, trainings['A']['conversation_id'])
        async def turn(session_label, text, name, allowed=True):
            row = await acp.prompt(sessions[session_label][0], trainings[session_label], text, name, allowed, options.turn_timeout, result['cases'], session_label)
            probe.require(row['outcome'] == 'completed', 'prompt_did_not_complete')
            return row
        async def rebind_identical(name):
            before = audit_counts(audit, probe)
            await acp.rpc('_tutor/rebind', {'sessionId': sessions['A'][0], **session_params(folder, configs['A'])})
            result['rebinds'].append({'case': name, 'strictlyIdenticalConfiguration': True, 'historyReplayObserved': False, 'wireObservedDelta': audit_delta(before, audit_counts(audit, probe))})
        first_profile = await turn('A', PROFILE_PROMPT, 'profile_cold_connection')
        check_profile(first_profile, objective, True, False)
        await rebind_identical('identical_before_second_profile')
        second_profile = await turn('A', PROFILE_PROMPT, 'profile_warm_lease')
        check_profile(second_profile, objective, True, reuse == 'on')
        pair = [first_profile, second_profile]
        add_check(result, 'profile_pair_two_permissions', sum(r['permissions'] for r in pair) == 2)
        add_check(result, 'profile_pair_exact_connect_initialize_count', sum(r['wireObservedDelta'].get('relay_start', 0) for r in pair) == (1 if reuse == 'on' else 2) and sum(r['wireObservedDelta'].get('initialize_success', 0) for r in pair) == (1 if reuse == 'on' else 2))
        add_check(result, 'profile_pair_retained_metadata', all(((r.get('observation') or {}).get('mcpConnection') or {}).get('retained') is (reuse == 'on') for r in pair))
        first = await turn('A', NAME_PROMPT, 'name_cold_model')
        check_generated(first, trainings['A']['name'], trainings['B']['name'], prefix == 'on', False)
        await rebind_identical('identical_before_second_generation')
        second = await turn('A', NAME_PROMPT, 'name_warm_model')
        check_generated(second, trainings['A']['name'], trainings['B']['name'], prefix == 'on', prefix == 'on')
        counts = [((r.get('observation') or {}).get('inference') or {}).get('counts', {}) for r in (first, second)]
        add_check(result, 'one_context_allocation_for_generation_pair', all(type(c.get('contextAllocations')) is int for c in counts) and sum(c['contextAllocations'] for c in counts) == 1)
        denial = await turn('A', PROFILE_PROMPT, 'profile_permission_denied', False)
        check_profile(denial, objective, False, reuse == 'on')
        if options.welcome_turns:
            for index in range(100):
                welcome = await turn('A', 'bonjour', 'welcome_' + str(index + 1))
                add_check(welcome, 'direct_rust_no_generation', (welcome.get('generation') or {}).get('inputTokens') == 0 and (welcome.get('generation') or {}).get('outputTokens') == 0)
                add_check(welcome, 'no_tools_or_permissions', welcome['permissions'] == 0 and welcome['wireObservedDelta'].get('profile_dispatch', 0) == 0)
            after_history = await turn('A', NAME_PROMPT, 'name_after_100_direct_welcomes')
            check_generated(after_history, trainings['A']['name'], trainings['B']['name'], prefix == 'on', prefix == 'on')
        # The old durable session must reject another Tutor conversation before
        # its history can be replayed. A separate session then exercises scope
        # invalidation within the SAME agent process and SAME resident model.
        rejected = await acp.rpc('_tutor/rebind', {'sessionId': sessions['A'][0], **session_params(folder, configs['B'])}, allow_error=True)
        add_check(result, 'old_session_rejects_new_conversation_without_replay', rejected.get('errorCode') == -32602)
        created = await acp.rpc('session/new', session_params(folder, configs['B']))
        active_session = created['sessionId']
        sessions['B'] = (active_session, trainings['B']['conversation_id'])
        add_check(result, 'new_conversation_has_distinct_session', sessions['A'][0] != sessions['B'][0])
        switched_profile = await turn('B', PROFILE_PROMPT, 'profile_new_conversation')
        check_profile(switched_profile, objective, True, False)
        add_check(switched_profile, 'new_binding_real_initialize', switched_profile['wireObservedDelta'].get('initialize_success', 0) == 1)
        switched = await turn('B', NAME_PROMPT, 'name_after_conversation_switch')
        check_generated(switched, trainings['B']['name'], trainings['A']['name'], prefix == 'on', False)
    except probe.QualificationError as error:
        result['errors'].append(error.args[0])
    except asyncio.TimeoutError:
        result['errors'].append('bounded_timeout')
    except asyncio.CancelledError:
        result['errors'].append('variant_interrupted')
        raise
    except (OSError, KeyError, TypeError, ValueError, RuntimeError):
        result['errors'].append('local_execution_error')
    finally:
        if process is not None:
            if result['errors'] and acp is not None and active_session is not None:
                try:
                    await acp.wire.send('session/cancel', {'sessionId': active_session}, time.monotonic() + 2)
                except (OSError, asyncio.TimeoutError, probe.QualificationError):
                    result['errors'].append('cancel_send_failed')
            result['shutdown'] = await stop_owned(process, probe)
            add_check(result, 'agent_reaped_gracefully', result['shutdown']['reaped'] and not result['shutdown']['forced'])
        try:
            result['journal'] = inspect_journal(data, sessions, result['cases'], options.welcome_turns, probe) if result.get('shutdown', {}).get('reaped') else {'notInspected': 'agent_not_reaped'}
            result['wireObservedTotals'] = dict(audit_counts(audit, probe))
            add_check(result, 'no_observed_relay_protocol_error', result['wireObservedTotals'].get('relay_error', 0) == 0)
        except (OSError, sqlite3.Error, probe.QualificationError, ValueError, KeyError):
            result['errors'].append('journal_or_audit_inspection_failed')
        result['environmentAfter'] = environment()
    return result


async def run(options):
    context, probe = helpers(options.repo)
    report = {'schemaVersion': 1, 'status': 'running', 'scope': 'Development real local Tutor/ACP/GGUF observations on execution host only', 'windowsP2000Qualified': False, 'pedagogyQualified': False, 'usefulResponseMeasured': False, 'heldOutCorpusUsed': False, 'startedAt': datetime.now(timezone.utc).isoformat(), 'configuration': {'context': 2048, 'maxOutput': 96, 'threads': 4, 'batch': 128, 'gpuLayers': 0, 'variantsInOrder': options.order, 'welcomeTurns': options.welcome_turns, 'timeoutSeconds': options.timeout, 'turnTimeoutSeconds': options.turn_timeout, 'environmentKeyAllowlist': list(ENV_KEYS), 'mcpServer': 'real_local_stdio'}, 'variants': [], 'errors': [], 'checks': [], 'environmentBefore': environment(), '_deadline': time.monotonic() + options.timeout, 'limits': ['Requested boolean flags and self-reported observations are not independent binary/source attestation.', 'Wire relay counts are distinct from wrapper initializeTotalMs/catalogPages, which may stay null.', 'Hashing warms file cache; cold means process/model cold, not OS cold.', 'One agent process per variant; fixed bothOff then bothOn order may be confounded by VM load.', 'First text is measured; first useful response, UI, coaching, Windows/P2000 and p95 are not measured.', 'Exact-name format failures remain failures with full answers; no retuning or retry.']}
    report['fixtureIdentity'] = {'ownerAuthority': 'synthetic_host_adapter_not_authenticated_desktop', 'cacheScopeAuthority': 'native_scope_derived_from_synthetic_owner_and_fixture_host_context', 'serverEvidence': 'real_local_server_on_disposable_data_only', 'limit': 'No proof of institution authentication, production account isolation, or independent executable/source correspondence.'}
    report['limits'].append('Transparent stdio observation relay adds transport startup/forwarding cost; this is not the uninstrumented packaged client path.')
    try:
        probe.require(options.order in (['bothOff', 'bothOn'], ['bothOn', 'bothOff']), 'invalid_variant_order')
        probe.require(not options.output.exists(), 'output_already_exists')
        report['artifactsBefore'] = {name: probe.fingerprint(getattr(options, name), report['_deadline']) for name in ('agent', 'server', 'model')}
        model = report['artifactsBefore']['model']
        probe.require(model['sha256'] == MODEL_SHA and model['bytes'] == MODEL_BYTES, 'pinned_model_mismatch')
        if options.agent_sha256 is not None:
            probe.require(report['artifactsBefore']['agent']['sha256'] == options.agent_sha256.lower(), 'agent_hash_mismatch')
        source_paths = ('client/scripts/qualify-local-agent.py', 'client/scripts/qualify-local-context.py',
            'client/local-agent/model-profile.json', 'client/local-agent/Cargo.toml', 'client/local-agent/Cargo.lock',
            'client/local-agent/src/runtime.rs', 'client/local-agent/src/acp.rs', 'client/local-agent/src/config.rs',
            'client/local-agent/src/main.rs', 'client/local-agent/src/lib.rs', 'client/local-agent/src/harness.rs',
            'client/local-agent/src/fast_harness.rs', 'client/local-agent/src/mcp_session.rs',
            'client/local-agent/src/context.rs', 'client/local-agent/src/session_store.rs', 'client/local-agent/src/host_context.rs')
        report['sourceFiles'] = {name: probe.fingerprint(options.repo / name, report['_deadline']) for name in source_paths}
        report['probeScript'] = probe.fingerprint(Path(__file__).resolve(), report['_deadline'])
        profile = json.loads((options.repo / 'client/local-agent/model-profile.json').read_text())
        probe.require(profile.get('gguf_sha256') == MODEL_SHA and profile.get('gguf_bytes') == MODEL_BYTES, 'repository_profile_changed')
        report['declaredModelProfile'] = {key: profile.get(key) for key in ('id', 'kv_type', 'flash_attention', 'thinking', 'sampling', 'llama_cpp_2', 'llama_cpp_sys_2')}
        report['pythonVersion'] = '.'.join(map(str, sys.version_info[:3]))
        with tempfile.TemporaryDirectory(prefix='tutor-resident-probe-') as temporary:
            root = Path(temporary).resolve()
            seed = root / 'seed'
            nonce = uuid.uuid4().hex[:10]
            objective = 'Cultiver ' + nonce
            trainings = {}
            for label, prefix in (('A', 'Potager résident '), ('B', 'Électricité résidente ')):
                trainings[label] = await seed_fixture(options, seed, probe, context, str(uuid.uuid4()), prefix + nonce, objective if label == 'A' else None, report)
            report['fixture'] = {'origin': 'fresh_local_only', 'trainingNamesSynthetic': [v['name'] for v in trainings.values()], 'objectiveSynthetic': objective, 'pairedSeedDatabaseCopiedAfterShutdown': True}
            for label in options.order:
                probe.remaining(report['_deadline'])
                await run_variant(options, label, root, seed, trainings, objective, context, probe, report)
            report['artifactsAfter'] = {name: probe.fingerprint(getattr(options, name), report['_deadline']) for name in ('agent', 'server', 'model')}
            source_after = {name: probe.fingerprint(options.repo / name, report['_deadline']) for name in source_paths}
            add_check(report, 'source_content_unchanged_during_probe', report['sourceFiles'] == source_after)
            add_check(report, 'binary_server_model_content_unchanged', report['artifactsBefore'] == report['artifactsAfter'])
            add_check(report, 'one_agent_process_per_variant', len(report['variants']) == 2)
    except probe.QualificationError as error:
        report['errors'].append(error.args[0])
    except asyncio.TimeoutError:
        report['errors'].append('campaign_timeout')
    except asyncio.CancelledError:
        report['errors'].append('campaign_interrupted')
    except (OSError, ValueError, RuntimeError, KeyError, TypeError):
        report['errors'].append('local_execution_error')
    finally:
        report.pop('_deadline', None)
        report['environmentAfter'] = environment()
        checks = report['checks'] + [check for variant in report['variants'] for check in variant['checks']] + [check for variant in report['variants'] for row in variant['cases'] for check in row['checks']] + [check for variant in report['variants'] for check in variant.get('journal', {}).get('checks', [])]
        errors = report['errors'] + [error for variant in report['variants'] for error in variant['errors']]
        report['checksSummary'] = {'observed': len(checks), 'passed': sum(c['passed'] for c in checks), 'failed': sum(not c['passed'] for c in checks)}
        report['developmentModelFailures'] = [{'variant': variant['variant'], 'case': row['case'], 'check': check['name']} for variant in report['variants'] for row in variant['cases'] for check in row['checks'] if check.get('category') == 'development_model_output' and not check['passed']]
        report['status'] = 'observations_complete' if not errors and len(report['variants']) == 2 and checks and all(c['passed'] for c in checks) else 'checks_failed_or_incomplete'
        report['completedAt'] = datetime.now(timezone.utc).isoformat()
    return report


def parse_options(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest='command', required=True)
    public = commands.add_parser('probe', help='Run only in an explicitly reserved slot')
    private = commands.add_parser('_relay', help=argparse.SUPPRESS)
    for child in (public, private):
        child.add_argument('--repo', type=Path, required=True)
        child.add_argument('--server', type=Path, required=True)
    for name in ('agent', 'model', 'output'):
        public.add_argument('--' + name, type=Path, required=True)
    public.add_argument('--agent-sha256')
    public.add_argument('--order', nargs=2, choices=tuple(VARIANTS), default=['bothOff', 'bothOn'])
    public.add_argument('--welcome-turns', type=int, choices=(0, 100), default=0)
    public.add_argument('--timeout', type=int, default=1800)
    public.add_argument('--turn-timeout', type=int, default=180)
    private.add_argument('--data-dir', type=Path, required=True)
    private.add_argument('--audit', type=Path, required=True)
    options = parser.parse_args(argv)
    options.repo = options.repo.resolve(strict=True)
    options.server = options.server.resolve(strict=True)
    if options.command == 'probe':
        for name in ('agent', 'model'):
            setattr(options, name, getattr(options, name).resolve(strict=True))
        options.output = options.output.resolve()
        if not 60 <= options.timeout <= 3600 or not 30 <= options.turn_timeout <= 600 or options.output.exists():
            parser.error('invalid_limits_or_output_exists')
        if options.agent_sha256 is not None and (len(options.agent_sha256) != 64 or any(c not in '0123456789abcdefABCDEF' for c in options.agent_sha256)):
            parser.error('invalid_agent_sha256')
    return options


def main(argv=None):
    options = parse_options(argv)
    if options.command == '_relay':
        asyncio.run(relay(options))
        return 0
    report = asyncio.run(run(options))
    with options.output.open('x', encoding='utf-8') as stream:
        json.dump(report, stream, ensure_ascii=False, indent=2, allow_nan=False)
        stream.write('\n')
    print(json.dumps({'status': report['status'], 'checksSummary': report['checksSummary'], 'windowsP2000Qualified': False, 'pedagogyQualified': False}))
    return 0 if report['status'] == 'observations_complete' else 1


if __name__ == '__main__':
    sys.exit(main())
