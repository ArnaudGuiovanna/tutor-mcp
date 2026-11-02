#!/usr/bin/env python3
"""Development-only first semantic ACP probe; model execution is explicit.

No imports spawn processes. Real execution requires --execute and a render
contract marked CONFIRMED_BEFORE_FIRST_RUN by the integrator BEFORE observing
model answers. Each campaign is immutable. No held-out, UI, hardware or accuracy claims.
Canonical helpers supply Wire, RPC parsing, metadata validation, fixture seeding,
shutdown and exclusive atomic publication. This sidecar adds only two-tool
matched permissions, an exact wire observer and eight development cases.
"""
import argparse
import asyncio
from contextlib import closing
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import sys
import tempfile
import threading
import time
import uuid

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
MAX_FRAME = 8 * 1024 * 1024
MAX_AUDIT = 2 * 1024 * 1024
MAX_JOURNAL = 32 * 1024 * 1024
TOOLS = {'get_learner_context', 'list_training_library'}
CASES = (
    ('no_read', 'Sans consulter mon profil ni mes formations, explique ce qu’est un objectif d’apprentissage.', 'no_mcp', True),
    ('quoted_definition', 'Que signifie « donne moi mon profil » ? Explique la phrase sans exécuter la demande.', 'no_mcp', True),
    ('coaching', 'Je débute en jardinage. Propose une première action concrète.', 'no_tool', True),
    ('profile_typo', 'donne moii mon profil', 'profile', True),
    ('profile_paraphrase', 'Tu sais quoi sur moi ?', 'profile', True),
    ('library_question', 'Quels cours je pourrais suivre ?', 'library', True),
    ('library_paraphrase', 'Je voudrais voir les formations accessibles', 'library', True),
    ('profile_denied', 'Tu sais quoi sur moi ?', 'profile', False),
)


class Invalid(Exception):
    pass


def require(condition, code):
    if not condition:
        raise Invalid(code)


def load_helpers(repo):
    path = repo / 'client/scripts/qualify-local-catalog.py'
    spec = importlib.util.spec_from_file_location('semantic_canonical_catalog', path)
    catalog = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(catalog)
    resident = catalog.resident
    context, probe = resident.helpers(repo)
    for module, name in ((catalog, 'catalog'), (resident, 'resident'), (context, 'context'), (probe, 'agent')):
        require(Path(module.__file__).resolve() == (repo / ('client/scripts/qualify-local-' + name + '.py')).resolve(), 'canonical_helper_import_path_mismatch')
    return catalog, resident, context, probe


def check(target, name, condition):
    target.setdefault('checks', []).append({'name': name, 'passed': bool(condition)})


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False, allow_nan=False).encode()


def digest(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def profile_answer(payload, training, contract):
    """Independent frozen rendering contract, not model-response normalization."""
    p = contract['profile']
    lines = [p['heading']]
    values = {'objective': payload.get('objective'), **payload.get('profile', {})}
    for field in ('objective', 'language', 'device', 'affect_baseline'):
        value = values.get(field)
        if isinstance(value, str) and value.strip():
            lines.append(p['fieldLines'][field].format(value=value))
    lines += ['', p['trainingLine'].format(value=training['name'])]
    return '\n'.join(lines)


def library_page(payload, arguments, all_entries):
    """Exact first-page facts/cursor, no ID or cursor supplied by the model."""
    require(type(arguments) is dict and set(arguments) == {'limit'}, 'library_arguments_not_exact_first_page')
    limit = arguments['limit']
    require(type(limit) is int and 1 <= limit <= 100, 'library_limit_invalid')
    require(type(payload) is dict and type(payload.get('items')) is list and type(payload.get('next_after')) is str, 'library_page_missing')
    expected = all_entries[:limit]
    items = payload['items']
    require(len(items) == len(expected), 'library_page_cardinality_wrong')
    keys = ('id', 'name', 'kind', 'revision', 'status', 'can_learn')
    require(all(type(item) is dict and type(want) is dict and all(k in item and k in want and type(item[k]) is type(want[k]) and item[k] == want[k] for k in keys) for item, want in zip(items, expected)), 'library_exact_items_or_order_wrong')
    next_after = expected[-1]['id'] if len(all_entries) > limit else ''
    require(payload['next_after'] == next_after, 'library_cursor_or_completeness_wrong')
    return {'items': items, 'next_after': next_after, 'partial': bool(next_after)}


def library_answer(page, contract):
    p = contract['library']
    if not page['items']:
        return p['emptyLine']
    text = p['heading'] + '\n\n' + '\n'.join(p['itemLine'].format(index=i + 1, name=item['name']) for i, item in enumerate(page['items']))
    if page['partial']:
        text += '\n\n' + p['partialLine']
    return text


def read_audit(path):
    if not path.exists():
        return []
    require(path.stat().st_size <= MAX_AUDIT, 'wire_audit_too_large')
    rows = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]
    require(len(rows) <= 4096, 'wire_audit_too_many_events')
    for item in rows:
        require(type(item) is dict, 'wire_audit_invalid')
        event = item.get('event')
        keys = {'request': {'event', 'method'}, 'reply': {'event', 'method', 'rpcError'}, 'other_frame': {'event', 'direction'}, 'call_sent': {'event', 'tool', 'argumentsSha256'}, 'call_result': {'event', 'tool', 'argumentsSha256', 'resultSha256', 'rpcError'}, 'relay_error': {'event'}, 'relay_end': {'event', 'reaped', 'forced', 'gracefulEof', 'exitCode'}}.get(event)
        require(keys is not None and set(item) == keys, 'wire_audit_fields_invalid')
        if event in ('request', 'reply'):
            require(item['method'] in ('initialize', 'tools/list'), 'wire_audit_method_invalid')
            if event == 'reply':
                require(type(item['rpcError']) is bool, 'wire_audit_outcome_invalid')
        elif event == 'other_frame':
            require(item['direction'] in ('to_server', 'from_server'), 'wire_audit_direction_invalid')
        elif event == 'relay_end':
            require(all(type(item[k]) is bool for k in ('reaped', 'forced', 'gracefulEof')) and (item['exitCode'] is None or type(item['exitCode']) is int), 'wire_audit_shutdown_invalid')
        elif event in ('call_sent', 'call_result'):
            require(item['tool'] in TOOLS | {'UNEXPECTED'}, 'wire_audit_tool_invalid')
            for key in keys & {'argumentsSha256', 'resultSha256'}:
                require(type(item[key]) is str and len(item[key]) == 64 and all(c in '0123456789abcdef' for c in item[key]), 'wire_audit_hash_invalid')
            if event == 'call_result':
                require(type(item['rpcError']) is bool, 'wire_audit_outcome_invalid')
    return rows


