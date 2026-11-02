#!/usr/bin/env python3
"""Offline synthetic checker tests: no process, server, network or model."""
import asyncio
from contextlib import closing
from copy import deepcopy
import hashlib
import importlib.util
import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('first_semantic', HERE / 'qualify-local-semantic.py')
b = importlib.util.module_from_spec(spec)
spec.loader.exec_module(b)
REPO = HERE.parent.parent
catalog, resident, context, probe = b.load_helpers(REPO)
CONTRACT = json.loads((REPO / 'docs/local-harness/semantic-render-contract.json').read_text(encoding="utf-8"))
TRAINING = {'domain_id': 'domain-fixture', 'conversation_id': 'conversation-fixture', 'name': 'Formation Go alpha'}
PROFILE = {'objective': 'Apprendre alpha', 'profile': {'language': 'fr', 'device': 'laptop'}, '_model_sha256': 'a' * 64}
ENTRIES = [dict(id='personal:a', name='Formation Go alpha', kind='personal', revision=1, status='active', can_learn=True), dict(id='personal:b', name='Formation Jardin alpha', kind='personal', revision=2, status='active', can_learn=True)]


def passing(result):
    return all(c['passed'] for c in result['checks'] + [c for r in result.get('cases', []) for c in r['checks']])


def permission(tool='get_learner_context', args=None):
    return {'jsonrpc': '2.0', 'id': 'permission:op-1', 'method': 'session/request_permission', 'params': {'sessionId': 'session', 'toolCall': {'toolCallId': 'op-1', 'rawInput': {'server': 'tutor', 'tool': tool, 'arguments': args if args is not None else {'domain_id': TRAINING['domain_id']}}}, 'options': [{'kind': 'allow_once', 'optionId': 'allow'}, {'kind': 'reject_once', 'optionId': 'reject'}]}}


class FakeInput:
    def __init__(self):
        self.frames = []

    def write(self, frame):
        self.frames.append(json.loads(frame))

    async def drain(self):
        pass


class FakeWire:
    def __init__(self, frames):
        self.frames = iter(frames)

    async def send(self, *args):
        pass

    async def receive(self, deadline):
        item = next(self.frames)
        if isinstance(item, Exception):
            raise item
        if callable(item):
            return item()
        return item


class CheckerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='offline-', dir=HERE)
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def page(self, limit=10):
        return {'items': deepcopy(ENTRIES[:limit]), 'next_after': 'personal:a' if limit == 1 else ''}

    def test_full_library_exact_order(self):
        page = b.library_page(self.page(), {'limit': 10}, ENTRIES)
        self.assertFalse(page['partial'])
        self.assertEqual(b.library_answer(page, CONTRACT), 'Formations trouvées :\n\n1. Formation Go alpha\n2. Formation Jardin alpha')

    def test_partial_library_cannot_claim_exhaustive(self):
        page = b.library_page(self.page(1), {'limit': 1}, ENTRIES)
        self.assertTrue(page['partial'])
        self.assertTrue(b.library_answer(page, CONTRACT).endswith(CONTRACT['library']['partialLine']))
        wrong = self.page(1)
        wrong['next_after'] = ''
        with self.assertRaises(b.Invalid):
            b.library_page(wrong, {'limit': 1}, ENTRIES)

    def test_wrong_names_order_cursor_types_and_extra_scope_rejected(self):
        samples = []
        wrong = self.page(); wrong['items'].reverse(); samples.append((wrong, {'limit': 10}))
        wrong = self.page(); wrong['items'][0]['name'] = 'Invented'; samples.append((wrong, {'limit': 10}))
        wrong = self.page(); wrong['next_after'] = 'invented'; samples.append((wrong, {'limit': 10}))
        wrong = self.page(); wrong['items'][0]['revision'] = True; samples.append((wrong, {'limit': 10}))
        wrong = self.page(); wrong['items'][0] = None; samples.append((wrong, {'limit': 10}))
        samples += [(self.page(), {'limit': 10, 'conversation_id': 'other'}), (self.page(), {'limit': True}), (self.page(), {'limit': 10, 'query': 'Go'})]
        for payload, args in samples:
            with self.subTest(payload=payload, args=args), self.assertRaises(b.Invalid):
                b.library_page(payload, args, ENTRIES)

    def test_profile_exact_facts_not_marker(self):
        expected = 'Informations de profil retournées par Tutor :\n- Objectif : Apprendre alpha\n- Langue : fr\n- Appareil : laptop\n\nFormation active : Formation Go alpha'
        self.assertEqual(b.profile_answer(PROFILE, TRAINING, CONTRACT), expected)
        self.assertNotEqual(expected, CONTRACT['profile']['heading'])

    def test_permission_exact_domain_and_id(self):
        self.assertEqual(b.validate_permission(permission(), 'session', 'get_learner_context', {'domain_id': TRAINING['domain_id']}, 0, probe), 'op-1')
        for modify in (lambda v: v.update(id='permission:other'), lambda v: v['params'].update(sessionId='wrong'), lambda v: v['params']['toolCall']['rawInput']['arguments'].update(domain_id='other'), lambda v: v['params']['toolCall']['rawInput']['arguments'].update(extra=1)):
            value = permission(); modify(value)
            with self.assertRaises(probe.QualificationError):
                b.validate_permission(value, 'session', 'get_learner_context', {'domain_id': TRAINING['domain_id']}, 0, probe)
        with self.assertRaises(probe.QualificationError):
            b.validate_permission(permission(), 'session', 'get_learner_context', {'domain_id': TRAINING['domain_id']}, 1, probe)

    def replay(self, *, kind='profile', allowed=True, mutate=None, updates=True, wire=True, generations=1):
        audit = self.root / 'audit.jsonl'
        if audit.exists(): audit.unlink()
        tool = 'list_training_library' if kind == 'library' else 'get_learner_context'
        args = {'limit': 10} if kind == 'library' else {'domain_id': TRAINING['domain_id']}
        def update(status):
            return {'method': 'session/update', 'params': {'sessionId': 'session', 'update': {'sessionUpdate': 'tool_call_update', 'title': tool, 'toolCallId': 'op-1', 'status': status}}}
        frames = []
        if kind in ('profile', 'library'):
            if updates: frames.append(update('pending'))
            frames.append(permission(tool, args))
            if allowed:
                def observed_dispatch():
                    if wire:
                        audit.write_bytes(b.canonical({'event': 'call_sent', 'tool': tool, 'argumentsSha256': b.digest(args)}) + b'\n')
                    return update('in_progress')
                frames.append(observed_dispatch)
                if updates: frames.append(update('completed'))
            elif updates:
                frames.append(update('failed'))
        frames.append({'method': 'session/update', 'params': {'sessionId': 'session', 'update': {'sessionUpdate': 'agent_message_chunk', 'content': {'type': 'text', 'text': 'Synthetic answer'}}}})
        result = {'jsonrpc': '2.0', 'id': 1, 'result': {'stopReason': 'end_turn', '_meta': {'tutor': {'generation': {'inputTokens': 32, 'outputTokens': 7, 'elapsedMs': 4}, 'runtime': {'schemaVersion': 1, 'componentScope': 'native_agent', 'inference': {'counts': {'generationCalls': generations, 'inputTokens': 32, 'outputTokens': 7}}}}}}}
        if mutate: mutate(result, frames, audit)
        frames.append(result)
        process = SimpleNamespace(stdin=FakeInput())
        active = b.SemanticAcp.__new__(b.SemanticAcp)
        active.resident, active.probe, active.audit = resident, probe, audit
        active.base = SimpleNamespace(identifier=0, deadline=time.monotonic() + 5, process=process, wire=FakeWire(frames))
        rows = []
        row = asyncio.run(active.prompt(('fake', 'untuned', kind, allowed), 'session', TRAINING, CONTRACT, rows, 2))
        self.assertEqual(rows, [row])
        return row, process

    def test_offline_protocol_happy_profile_and_library(self):
        for kind in ('profile', 'library'):
            row, process = self.replay(kind=kind)
            self.assertTrue(passing(row), row)
            self.assertEqual(process.stdin.frames[0]['id'], 'permission:op-1')

    def test_denied_no_dispatch(self):
        row, process = self.replay(allowed=False)
        self.assertTrue(passing(row), row)
        self.assertEqual(process.stdin.frames[0]['result']['outcome']['optionId'], 'reject')

    def test_denied_with_observed_dispatch_fails(self):
        def mutate(value, frames, audit):
            chunk = frames[-1]
            def late_dispatch():
                audit.write_bytes(b.canonical({'event': 'call_sent', 'tool': 'get_learner_context', 'argumentsSha256': b.digest({'domain_id': TRAINING['domain_id']})}) + b'\n')
                return chunk
            frames[-1] = late_dispatch
        row, _ = self.replay(allowed=False, mutate=mutate)
        self.assertFalse(passing(row))

    def test_fabricated_success_without_wire_fails(self):
        row, _ = self.replay(wire=False)
        self.assertFalse(passing(row))

    def test_missing_updates_fails(self):
        row, _ = self.replay(updates=False)
        self.assertFalse(passing(row))

    def test_extra_llm_answer_generation_fails(self):
        row, _ = self.replay(generations=2)
        self.assertFalse(passing(row))

    def test_wrong_and_bool_response_ids_fail(self):
        for identifier in (True, '1', 9):
            row, _ = self.replay(mutate=lambda value, frames, audit: value.update(id=identifier))
            self.assertEqual(row['outcome'], 'failed')

    def test_no_read_unexpected_permission_fails(self):
        row, _ = self.replay(kind='no_mcp', mutate=lambda value, frames, audit: frames.insert(0, permission()))
        self.assertEqual(row['outcome'], 'failed')

    def test_timeout_preserved_not_success(self):
        row, _ = self.replay(mutate=lambda value, frames, audit: frames.insert(0, asyncio.TimeoutError()))
        self.assertEqual(row['outcome'], 'timeout')
        self.assertFalse(passing(row))

    def journal(self, *, answer=None, denial=False, mutate=None):
        data = self.root / ('journal-' + str(len(list(self.root.glob('journal-*')))))
        path = data / 'owner' / 'session.sqlite3'
        path.parent.mkdir(parents=True)
        envelope = {'structuredContent': {k: v for k, v in PROFILE.items() if not k.startswith('_')}}
        text = answer if answer is not None else CONTRACT['deniedProfile'] if denial else b.profile_answer(PROFILE, TRAINING, CONTRACT)
        args = {'domain_id': TRAINING['domain_id']}
        row = {'case': 'fake', 'kind': 'profile', 'prompt': 'donne moii mon profil', 'permissionAllowed': not denial, 'permissionOperationIds': ['op-1'], 'answerSynthetic': text, 'generation': {'inputTokens': 32, 'outputTokens': 7}, 'wireEvents': [] if denial else [{'event': 'call_result', 'tool': 'get_learner_context', 'argumentsSha256': b.digest(args), 'resultSha256': b.digest(envelope), 'rpcError': False}]}
        with closing(sqlite3.connect(path)) as db:
            db.executescript('CREATE TABLE turns(sequence INTEGER,id TEXT,payload BLOB,payload_sha256 BLOB); CREATE TABLE events(sequence INTEGER,turn_id TEXT,kind TEXT,payload BLOB,payload_sha256 BLOB); CREATE TABLE operations(id TEXT,turn_id TEXT,tool TEXT,arguments BLOB,state TEXT,mutating INTEGER); CREATE TABLE results(operation_id TEXT,payload BLOB); CREATE TABLE transitions(sequence INTEGER,operation_id TEXT,from_state TEXT,to_state TEXT);')
            raw = b.canonical({'user': row['prompt']})
            db.execute('INSERT INTO turns VALUES(1,?,?,?)', ('turn-1', raw, hashlib.sha256(raw).digest()))
            events = [(None, 'session_header', {'model_sha256': 'a' * 64}), (None, 'tutor_conversation_binding', {'conversation_id': TRAINING['conversation_id']}), ('turn-1', 'assistant_chunk', {'text': text}), ('turn-1', 'turn_finished', 'complete'), ('turn-1', 'semantic_intent_proposed', {'intent': 'profile'}), ('turn-1', 'context_projection', {'input_tokens': 32, 'output_reserved': 64})]
            for n, (turn, kind, value) in enumerate(events):
                raw = b.canonical(value)
                db.execute('INSERT INTO events VALUES(?,?,?,?,?)', (n, turn, kind, raw, hashlib.sha256(raw).digest()))
            db.execute('INSERT INTO operations VALUES(?,?,?,?,?,?)', ('op-1', 'turn-1', 'tutor/get_learner_context', b.canonical(args), 'rejected' if denial else 'completed', 0))
            if not denial:
                db.execute('INSERT INTO results VALUES(?,?)', ('op-1', b.canonical(envelope)))
            transitions = [(None, 'prepared'), ('prepared', 'rejected')] if denial else [(None, 'prepared'), ('prepared', 'dispatched'), ('dispatched', 'completed')]
            for n, (before, after) in enumerate(transitions):
                db.execute('INSERT INTO transitions VALUES(?,?,?,?)', (n, 'op-1', before, after))
            if mutate: mutate(db, row)
            db.commit()
        return b.inspect_journal(data, 'session', [row], TRAINING, PROFILE, ENTRIES, CONTRACT, context, probe)

    def test_journal_exact_complete_profile(self):
        result = self.journal()
        self.assertTrue(passing(result), result)

    def test_marker_or_invented_fact_not_success(self):
        for answer in (CONTRACT['profile']['heading'], b.profile_answer(PROFILE, TRAINING, CONTRACT) + '\nTu as progressé de 50 %.'):
            result = self.journal(answer=answer)
            self.assertFalse(passing(result))

    def test_denial_no_receipt_or_leak(self):
        self.assertTrue(passing(self.journal(denial=True)))
        self.assertFalse(passing(self.journal(denial=True, answer=CONTRACT['deniedProfile'] + ' Apprendre alpha')))

    def test_wrong_operation_id_transition_or_result_hash_fails(self):
        mutations = [lambda db, row: row.update(permissionOperationIds=['wrong']), lambda db, row: db.execute("UPDATE transitions SET to_state='unknown' WHERE to_state='completed'"), lambda db, row: row['wireEvents'][0].update(resultSha256='b' * 64), lambda db, row: db.execute("INSERT INTO operations VALUES('extra','other','tutor/other','{}','prepared',0)")]
        for change in mutations:
            self.assertFalse(passing(self.journal(mutate=change)))

    def test_unknown_wire_fields_not_reported(self):
        path = self.root / 'wire.jsonl'
        path.write_text(json.dumps({'event': 'request', 'method': 'tools/list', 'secret': 'must never serialize'}), encoding="utf-8")
        with self.assertRaises(b.Invalid):
            b.read_audit(path)

    def test_unknown_catalog_page_never_invented(self):
        path = self.root / 'wire.jsonl'
        path.write_text(json.dumps({'event': 'request', 'method': 'tools/list'}), encoding="utf-8")
        self.assertEqual(b.read_audit(path), [{'event': 'request', 'method': 'tools/list'}])
        self.assertNotIn('pages', b.read_audit(path)[0])

    def test_exclusive_atomic_output_and_directory(self):
        output = b.new_output(self.root / 'fresh.json')
        catalog.publish_report(output, {'ok': True})
        with self.assertRaises((FileExistsError, probe.QualificationError)):
            catalog.publish_report(output, {'ok': False})
        self.assertEqual(json.loads(output.read_text(encoding="utf-8")), {'ok': True})
        for path in (self.root, self.root / 'missing' / 'out.json', Path('/tmp/outside-sidecar.json')):
            with self.assertRaises(b.Invalid): b.new_output(path)
        bad = self.root / 'not-published.json'
        with self.assertRaises(ValueError): catalog.publish_report(bad, {'duration': float('nan')})
        self.assertFalse(bad.exists())

    def test_run_unconfirmed_contract_fails_before_any_process(self):
        contract = self.root / 'unconfirmed.json'
        value = deepcopy(CONTRACT)
        value['status'] = 'PROPOSED'
        contract.write_text(json.dumps(value), encoding="utf-8")
        options = SimpleNamespace(repo=REPO, timeout=60, turn_timeout=10, contract=contract)
        result = asyncio.run(b.run(options))
        self.assertEqual(result['modelStage'], 'NOT_RUN')
        self.assertEqual(result['status'], 'failed_or_incomplete')
        self.assertIn('render_and_counter_contract_not_confirmed', result['errors'])
        self.assertEqual(len(result['missingCases']), 8)


if __name__ == '__main__':
    # A test accidentally reaching execution fails before any child/network IO.
    with patch('asyncio.create_subprocess_exec', side_effect=AssertionError('OFFLINE_ONLY')), patch('subprocess.Popen', side_effect=AssertionError('OFFLINE_ONLY')), patch('socket.create_connection', side_effect=AssertionError('OFFLINE_ONLY')), patch('asyncio.open_connection', side_effect=AssertionError('OFFLINE_ONLY')):
        unittest.main()
