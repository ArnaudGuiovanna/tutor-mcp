#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Offline tests for the catalog probe's evidence checks; no native processes."""
import contextlib
import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import sqlite3
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, patch

spec = importlib.util.spec_from_file_location('local_catalog_probe', Path(__file__).with_name('qualify-local-catalog.py'))
catalog = importlib.util.module_from_spec(spec)
spec.loader.exec_module(catalog)
context, probe = catalog.resident.helpers(Path(__file__).resolve().parents[2])


def checks(row):
    return {item['name']: item['passed'] for item in row['checks']}


def row(hit=True, allowed=True, connection=True):
    return {'checks': [], 'outcome': 'completed', 'stopReason': 'end_turn', 'permissions': 1,
            'answerSynthetic': 'Objectif fixture' if allowed else 'Autorisation refusée', 'context': None,
            'generation': {'inputTokens': 0, 'outputTokens': 0}, 'permissionOperationIds': ['fixture-operation'], 'permissionRpcIds': ['permission:fixture-operation'], 'observedDispatchesBeforePermission': [0], 'toolUpdates': [{'operationId': 'fixture-operation', 'status': 'pending'}, {'operationId': 'fixture-operation', 'status': 'completed' if allowed else 'failed'}],
            'wireObservedDelta': {'profile_dispatch': int(allowed), 'catalog_request': int(not hit), 'catalog_page': int(not hit), 'initialize_success': int(not connection)},
            'observation': {'inference': {'counts': {key: 0 for key in catalog.ZERO_MODEL_COUNTS}, 'phases': {}, 'operations': {}},
                            'mcp': {'callAttempts': int(allowed), 'catalogAttempts': int(not hit), 'catalogTotalMs': None if hit else 3.0},
                            'mcpConnection': {'connectionReused': connection, 'catalogReused': hit, 'retained': True, 'restartRequired': False,
                                              'catalog': {'schemaVersion': 1, 'cacheHits': int(hit), 'cacheMisses': int(not hit), 'lastMissReason': None if hit else 'cold'}}}}