def dispatches(events):
    return [event for event in events if event.get('event') == 'call_sent']


def validate_permission(value, session, tool, arguments, previously_sent, probe):
    params = value.get('params', {})
    raw = params.get('toolCall', {}).get('rawInput', {})
    probe.require(params.get('sessionId') == session and raw == {'server': 'tutor', 'tool': tool, 'arguments': arguments}, 'semantic_permission_wrong_tool_or_arguments')
    operation = params.get('toolCall', {}).get('toolCallId')
    probe.require(type(operation) is str and 0 < len(operation) <= 128 and not any(ord(c) < 32 for c in operation) and value.get('id') == 'permission:' + operation, 'semantic_permission_id_mismatch')
    probe.require(previously_sent == 0, 'semantic_dispatch_before_permission')
    return operation


def new_output(path):
    path = path.resolve()
    require(path.is_relative_to(HERE) and path.parent.is_dir() and not path.exists(), 'new_output_inside_sidecar_required')
    return path


async def relay(options, resident, probe):
    """Observer for the exact real Tutor wire; no frame is rewritten."""
    process, tasks, pending = None, [], {}
    loop, queue = asyncio.get_running_loop(), asyncio.Queue(maxsize=4)
    def emit(value):
        fd = os.open(options.audit, os.O_CREAT | os.O_APPEND | os.O_WRONLY, 0o600)
        try:
            os.write(fd, canonical(value) + b'\n')
        finally:
            os.close(fd)
    def stdin_reader():
        try:
            while True:
                data = sys.stdin.buffer.readline(MAX_FRAME + 1)
                asyncio.run_coroutine_threadsafe(queue.put(data), loop).result(timeout=30)
                if not data or len(data) > MAX_FRAME:
                    return
        except Exception:
            return
    def frame(data):
        require(len(data) <= MAX_FRAME and data.endswith(b'\n'), 'relay_frame_bounds')
        value = json.loads(data, object_pairs_hook=probe.unique_object, parse_constant=probe.reject_constant)
        require(type(value) is dict and value.get('jsonrpc') == '2.0', 'relay_rpc_version')
        return value
    try:
        process = await asyncio.create_subprocess_exec(str(options.server), '--local', '--data-dir', str(options.data_dir), env=resident.safe_env(os.environ.get('TUTOR_CONVERSATION_ID')), stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL, limit=MAX_FRAME, start_new_session=False)
        threading.Thread(target=stdin_reader, daemon=True).start()
        async def incoming():
            while True:
                data = await queue.get()
                if not data:
                    process.stdin.close()
                    return
                value = frame(data)
                method = value.get('method')
                if method in ('initialize', 'tools/list', 'tools/call'):
                    key = value.get('id')
                    require(type(key) in (int, str) and key not in pending, 'relay_request_id')
                    entry = {'event': 'request', 'method': method}
                    if method == 'tools/call':
                        tool = value.get('params', {}).get('name')
                        entry = {'event': 'call_sent', 'tool': tool if tool in TOOLS else 'UNEXPECTED', 'argumentsSha256': digest(value.get('params', {}).get('arguments'))}
                    pending[key] = entry
                    emit(entry)
                else:
                    emit({'event': 'other_frame', 'direction': 'to_server'})
                process.stdin.write(data)
                await process.stdin.drain()
        async def outgoing():
            while True:
                data = await process.stdout.readline()
                if not data:
                    return
                value = frame(data)
                key = value.get('id')
                require(key is None or type(key) in (int, str), 'relay_response_id')
                entry = pending.pop(key, None)
                if entry and entry['event'] == 'call_sent':
                    emit({'event': 'call_result', 'tool': entry['tool'], 'argumentsSha256': entry['argumentsSha256'], 'resultSha256': digest(value.get('result')), 'rpcError': 'error' in value})
                elif entry:
                    emit({'event': 'reply', 'method': entry['method'], 'rpcError': 'error' in value})
                else:
                    emit({'event': 'other_frame', 'direction': 'from_server'})
                sys.stdout.buffer.write(data)
                sys.stdout.buffer.flush()
        tasks = [asyncio.create_task(incoming()), asyncio.create_task(outgoing())]
        done, _ = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        for task in done:
            task.result()
    except (OSError, ValueError, TypeError, RuntimeError, Invalid, probe.QualificationError, asyncio.TimeoutError):
        emit({'event': 'relay_error'})
    finally:
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        if process is not None:
            receipt = await resident.stop_owned(process, probe)
            emit({'event': 'relay_end', 'reaped': receipt['reaped'], 'forced': receipt['forced'], 'gracefulEof': receipt['graceful_eof'], 'exitCode': receipt['exit_code']})
            if not receipt['reaped']:
                emit({'event': 'relay_error'})


class SemanticAcp:
    """Canonical RPC helper plus the narrow two-tool permission extension."""
    def __init__(self, process, resident, probe, audit, deadline):
        self.base = resident.Acp(process, probe, audit, deadline)
        self.resident, self.probe, self.audit = resident, probe, audit

    async def rpc(self, *args, **kwargs):
        return await self.base.rpc(*args, **kwargs)

    async def prompt(self, case, session, training, contract, sink, timeout):
        name, text, kind, allowed = case
        tool = {'profile': 'get_learner_context', 'library': 'list_training_library'}.get(kind)
        arguments = self.resident.expected_profile_arguments(training) if kind == 'profile' else {'limit': contract['libraryLimit']} if kind == 'library' else None
        row = {'case': name, 'prompt': text, 'kind': kind, 'permissionAllowed': allowed, 'outcome': 'running', 'checks': [], 'permissionOperationIds': [], 'permissionRpcIds': [], 'toolUpdates': [], 'observedDispatchesBeforePermission': [], 'firstTextMs': None}
        sink.append(row)
        start_audit = len(read_audit(self.audit))
        started, chunks = time.monotonic(), []
        self.base.identifier += 1
        request_id = self.base.identifier
        deadline = min(self.base.deadline, started + timeout)
        wire, process, probe = self.base.wire, self.base.process, self.probe
        try:
            await wire.send('session/prompt', {'sessionId': session, 'prompt': [{'type': 'text', 'text': text}], '_meta': {'tutorLocal': {'task': {'conversationId': training['conversation_id'], 'language': 'fr', 'trainingContext': training}}}}, deadline, request_id)
            while True:
                value = await wire.receive(deadline)
                method = value.get('method')
                if method == 'session/request_permission':
                    probe.require(tool is not None and not row['permissionOperationIds'], 'semantic_unexpected_permission')
                    sent = len(dispatches(read_audit(self.audit)[start_audit:]))
                    operation = validate_permission(value, session, tool, arguments, sent, probe)
                    row['permissionOperationIds'].append(operation)
                    row['permissionRpcIds'].append(value['id'])
                    row['observedDispatchesBeforePermission'].append(sent)
                    choice = next((c for c in value['params'].get('options', []) if c.get('kind') == ('allow_once' if allowed else 'reject_once')), None)
                    probe.require(choice is not None, 'semantic_permission_choice_missing')
                    response = {'jsonrpc': '2.0', 'id': value['id'], 'result': {'outcome': {'outcome': 'selected', 'optionId': choice['optionId']}}}
                    process.stdin.write(canonical(response) + b'\n')
                    await asyncio.wait_for(process.stdin.drain(), probe.remaining(deadline))
                elif method == 'session/update':
                    probe.require(value.get('params', {}).get('sessionId') == session, 'semantic_wrong_session')
                    update = value['params'].get('update', {})
                    if update.get('sessionUpdate') == 'agent_message_chunk':
                        content = update.get('content', {})
                        probe.require(content.get('type') == 'text' and type(content.get('text')) is str, 'semantic_chunk_invalid')
                        piece = content['text']
                        if piece and row['firstTextMs'] is None:
                            row['firstTextMs'] = (time.monotonic() - started) * 1000
                        chunks.append(piece)
                        probe.require(sum(len(c.encode()) for c in chunks) <= probe.MAX_TEXT, 'semantic_text_budget')
                    else:
                        probe.require(tool is not None and update.get('sessionUpdate') in ('tool_call', 'tool_call_update') and update.get('title') == tool, 'semantic_unexpected_update')
                        operation = update.get('toolCallId')
                        probe.require(type(operation) is str and 0 < len(operation) <= 128, 'semantic_tool_update_id')
                        row['toolUpdates'].append({'operationId': operation, 'status': update.get('status')})
                else:
                    probe.require(method is None and type(value.get('id')) is int and value['id'] == request_id, 'semantic_wrong_response_id')
                    if 'error' in value:
                        row['rpcErrorCode'] = value['error'].get('code')
                        row['outcome'] = 'rpc_error'
                        break
                    body = value.get('result', {})
                    meta = body.get('_meta', {}).get('tutor', {})
                    row['generation'] = meta.get('generation')
                    row['stopReason'] = body.get('stopReason')
                    row['observation'] = self.resident.checked_runtime(meta.get('runtime'), probe)
                    stats = row['generation']
                    probe.require(type(stats) is dict and set(stats) == {'inputTokens', 'outputTokens', 'elapsedMs'} and all(type(v) is int and v >= 0 for v in stats.values()), 'semantic_generation_metadata')
                    # Direct reads contain proposer totals. Chat contains proposer
                    # plus answer: enforce limits per durable projection below.
                    probe.require(stats['inputTokens'] <= 4096 and stats['outputTokens'] <= 192, 'semantic_generation_limits')
                    projection = meta.get('context')
                    probe.require(projection is None or type(projection) is dict and set(projection) <= {'inputTokens', 'includedRecords', 'omittedRecords'} and all(type(v) is int and v >= 0 for v in projection.values()), 'semantic_context_metadata')
                    row['context'] = projection
                    row['outcome'] = 'completed'
                    break
        except asyncio.CancelledError:
            row['outcome'], row['error'] = 'interrupted', 'semantic_turn_interrupted'
            raise
        except asyncio.TimeoutError:
            row['outcome'], row['error'] = 'timeout', 'semantic_turn_timeout'
        except (Invalid, probe.QualificationError) as error:
            row['outcome'], row['error'] = 'failed', error.args[0]
        except (OSError, ValueError, KeyError, TypeError, RuntimeError):
            row['outcome'], row['error'] = 'failed', 'semantic_turn_local_error'
        finally:
            row['totalMs'] = (time.monotonic() - started) * 1000
            row['answerSynthetic'] = ''.join(chunks)
            row['wireEvents'] = read_audit(self.audit)[start_audit:]
        check(row, 'completed_end_turn', row['outcome'] == 'completed' and row.get('stopReason') == 'end_turn')
        calls = dispatches(row['wireEvents'])
        check(row, 'only_expected_dispatch', len(calls) == int(tool is not None and allowed) and all(c['tool'] == tool and c['argumentsSha256'] == digest(arguments) for c in calls))
        updates = row['toolUpdates']
        expected_statuses = ['pending', 'in_progress', 'completed'] if allowed else ['pending', 'failed']
        check(row, 'matched_permission', len(row['permissionOperationIds']) == int(tool is not None) and row['permissionRpcIds'] == ['permission:' + operation for operation in row['permissionOperationIds']] and (not updates if tool is None else bool(updates) and [u['status'] for u in updates] == expected_statuses) and all(u['operationId'] in row['permissionOperationIds'] for u in updates))
        check(row, 'no_dispatch_before_permission', row['observedDispatchesBeforePermission'] == ([0] if tool else []))
        if kind == 'no_mcp':
            check(row, 'explicit_no_read_zero_mcp_frames', not row['wireEvents'])
        counts = ((row.get('observation') or {}).get('inference') or {}).get('counts') or {}
        if tool is not None:
            check(row, 'proposer_calls_without_extra_answer_generation', counts.get('generationCalls') == contract['proposerCallsForDirectRead'])
            stats = row.get('generation') or {}
            check(row, 'proposer_tokens_present_in_combined_totals', type(counts.get('inputTokens')) is int and counts['inputTokens'] > 0 and type(counts.get('outputTokens')) is int and counts['outputTokens'] > 0 and stats.get('inputTokens') == counts['inputTokens'] and stats.get('outputTokens') == counts['outputTokens'])
            check(row, 'direct_read_single_call_limits', type(stats.get('inputTokens')) is int and stats['inputTokens'] <= 2048 and type(stats.get('outputTokens')) is int and stats['outputTokens'] <= 96)
        else:
            check(row, 'generated_general_answer_observed', type(counts.get('generationCalls')) is int and counts['generationCalls'] >= 1 and bool(row['answerSynthetic'].strip()))
        return row