class Offline(unittest.TestCase):
    def setUp(self):
        guard = patch('asyncio.create_subprocess_exec', side_effect=AssertionError('OFFLINE: process launch forbidden'))
        guard.start()
        self.addCleanup(guard.stop)

    def test_valid_cold_hit_and_denial_require_matching_wire_and_dto(self):
        for hit, allowed, connection in ((False, True, False), (True, True, True), (True, False, True)):
            value = row(hit, allowed, connection)
            catalog.check_profile(value, 'Objectif fixture', allowed, connection, hit)
            self.assertTrue(all(checks(value).values()), checks(value))

    def test_claimed_hit_with_wire_fetch_fails(self):
        value = row()
        value['wireObservedDelta']['catalog_request'] = 1
        catalog.check_profile(value, 'Objectif fixture', True, True, True)
        self.assertFalse(checks(value)['cache_hit_sends_no_catalog_request'])

    def test_hit_cannot_invent_zero_catalog_duration(self):
        value = row()
        value['observation']['mcp']['catalogTotalMs'] = 0
        catalog.check_profile(value, 'Objectif fixture', True, True, True)
        self.assertFalse(checks(value)['cache_hit_sends_no_catalog_request'])

    def test_old_cache_after_scope_change_fails(self):
        value = row()
        catalog.check_profile(value, 'Objectif fixture', True, False, False)
        self.assertFalse(checks(value)['catalog_reuse_matches_case'])
        self.assertFalse(checks(value)['connection_reuse_matches_case'])
        self.assertFalse(checks(value)['initialize_only_on_connection_change'])

    def test_denial_cannot_dispatch_or_reveal_prior_profile(self):
        value = row(allowed=False)
        value['wireObservedDelta']['profile_dispatch'] = 1
        value['answerSynthetic'] += ' Objectif fixture'
        catalog.check_profile(value, 'Objectif fixture', False, True, True)
        self.assertFalse(checks(value)['dispatch_matches_permission'])
        self.assertFalse(checks(value)['synthetic_objective_marker_or_nonempty_denial'])

    def test_no_generation_claim_requires_runtime_counters(self):
        for key in ('generationCalls', 'modelLoadCount', 'templateCalls', 'tokenizationCalls'):
            value = row()
            value['observation']['inference']['counts'][key] = 1
            catalog.check_profile(value, 'Objectif fixture', True, True, True)
            self.assertFalse(checks(value)['no_model_or_token_work'], key)

    def test_no_generation_claim_rejects_boolean_counters(self):
        value = row()
        value['observation']['inference']['counts']['generationCalls'] = False
        value['observation']['mcpConnection']['catalog']['cacheHits'] = True
        catalog.check_profile(value, 'Objectif fixture', True, True, True)
        self.assertFalse(checks(value)['no_model_or_token_work'])
        self.assertFalse(checks(value)['catalog_counters_match_case'])

    def fixture(self, root):
        owner = root / 'synthetic-owner'
        owner.mkdir()
        path = owner / 'session-A.sqlite3'
        db = sqlite3.connect(path)
        db.executescript('''
CREATE TABLE turns(sequence INTEGER PRIMARY KEY, id TEXT, payload BLOB, payload_sha256 BLOB);
CREATE TABLE events(sequence INTEGER PRIMARY KEY AUTOINCREMENT, turn_id TEXT, kind TEXT, payload BLOB, payload_sha256 BLOB);
CREATE TABLE operations(id TEXT, turn_id TEXT, tool TEXT, arguments BLOB, state TEXT, mutating INTEGER, created_at_ms INTEGER);
CREATE TABLE results(operation_id TEXT, payload BLOB);
CREATE TABLE transitions(sequence INTEGER PRIMARY KEY AUTOINCREMENT, operation_id TEXT, from_state TEXT, to_state TEXT);
''')
        def event(turn, kind, value):
            payload = json.dumps(value).encode()
            db.execute('INSERT INTO events(turn_id,kind,payload,payload_sha256) VALUES(?,?,?,?)', (turn, kind, payload, hashlib.sha256(payload).digest()))
        training = {'conversation_id': 'catalog-fixture-conversation', 'domain_id': 'verified-domain'}
        event(None, 'tutor_conversation_binding', {'version': 1, 'conversation_id': training['conversation_id']})
        rows = []
        for index, allowed in enumerate((True, False)):
            turn = 'turn-' + str(index)
            value = row(allowed=allowed)
            value.update({'sessionLabel': 'A', 'prompt': catalog.resident.PROFILE_PROMPT, 'permissionAllowed': allowed})
            rows.append(value)
            payload = json.dumps({'user': value['prompt']}).encode()
            db.execute('INSERT INTO turns VALUES(?,?,?,?)', (index + 1, turn, payload, hashlib.sha256(payload).digest()))
            event(turn, 'assistant_chunk', {'text': value['answerSynthetic']})
            event(turn, 'turn_finished', 'complete')
            operation = 'op-' + str(index)
            value['permissionOperationIds'] = [operation]
            db.execute('INSERT INTO operations VALUES(?,?,?,?,?,?,?)', (operation, turn, 'tutor/get_learner_state', json.dumps({'domain_id': 'verified-domain'}).encode(), 'completed' if allowed else 'rejected', 0, index))
            states = [(None, 'prepared'), ('prepared', 'dispatched'), ('dispatched', 'completed')] if allowed else [(None, 'prepared'), ('prepared', 'rejected')]
            db.executemany('INSERT INTO transitions(operation_id,from_state,to_state) VALUES(?,?,?)', [(operation, before, after) for before, after in states])
            if allowed:
                result = {'content': [], 'structuredContent': {'profile': {'language': 'fr'}, 'objective': 'Objectif fixture'}}
                db.execute('INSERT INTO results VALUES(?,?)', (operation, json.dumps(result).encode()))
        db.commit()
        db.close()
        return path, rows, {'A': training}

    def inspect(self, root, rows, trainings):
        return catalog.inspect_journal(root, {'A': 'session-A'}, rows, trainings, 'Objectif fixture', context, probe)

    def test_exact_journal_and_results_are_linked_to_each_permission(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            root = Path(folder).resolve()
            _, rows, trainings = self.fixture(root)
            result = self.inspect(root, rows, trainings)
            self.assertTrue(all(checks(result).values()), checks(result))

    def test_swapped_operation_turns_fail_even_with_same_totals(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            root = Path(folder).resolve()
            path, rows, trainings = self.fixture(root)
            with contextlib.closing(sqlite3.connect(path)) as db:
                db.execute("UPDATE operations SET turn_id=CASE turn_id WHEN 'turn-0' THEN 'turn-1' ELSE 'turn-0' END")
                db.commit()
            result = self.inspect(root, rows, trainings)
            self.assertTrue(checks(result)['A_dispatch_and_denial_receipts'])
            self.assertFalse(checks(result)['A_one_operation_linked_to_each_actual_turn'])

    def test_wrong_conversation_or_argument_proof_is_not_accepted(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            root = Path(folder).resolve()
            path, rows, trainings = self.fixture(root)
            wrong = copy.deepcopy(trainings)
            wrong['A']['conversation_id'] = 'other-conversation'
            self.assertFalse(checks(self.inspect(root, rows, wrong))['A_exact_conversation_binding'])
            with contextlib.closing(sqlite3.connect(path)) as db:
                db.execute('UPDATE operations SET arguments=?', (b'{"domain_id":"other-domain"}',))
                db.commit()
            self.assertFalse(checks(self.inspect(root, rows, trainings))['A_exact_verified_arguments'])

    def test_corrupt_journal_hash_is_rejected(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            root = Path(folder).resolve()
            path, rows, trainings = self.fixture(root)
            with contextlib.closing(sqlite3.connect(path)) as db:
                db.execute('UPDATE turns SET payload_sha256=?', (b'x' * 32,))
                db.commit()
            with self.assertRaises(probe.QualificationError):
                self.inspect(root, rows, trainings)

    def test_permission_id_must_match_journal_operation(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            root = Path(folder).resolve()
            _, rows, trainings = self.fixture(root)
            rows[0]['permissionOperationIds'] = ['other-operation']
            result = self.inspect(root, rows, trainings)
            self.assertFalse(checks(result)['A_permission_ids_match_executed_operations'])

    def test_denied_operation_with_dispatch_transition_is_rejected(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            root = Path(folder).resolve()
            path, rows, trainings = self.fixture(root)
            with contextlib.closing(sqlite3.connect(path)) as db:
                db.execute("INSERT INTO transitions(operation_id,from_state,to_state) VALUES('op-1','prepared','dispatched')")
                db.commit()
            result = self.inspect(root, rows, trainings)
            self.assertFalse(checks(result)['A_dispatch_or_denial_transitions_exact'])

    def test_model_attempts_and_hash_cannot_hide_behind_zero_load_count(self):
        value = row()
        value['observation']['inference']['counts']['modelLoadAttempts'] = 1
        value['observation']['inference']['phases']['modelHashMs'] = 3.0
        catalog.check_profile(value, 'Objectif fixture', True, True, True)
        self.assertFalse(checks(value)['no_model_or_token_work'])
        self.assertFalse(checks(value)['no_observed_model_phases_or_calls'])

    def test_denial_must_be_nonempty_and_marker_oracle_is_narrow(self):
        value = row(allowed=False)
        value['answerSynthetic'] = ''
        catalog.check_profile(value, 'Objectif fixture', False, True, True)
        self.assertFalse(checks(value)['synthetic_objective_marker_or_nonempty_denial'])
        value = row()
        value['answerSynthetic'] += ' unrelated prose is not judged by this marker oracle'
        catalog.check_profile(value, 'Objectif fixture', True, True, True)
        self.assertTrue(checks(value)['synthetic_objective_marker_or_nonempty_denial'])

    def test_nonzero_or_forced_exit_is_not_graceful(self):
        for code, forced, expected in ((0, False, True), (1, False, False), (-9, True, False)):
            result = {'checks': []}
            catalog.check_shutdown(result, {'reaped': True, 'forced': forced, 'graceful_eof': code == 0, 'exit_code': code})
            self.assertEqual(checks(result)['agent_gracefully_reaped'], expected)

    def test_journal_size_is_bounded_before_payload_reads(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            root = Path(folder).resolve()
            _, rows, trainings = self.fixture(root)
            with patch.object(catalog, 'MAX_JOURNAL_BYTES', 1), self.assertRaises(probe.QualificationError):
                self.inspect(root, rows, trainings)

    def test_atomic_writer_io_failure_leaves_no_final_or_partial_file(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            root = Path(folder).resolve()
            output = root / 'new.json'
            with patch.object(catalog.os, 'fsync', side_effect=OSError('fixture failure')), self.assertRaises(OSError):
                catalog.publish_report(output, {'status': 'failed'})
            self.assertEqual(list(root.iterdir()), [])

    def test_atomic_publish_race_preserves_existing_file(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            root = Path(folder).resolve()
            output = root / 'new.json'
            method = 'rename' if catalog.os.name == 'nt' else 'link'
            original = getattr(catalog.os, method)
            def race(source, destination):
                Path(destination).write_text('preserve racing file')
                return original(source, destination)
            with patch.object(catalog.os, method, side_effect=race), self.assertRaises(FileExistsError):
                catalog.publish_report(output, {'status': 'failed'})
            self.assertEqual(output.read_text(), 'preserve racing file')
            self.assertEqual(list(root.iterdir()), [output])

    def test_serialization_failure_creates_no_file(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            root = Path(folder).resolve()
            with self.assertRaises(ValueError):
                catalog.publish_report(root / 'report.json', {'duration': float('nan')})
            self.assertEqual(list(root.iterdir()), [])

    def test_report_writer_never_overwrites_a_racing_file(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            output = Path(folder) / 'report.json'
            options = SimpleNamespace(output=output)
            async def fake_run(_):
                output.write_text('preserve existing report')
                return {'status': 'observations_complete', 'checksSummary': {}}
            with patch.object(catalog, 'parse_options', return_value=options), patch.object(catalog, 'run', side_effect=fake_run), self.assertRaises(FileExistsError):
                catalog.main([])
            self.assertEqual(output.read_text(), 'preserve existing report')

    def test_report_writer_preserves_failure_status(self):
        with tempfile.TemporaryDirectory(prefix='catalog-offline-') as folder:
            output = Path(folder) / 'rapport élève.json'
            options = SimpleNamespace(output=output)
            report = {'status': 'checks_failed_or_incomplete', 'checksSummary': {'observed': 1, 'passed': 0, 'failed': 1}}
            with patch.object(catalog, 'parse_options', return_value=options), patch.object(catalog, 'run', new=AsyncMock(return_value=report)), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(catalog.main([]), 1)
            self.assertEqual(json.loads(output.read_text(encoding='utf-8')), report)


class PartialReportTests(unittest.IsolatedAsyncioTestCase):
    async def test_interruption_keeps_completed_and_interrupted_cases(self):
        async def interrupted_variant(options, enabled, root, seed, trainings, objective, actual_context, actual_probe, report):
            report['variants'].append({'variant': 'catalogOff', 'cases': [{'case': 'cold_profile', 'checks': [], 'outcome': 'completed'}, {'case': 'warm_profile', 'checks': [], 'outcome': 'interrupted'}], 'checks': [], 'errors': ['catalog_variant_interrupted']})
            raise catalog.asyncio.CancelledError()
        def fingerprint(path, _):
            return {'sha256': catalog.resident.MODEL_SHA, 'bytes': catalog.resident.MODEL_BYTES}
        options = SimpleNamespace(repo=Path(__file__).resolve().parents[2], agent=Path('/fixture-agent'), server=Path('/fixture-server'), model=Path('/fixture-model'), agent_sha256=None, timeout=60, turn_timeout=10, order=['catalogOff', 'catalogOn'])
        seed = {'conversation_id': 'fixture-conversation', 'name': 'fixture-training'}
        with patch('asyncio.create_subprocess_exec', side_effect=AssertionError('OFFLINE process forbidden')), patch.object(catalog.resident, 'helpers', return_value=(context, probe)), patch.object(probe, 'fingerprint', side_effect=fingerprint), patch.object(catalog.resident, 'seed_fixture', new=AsyncMock(return_value=seed)), patch.object(catalog, 'variant', side_effect=interrupted_variant):
            report = await catalog.run(options)
        self.assertEqual(report['status'], 'checks_failed_or_incomplete')
        self.assertIn('catalog_campaign_interrupted', report['errors'])
        self.assertEqual(report['variants'][0]['cases'][0]['outcome'], 'completed')
        self.assertEqual(report['variants'][0]['cases'][1]['outcome'], 'interrupted')
        self.assertTrue(report['missingCases'])


if __name__ == '__main__':
    unittest.main(verbosity=2)