async def seed_snapshot(options, resident, context, probe, root, report):
    seed, nonce = root / 'server', uuid.uuid4().hex[:10]
    names = ['Formation Go ' + nonce, 'Formation Jardin ' + nonce]
    training = None
    for i, name in enumerate(names):
        current = await resident.seed_fixture(options, seed, probe, context, str(uuid.uuid4()), name, 'Apprendre ' + nonce if i == 0 else None, report)
        if i == 0:
            training = current
    process = await asyncio.create_subprocess_exec(str(options.server), '--local', '--data-dir', str(seed), env=resident.safe_env(training['conversation_id']), stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL, limit=probe.MAX_FRAME, start_new_session=os.name != 'nt')
    rpc = resident.Acp(process, probe, root / 'unused-seed-audit', report['_deadline'])
    try:
        await rpc.rpc('initialize', {'protocolVersion': '2025-03-26', 'capabilities': {}, 'clientInfo': {'name': 'semantic-local-fixture', 'version': '1'}})
        await rpc.wire.send('notifications/initialized', {}, min(report['_deadline'], time.monotonic() + 10))
        profile = context.payload(await rpc.rpc('tools/call', {'name': 'get_learner_context', 'arguments': resident.expected_profile_arguments(training)}))
        library = context.payload(await rpc.rpc('tools/call', {'name': 'list_training_library', 'arguments': {'limit': 100}}))
        require(type(library.get('items')) is list and len(library['items']) == 2 and library.get('next_after') == '', 'fixture_not_exactly_two_trainings')
        require({e['name'] for e in library['items']} == set(names), 'fixture_training_names_wrong')
        require([e['id'] for e in library['items']] == sorted(e['id'] for e in library['items']), 'fixture_library_not_ordered')
        return seed, training, profile, library['items']
    finally:
        receipt = await resident.stop_owned(process, probe)
        report.setdefault('fixtureShutdowns', []).append(receipt)
        require(receipt['graceful_eof'] and receipt['reaped'] and not receipt['forced'] and receipt['exit_code'] == 0, 'semantic_seed_shutdown_failed')


def inspect_journal(data, session, rows, training, profile, all_entries, contract, context, probe):
    checks = {'checks': [], 'cases': []}
    paths = list(data.glob('*/*.sqlite3'))
    require(len(paths) == 1 and paths[0].stem == session and paths[0].stat().st_size <= MAX_JOURNAL, 'semantic_journal_identity_or_size')
    with closing(sqlite3.connect(paths[0].as_uri() + '?mode=ro&immutable=1', uri=True)) as db:
        deadline = time.monotonic() + 10
        db.set_progress_handler(lambda: int(time.monotonic() > deadline), 1000)
        volume = sum(db.execute('SELECT COALESCE(SUM(length(payload)),0) FROM ' + table).fetchone()[0] for table in ('turns', 'events', 'results'))
        require(volume <= MAX_JOURNAL, 'semantic_journal_payload_budget')
        turns = db.execute('SELECT id,payload,payload_sha256 FROM turns ORDER BY sequence LIMIT 17').fetchall()
        events = db.execute('SELECT turn_id,kind,payload,payload_sha256 FROM events ORDER BY sequence LIMIT 4097').fetchall()
        ops = db.execute('SELECT o.id,o.turn_id,o.tool,o.arguments,o.state,o.mutating,r.payload FROM operations o LEFT JOIN results r ON r.operation_id=o.id LIMIT 17').fetchall()
        transitions = db.execute('SELECT operation_id,from_state,to_state FROM transitions ORDER BY sequence LIMIT 65').fetchall()
        require(len(events) <= 4096 and len(ops) <= 16 and len(transitions) <= 64, 'semantic_journal_row_bounds')
        require(all(hashlib.sha256(p).digest() == h for _, p, h in turns) and all(hashlib.sha256(p).digest() == h for _, _, p, h in events), 'semantic_journal_digest_invalid')
        check(checks, 'all_attempted_turns_present', len(turns) == len(rows))
        decoded = [(turn, kind, json.loads(payload)) for turn, kind, payload, _ in events]
        bindings = [v for _, k, v in decoded if k == 'tutor_conversation_binding']
        check(checks, 'exact_conversation_binding', len(bindings) == 1 and bindings[0].get('conversation_id') == training['conversation_id'])
        headers = [v for _, k, v in decoded if k == 'session_header']
        check(checks, 'pinned_model_binding', len(headers) == 1 and headers[0].get('model_sha256') == profile['_model_sha256'])
        linked_ops = set()
        for turn, row in zip(turns, rows):
            result = {'case': row['case'], 'checks': []}
            checks['cases'].append(result)
            check(result, 'exact_user_and_answer', json.loads(turn[1]) == {'user': row['prompt']} and ''.join(v['text'] for t, k, v in decoded if t == turn[0] and k == 'assistant_chunk') == row['answerSynthetic'])
            check(result, 'complete_durable_commit', [v for t, k, v in decoded if t == turn[0] and k == 'turn_finished'] == ['complete'])
            own = [op for op in ops if op[1] == turn[0]]
            required = row['kind'] in ('profile', 'library')
            check(result, 'expected_operation_cardinality', len(own) == int(required))
            if not required:
                continue
            if len(own) != 1:
                continue
            op = own[0]
            linked_ops.add(op[0])
            result['operationId'], result['turnId'] = op[0], turn[0]
            result['tool'], result['arguments'], result['state'], result['mutating'] = op[2], json.loads(op[3]), op[4], bool(op[5])
            result['transitions'] = [{'from': a, 'to': b} for oid, a, b in transitions if oid == op[0]]
            expected_tool = 'get_learner_context' if row['kind'] == 'profile' else 'list_training_library'
            args = {'domain_id': training['domain_id']} if row['kind'] == 'profile' and training.get('domain_id') else {} if row['kind'] == 'profile' else {'limit': contract['libraryLimit']}
            proposals = [v for t, k, v in decoded if t == turn[0] and k == 'semantic_intent_proposed']
            expected_proposal = {'intent': 'profile'} if row['kind'] == 'profile' else {'intent': 'training_search', 'query': ''}
            check(result, 'exact_semantic_proposal_not_execution_authority', proposals == [expected_proposal])
            own_projections = [p for t, k, p in decoded if t == turn[0] and k == 'context_projection']
            check(result, 'one_proposer_projection_no_answer_generation', len(own_projections) == 1 and own_projections[0].get('input_tokens') == (row.get('generation') or {}).get('inputTokens'))
            check(result, 'exact_tool_args_and_permission_operation', op[2] == 'tutor/' + expected_tool and json.loads(op[3]) == args and row['permissionOperationIds'] == [op[0]])
            allowed = row['permissionAllowed']
            target = [(None, 'prepared'), ('prepared', 'dispatched'), ('dispatched', 'completed')] if allowed else [(None, 'prepared'), ('prepared', 'rejected')]
            check(result, 'exact_transitions', [(a, b) for oid, a, b in transitions if oid == op[0]] == target)
            check(result, 'reviewed_effect_recorded', op[5] == int(row['kind'] == 'library'))
            if not allowed:
                check(result, 'denial_zero_receipt_zero_dispatch', op[4] == 'rejected' and op[6] is None and not dispatches(row['wireEvents']))
                check(result, 'exact_denial_no_personal_fact', row['answerSynthetic'] == contract['deniedProfile'] and profile.get('objective', '') not in row['answerSynthetic'])
                continue
            check(result, 'fresh_completed_receipt', op[4] == 'completed' and type(op[6]) is bytes)
            if type(op[6]) is not bytes:
                continue
            envelope = json.loads(op[6])
            result['receiptSha256'] = digest(envelope)
            payload = context.payload(envelope)
            received = [e for e in row['wireEvents'] if e.get('event') == 'call_result']
            check(result, 'wire_result_matches_exact_journal_envelope', len(received) == 1 and received[0].get('tool') == expected_tool and received[0].get('argumentsSha256') == digest(args) and received[0].get('resultSha256') == digest(envelope) and received[0].get('rpcError') is False)
            if row['kind'] == 'profile':
                check(result, 'all_fixture_profile_fields', payload.get('objective') == profile.get('objective') and payload.get('profile') == profile.get('profile'))
                expected_text = profile_answer(payload, training, contract)
            else:
                try:
                    page = library_page(payload, json.loads(op[3]), all_entries)
                except Invalid as error:
                    result['pageError'] = str(error)
                    check(result, 'exact_library_page', False)
                    continue
                check(result, 'exact_library_page', True)
                result['partialPaginationObserved'] = page['partial']
                result['pageSynthetic'] = page
                expected_text = library_answer(page, contract)
            result['expectedAnswerSynthetic'] = expected_text
            check(result, 'exact_frozen_rust_render_no_extra_facts', row['answerSynthetic'] == expected_text)
        check(checks, 'no_unlinked_operations', len(linked_ops) == len(ops))
        projections = [p for _, k, p in decoded if k == 'context_projection']
        check(checks, 'all_observed_projections_within_budget', bool(projections) and all(type(p.get('input_tokens')) is int and p['input_tokens'] >= 0 and type(p.get('output_reserved')) is int and 0 < p['output_reserved'] <= 96 and p['input_tokens'] + p['output_reserved'] + 256 <= 2048 for p in projections))
    return checks


async def run(options):
    catalog, resident, context, probe = load_helpers(options.repo)
    report = {'schemaVersion': 1, 'modelStage': 'NOT_RUN', 'status': 'running', 'scope': 'Eight development cases, real local Tutor/ACP, synthetic fixture only; no general accuracy claim', 'heldout': False, 'windowsP2000Qualified': False, 'pedagogyQualified': False, 'uiQualified': False, 'usefulResponseMeasured': False, 'cases': [], 'errors': [], 'checks': [], 'startedAt': datetime.now(timezone.utc).isoformat(), '_deadline': time.monotonic() + options.timeout, 'configuration': {'semanticRouter': True, 'prefixCache': True, 'reuseMcp': True, 'catalogCache': True, 'context': 2048, 'maxOutput': 96, 'threads': 4, 'batch': 128, 'gpuLayers': 0, 'timeoutSeconds': options.timeout, 'turnTimeoutSeconds': options.turn_timeout}, 'limits': ['Known development utterances, not held-out or a general understanding/accuracy benchmark.', 'Synthetic Owner/HostContext, not institution authentication or independent binary/source attestation.', 'First TEXT only; no first USEFUL response, UI, hardware, p95 or speedup claim.', 'Combined generation totals include the proposer. One observed proposer call plus its single journal projection tests absence of an extra answer generation.', 'Pagination oracle validates first-page items/order/next_after. Two seeded trainings with source limit=10 produce no real partial page; offline coverage is not server coverage. No continuation is admitted until the state/ChoiceSet path exists.', 'Renderer and proposer-call contract must be confirmed from source before the first run; failures are retained with no prompt or response repair.']}
    process, session, active = None, None, None
    try:
        contract = json.loads(options.contract.read_text(encoding="utf-8"))
        require(contract['version'] == 1 and contract['status'] == 'CONFIRMED_BEFORE_FIRST_RUN', 'render_and_counter_contract_not_confirmed')
        require(type(contract['proposerCallsForDirectRead']) is int and contract['proposerCallsForDirectRead'] >= 1 and type(contract['libraryLimit']) is int and 1 <= contract['libraryLimit'] <= 100, 'contract_limits_invalid')
        report['contract'] = contract
        report['artifactsBefore'] = {key: probe.fingerprint(getattr(options, key), report['_deadline']) for key in ('agent', 'server', 'model', 'contract')}
        require(report['artifactsBefore']['model']['sha256'] == resident.MODEL_SHA and report['artifactsBefore']['model']['bytes'] == resident.MODEL_BYTES, 'model_pin_mismatch')
        if options.agent_sha256:
            require(report['artifactsBefore']['agent']['sha256'] == options.agent_sha256.lower(), 'agent_sha_mismatch')
        sources = ['client/scripts/qualify-local-' + key + '.py' for key in ('catalog', 'resident', 'context', 'agent')]
        sources += [str(p.relative_to(options.repo)) for p in (options.repo / 'client/local-agent/src').rglob('*.rs')]
        sources += ['client/local-agent/Cargo.toml', 'client/local-agent/Cargo.lock', 'client/local-agent/model-profile.json']
        report['sourceFiles'] = {name: probe.fingerprint(options.repo / name, report['_deadline']) for name in sorted(sources)}
        report['environmentAllowlistNames'] = sorted(resident.safe_env())
        report['sidecar'] = probe.fingerprint(Path(__file__).resolve(), report['_deadline'])
        report['environmentBefore'] = resident.environment()
        with tempfile.TemporaryDirectory(prefix='run-', dir=HERE) as temporary:
            root = Path(temporary)
            seed, training, profile, entries = await seed_snapshot(options, resident, context, probe, root, report)
            check(report, 'all_fixture_children_gracefully_reaped', len(report.get('fixtureShutdowns', [])) == 3 and all(r.get('reaped') is True and r.get('forced') is False and r.get('graceful_eof') is True and type(r.get('exit_code')) is int and r['exit_code'] == 0 for r in report['fixtureShutdowns']))
            profile['_model_sha256'] = resident.MODEL_SHA
            report['fixture'] = {'profileSynthetic': profile, 'trainingNamesOrdered': [e['name'] for e in entries], 'twoRealTrainings': True}
            audit, data = root / 'wire.jsonl', root / 'agent'
            server = {'name': 'tutor', 'command': sys.executable, 'args': ['-B', str(Path(__file__).resolve()), '--relay', '--repo', str(options.repo), '--server', str(options.server), '--data-dir', str(seed), '--audit', str(audit)], 'env': [{'name': 'TUTOR_CONVERSATION_ID', 'value': training['conversation_id']}]}
            argv = [str(options.agent), '--model', str(options.model), '--sha256', resident.MODEL_SHA, '--data-dir', str(data), '--context', '2048', '--max-output', '96', '--threads', '4', '--batch', '128', '--gpu-layers', '0', '--semantic-router', 'on', '--prefix-cache', 'on', '--reuse-mcp', 'on', '--catalog-cache', 'on']
            try:
                process = await asyncio.wait_for(asyncio.create_subprocess_exec(*argv, cwd=root, env=resident.safe_env(), stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL, limit=probe.MAX_FRAME, start_new_session=os.name != 'nt'), 30)
                report['agentProcessStarted'] = True
                active = SemanticAcp(process, resident, probe, audit, report['_deadline'])
                initialized = await active.rpc('initialize', {'protocolVersion': 1, 'clientCapabilities': {}})
                require(initialized.get('protocolVersion') == 1 and initialized.get('authMethods') == [], 'initialize_contract_invalid')
                session = (await active.rpc('session/new', resident.session_params(root, [server])))['sessionId']
                for case in CASES:
                    probe.remaining(report['_deadline'])
                    row = await active.prompt(case, session, training, contract, report['cases'], options.turn_timeout)
                    if row['outcome'] != 'completed':
                        report['errors'].append('prompt_incomplete_stop_without_retry')
                        break
            finally:
                if process is not None:
                    if active is not None and session is not None and report['errors']:
                        try:
                            await active.base.wire.send('session/cancel', {'sessionId': session}, time.monotonic() + 2)
                        except (OSError, asyncio.TimeoutError, probe.QualificationError):
                            report['errors'].append('cancel_send_failed')
                    report['shutdown'] = await resident.stop_owned(process, probe)
                    catalog.check_shutdown(report, report['shutdown'])
            if report.get('shutdown', {}).get('reaped') and session:
                report['journal'] = inspect_journal(data, session, report['cases'], training, profile, entries, contract, context, probe)
            report['wireEvents'] = read_audit(audit)
            report['partialPaginationObserved'] = any(c.get('partialPaginationObserved') is True for c in report.get('journal', {}).get('cases', []))
            check(report, 'no_observed_relay_error', not any(e.get('event') == 'relay_error' for e in report['wireEvents']))
            ends = [e for e in report['wireEvents'] if e.get('event') == 'relay_end']
            check(report, 'all_observed_tutor_children_gracefully_reaped', bool(ends) and all(e['reaped'] is True and e['forced'] is False and e['gracefulEof'] is True and type(e['exitCode']) is int and e['exitCode'] == 0 for e in ends))
        report['artifactsAfter'] = {key: probe.fingerprint(getattr(options, key), report['_deadline']) for key in ('agent', 'server', 'model', 'contract')}
        check(report, 'artifacts_unchanged', report['artifactsBefore'] == report['artifactsAfter'])
        check(report, 'sources_unchanged', report['sourceFiles'] == {name: probe.fingerprint(options.repo / name, report['_deadline']) for name in sorted(sources)})
    except asyncio.CancelledError:
        report['errors'].append('campaign_interrupted')
    except (Invalid, probe.QualificationError) as error:
        report['errors'].append(error.args[0])
    except asyncio.TimeoutError:
        report['errors'].append('campaign_timeout')
    except (OSError, ValueError, KeyError, TypeError, RuntimeError, sqlite3.Error):
        report['errors'].append('campaign_local_error')
    finally:
        report.pop('_deadline', None)
        report['missingCases'] = [c[0] for c in CASES if not any(r['case'] == c[0] for r in report['cases'])]
        check(report, 'eight_cases_in_order', [r['case'] for r in report['cases']] == [c[0] for c in CASES])
        items = report['checks'] + [c for r in report['cases'] for c in r['checks']] + report.get('journal', {}).get('checks', []) + [c for r in report.get('journal', {}).get('cases', []) for c in r['checks']]
        report['checksSummary'] = {'observed': len(items), 'passed': sum(i['passed'] for i in items), 'failed': sum(not i['passed'] for i in items)}
        report['status'] = 'development_checks_passed' if items and all(i['passed'] for i in items) and not report['errors'] else 'failed_or_incomplete'
        generations = [(((r.get('observation') or {}).get('inference') or {}).get('counts') or {}).get('generationCalls') for r in report['cases']]
        observed = any(type(count) is int and count > 0 for count in generations)
        report['modelStage'] = 'OBSERVED_REPORTED_INFERENCE' if observed else 'UNKNOWN_AFTER_AGENT_START' if process is not None else 'NOT_RUN'
        report['environmentAfter'] = resident.environment()
        report['completedAt'] = datetime.now(timezone.utc).isoformat()
    return report


def parse_options(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--relay', action='store_true')
    parser.add_argument('--execute', action='store_true', help='Use only after parent grants model execution slot')
    parser.add_argument('--repo', type=Path, required=True)
    parser.add_argument('--server', type=Path, required=True)
    for name in ('agent', 'model', 'output', 'contract', 'data-dir', 'audit'):
        parser.add_argument('--' + name, type=Path)
    parser.add_argument('--agent-sha256')
    parser.add_argument('--timeout', type=int, default=1800)
    parser.add_argument('--turn-timeout', type=int, default=180)
    options = parser.parse_args(argv)
    options.repo, options.server = options.repo.resolve(strict=True), options.server.resolve(strict=True)
    if options.relay:
        require(options.data_dir is not None and options.audit is not None, 'relay_paths_required')
    else:
        require(all(getattr(options, k) is not None for k in ('agent', 'model', 'output', 'contract')), 'probe_paths_required')
        for key in ('agent', 'model', 'contract'):
            setattr(options, key, getattr(options, key).resolve(strict=True))
        options.output = new_output(options.output)
        require(60 <= options.timeout <= 3600 and 10 <= options.turn_timeout <= 600, 'timeouts_invalid')
        require(options.agent_sha256 is None or len(options.agent_sha256) == 64 and all(c in '0123456789abcdefABCDEF' for c in options.agent_sha256), 'agent_sha_invalid')
    return options


def main(argv=None):
    options = parse_options(argv)
    catalog, resident, _, probe = load_helpers(options.repo)
    if options.relay:
        asyncio.run(relay(options, resident, probe))
        return 0
    require(options.execute, 'model_stage_NOT_RUN_use_execute_only_after_grant')
    report = asyncio.run(run(options))
    catalog.publish_report(options.output, report)
    print(json.dumps({'status': report['status'], 'modelStage': report['modelStage'], 'checksSummary': report['checksSummary']}))
    return 0 if report['status'] == 'development_checks_passed' else 1


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Invalid as error:
        print(json.dumps({'error': error.args[0]}), file=sys.stderr)
        raise SystemExit(2)
