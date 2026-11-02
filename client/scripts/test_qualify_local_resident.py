#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""OFFLINE tests only. No native process, server, GGUF, network or build.

Canonical checker: client/scripts/qualify-local-resident.py in repository.
Repository location override: TUTOR_RESIDENT_PROBE_REPO=/path/to/repository
Run: python3 -B client/scripts/test_qualify_local_resident.py
"""
import asyncio
from collections import Counter
import contextlib
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
from types import SimpleNamespace
import sys
import tempfile
import time
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

sys.dont_write_bytecode = True
REPO = Path(os.environ.get('TUTOR_RESIDENT_PROBE_REPO', Path(__file__).resolve().parents[2]))
SOURCE = REPO / 'client/scripts/qualify-local-resident.py'
spec = importlib.util.spec_from_file_location('resident_under_review', SOURCE)
resident = importlib.util.module_from_spec(spec)
spec.loader.exec_module(resident)
_, probe = resident.helpers(REPO)  # Loads stdlib helper definitions only.


def runtime_metadata():
    return {
        'schemaVersion': 1, 'componentScope': 'native_agent',
        'inference': {
            'schemaVersion': 1, 'timingKind': 'host_wall_clock',
            'firstUsefulResponseMs': None,
            'counts': {'generationCalls': 1, 'inputTokens': 120, 'outputTokens': 6,
                       'reusedTokens': 80, 'evaluatedInputTokens': 40,
                       'modelLoadCount': 1, 'contextAllocations': 0},
            'phases': {'prefillMs': 1, 'decodeMs': 2, 'bridgeWaitMs': 0},
        },
        'mcp': {'schemaVersion': 1, 'initializeTotalMs': None, 'catalogPages': None,
                'catalogAttempts': 1, 'callAttempts': 0},
        'mcpConnection': {'schemaVersion': 1, 'connectionReused': True,
                          'retained': True, 'catalogReused': False,
                          'restartRequired': False, 'priorCleanup': []},
        'workerElapsedMs': 3,
    }


def generated_row(answer='Potager A'):
    return {'answerSynthetic': answer, 'stopReason': 'end_turn', 'permissions': 0,
            'wireObservedDelta': {}, 'generation': {'inputTokens': 120, 'outputTokens': 6, 'elapsedMs': 3},
            'observation': runtime_metadata(), 'checks': []}


def denied_row():
    row = generated_row('Le profil n’a pas été consulté car l’autorisation n’a pas été accordée.')
    row['permissions'] = 1
    row['generation'] = {'inputTokens': 0, 'outputTokens': 0, 'elapsedMs': 0}
    row['wireObservedDelta'] = {'profile_dispatch': 0, 'catalog_request': 1, 'catalog_page': 1}
    row['observation']['inference']['counts']['generationCalls'] = 0
    return row


def checks(row):
    return {check['name']: check['passed'] for check in row['checks']}


class MemoryInput:
    def __init__(self):
        self.written = []

    def write(self, value):
        self.written.append(value)

    async def drain(self):
        pass


def memory_process(frames):
    stdout = asyncio.StreamReader()
    for frame in frames:
        stdout.feed_data(frame if isinstance(frame, bytes) else json.dumps(frame).encode() + b'\n')
    stdout.feed_eof()
    return SimpleNamespace(stdout=stdout, stdin=MemoryInput())


def answer_frame(request_id=1):
    return {'jsonrpc': '2.0', 'id': request_id, 'result': {
        'stopReason': 'end_turn', '_meta': {'tutor': {
            'generation': {'inputTokens': 0, 'outputTokens': 0, 'elapsedMs': 0},
            'context': None, 'runtime': runtime_metadata(),
        }}}}


class NoProcesses(unittest.TestCase):
    def setUp(self):
        self.guard = patch('asyncio.create_subprocess_exec', side_effect=AssertionError('OFFLINE: native process forbidden'))
        self.guard.start()
        self.addCleanup(self.guard.stop)


class CheckerTests(NoProcesses):
    def test_checker_loaded_from_canonical_repository_file(self):
        self.assertEqual(Path(resident.__file__).resolve(), (REPO / 'client/scripts/qualify-local-resident.py').resolve())

    def test_expected_profile_arguments_use_only_confirmed_domain(self):
        training = {'domain_id': 'domain-verified', 'conversation_id': 'fixture', 'training_id': 'training-verified', 'name': 'Formation publique'}
        self.assertEqual(resident.expected_profile_arguments(training), {'domain_id': 'domain-verified'})

    def test_expected_profile_arguments_empty_without_selected_domain(self):
        for training in ({}, {'domain_id': ''}, {'domain_id': None}):
            with self.subTest(training=training):
                self.assertEqual(resident.expected_profile_arguments(training), {})

    def test_true_warm_counts_are_accepted(self):
        row = generated_row()
        resident.check_generated(row, 'Potager A', 'Électricité B', True, True)
        self.assertTrue(all(checks(row).values()))

    def test_positive_reused_counter_does_not_hide_false_cache_identity(self):
        row = generated_row()
        row['observation']['inference']['counts'].update({'reusedTokens': 121, 'evaluatedInputTokens': 0})
        resident.check_generated(row, 'Potager A', 'Électricité B', True, True)
        self.assertFalse(checks(row)['input_equals_reused_plus_evaluated'])

    def test_false_warm_hit_and_disabled_cache_are_rejected(self):
        row = generated_row()
        row['observation']['inference']['counts'].update({'reusedTokens': 0, 'evaluatedInputTokens': 120})
        resident.check_generated(row, 'Potager A', 'Électricité B', True, True)
        self.assertFalse(checks(row)['expected_prefix_reuse'])
        row = generated_row()
        resident.check_generated(row, 'Potager A', 'Électricité B', False, False)
        self.assertFalse(checks(row)['no_prefix_reuse_when_disabled'])

    def test_context_budget_reserves_output_and_margin(self):
        row = generated_row()
        row['generation']['inputTokens'] = 1697  # 2048 - 96 - 256 + 1
        row['observation']['inference']['counts'].update({'inputTokens': 1697, 'reusedTokens': 0, 'evaluatedInputTokens': 1697})
        resident.check_generated(row, 'Potager A', 'Électricité B', False, False)
        self.assertFalse(checks(row)['exact_context_budget_with_output_and_margin'])

    def test_model_format_failure_remains_visible_without_repair(self):
        raw = 'La formation active est « Potager A ».\n'
        row = generated_row(raw)
        resident.check_generated(row, 'Potager A', 'Électricité B', True, True)
        self.assertTrue(checks(row)['confirmed_name_grounded'])
        self.assertFalse(checks(row)['exact_name_only_without_repair'])
        self.assertEqual(row['answerSynthetic'], raw)
        failure = next(c for c in row['checks'] if c['name'] == 'exact_name_only_without_repair')
        self.assertEqual(failure['category'], 'development_model_output')

    def test_other_conversation_name_fails_grounding(self):
        row = generated_row('Électricité B')
        resident.check_generated(row, 'Potager A', 'Électricité B', True, False)
        self.assertFalse(checks(row)['confirmed_name_grounded'])

    def test_unknown_initialize_and_pages_remain_null(self):
        original = runtime_metadata()
        value = resident.checked_runtime(copy.deepcopy(original), probe)
        self.assertIsNone(value['mcp']['initializeTotalMs'])
        self.assertIsNone(value['mcp']['catalogPages'])
        self.assertEqual(value, original)
        row = denied_row()
        row['wireObservedDelta'] = {'profile_dispatch': 0}
        resident.check_profile(row, 'SECRET_FIXTURE_OBJECTIVE', False, True)
        self.assertFalse(checks(row)['fresh_catalog_every_lease'])
        self.assertIsNone(row['observation']['mcp']['catalogPages'])

    def test_permission_denial_has_zero_dispatch_and_no_leak(self):
        row = denied_row()
        resident.check_profile(row, 'SECRET_FIXTURE_OBJECTIVE', False, True)
        self.assertTrue(all(checks(row).values()))
        row = denied_row()
        row['wireObservedDelta']['profile_dispatch'] = 1
        resident.check_profile(row, 'SECRET_FIXTURE_OBJECTIVE', False, True)
        self.assertFalse(checks(row)['dispatch_matches_permission'])
        row = denied_row()
        row['answerSynthetic'] += ' SECRET_FIXTURE_OBJECTIVE'
        resident.check_profile(row, 'SECRET_FIXTURE_OBJECTIVE', False, True)
        self.assertFalse(checks(row)['expected_grounding_or_denial'])

    def test_unknown_metadata_and_nonfinite_timing_are_not_exported(self):
        value = runtime_metadata()
        value['mcpConnection']['privateHeader'] = 'must-not-be-exported'
        with self.assertRaises(probe.QualificationError):
            resident.checked_runtime(value, probe)
        value = runtime_metadata()
        value['inference']['phases']['prefillMs'] = float('nan')
        with self.assertRaises(probe.QualificationError):
            resident.checked_runtime(value, probe)


class AcpTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.guard = patch('asyncio.create_subprocess_exec', side_effect=AssertionError('OFFLINE: native process forbidden'))
        self.guard.start()
        self.addCleanup(self.guard.stop)
        self.audit = patch.object(resident, 'audit_counts', return_value=Counter())
        self.audit.start()
        self.addCleanup(self.audit.stop)
        self.env = patch.object(resident, 'environment', return_value={'synthetic': True})
        self.env.start()
        self.addCleanup(self.env.stop)

    def acp(self, frames):
        return resident.Acp(memory_process(frames), probe, Path(tempfile.gettempdir()) / 'unused-offline-audit', time.monotonic() + 10)

    async def test_rpc_expected_id_is_accepted(self):
        acp = self.acp([{'jsonrpc': '2.0', 'id': 1, 'result': {}}])
        self.assertEqual(await acp.rpc('initialize', {}), {})

    async def test_rpc_wrong_id_types_and_values_are_rejected(self):
        for bad in (2, '1', True, 1.0):
            with self.subTest(id_type=type(bad).__name__, id_value=bad):
                acp = self.acp([{'jsonrpc': '2.0', 'id': bad, 'result': {}}])
                with self.assertRaises(probe.QualificationError):
                    await acp.rpc('initialize', {})

    async def test_wrong_wire_version_and_duplicate_json_keys_are_rejected(self):
        for raw in (b'{"jsonrpc":"1.0","id":1,"result":{}}\n', b'{"jsonrpc":"2.0","id":1,"id":1,"result":{}}\n'):
            with self.subTest(frame=raw):
                with self.assertRaises(probe.QualificationError):
                    await self.acp([raw]).rpc('initialize', {})

    async def test_rebind_must_not_replay_history(self):
        replay = {'jsonrpc': '2.0', 'method': 'session/update', 'params': {'update': {'sessionUpdate': 'user_message_chunk'}}}
        with self.assertRaises(probe.QualificationError):
            await self.acp([replay]).rpc('_tutor/rebind', {})

    async def run_prompt(self, frames):
        sink = []
        row = await self.acp(frames).prompt('session-A', {'conversation_id': 'fixture-conversation'}, resident.NAME_PROMPT, 'offline_case', True, 5, sink, 'A')
        self.assertIs(sink[0], row)
        return row

    async def test_prompt_wrong_response_id_types_are_rejected(self):
        for bad in (2, '1', True, 1.0):
            with self.subTest(id_type=type(bad).__name__, id_value=bad):
                row = await self.run_prompt([answer_frame(bad)])
                self.assertEqual(row['outcome'], 'failed')
                self.assertEqual(row['error'], 'unexpected_agent_message')

    async def test_wrong_session_and_update_kind_are_rejected(self):
        for sid, kind, expected in (('other-session', 'agent_message_chunk', 'wrong_update_session'), ('session-A', 'user_message_chunk', 'unexpected_update')):
            frame = {'jsonrpc': '2.0', 'method': 'session/update', 'params': {'sessionId': sid, 'update': {'sessionUpdate': kind, 'content': {'type': 'text', 'text': 'private-replay'}}}}
            row = await self.run_prompt([frame])
            self.assertEqual(row['error'], expected)
            self.assertEqual(row['answerSynthetic'], '')

    async def test_unexpected_permission_tool_never_gets_approved(self):
        frame = {'jsonrpc': '2.0', 'id': 'permission:wrong', 'method': 'session/request_permission', 'params': {'sessionId': 'session-A', 'toolCall': {'rawInput': {'server': 'tutor', 'tool': 'update_learner_profile', 'arguments': {}}}}}
        acp = self.acp([frame])
        sink = []
        row = await acp.prompt('session-A', {'conversation_id': 'fixture'}, resident.PROFILE_PROMPT, 'offline_deny', False, 5, sink, 'A')
        self.assertEqual(row['error'], 'permission_not_expected_read')
        self.assertEqual(len(acp.process.stdin.written), 1)  # Prompt only, no permission response.

    def permission_frame(self, arguments):
        return {'jsonrpc': '2.0', 'id': 'permission:profile', 'method': 'session/request_permission', 'params': {'sessionId': 'session-A', 'toolCall': {'rawInput': {'server': 'tutor', 'tool': 'get_learner_context', 'arguments': arguments}}, 'options': [{'kind': 'allow_once', 'optionId': 'allow'}, {'kind': 'reject_once', 'optionId': 'deny'}]}}

    async def profile_permission(self, training, arguments, allowed=True):
        acp = self.acp([self.permission_frame(arguments), answer_frame()])
        row = await acp.prompt('session-A', training, resident.PROFILE_PROMPT, 'offline_profile', allowed, 5, [], 'A')
        return row, acp.process.stdin.written

    async def test_permission_correct_host_domain_is_accepted(self):
        row, written = await self.profile_permission({'conversation_id': 'fixture', 'domain_id': 'domain-verified'}, {'domain_id': 'domain-verified'})
        self.assertEqual(row['outcome'], 'completed')
        self.assertEqual(row['permissions'], 1)
        self.assertEqual(len(written), 2)
        answer = json.loads(written[1])
        self.assertEqual(answer['result']['outcome']['optionId'], 'allow')

    async def test_permission_wrong_domain_is_rejected_before_approval(self):
        row, written = await self.profile_permission({'conversation_id': 'fixture', 'domain_id': 'domain-verified'}, {'domain_id': 'domain-other'})
        self.assertEqual(row['error'], 'permission_not_expected_read')
        self.assertEqual(row['outcome'], 'failed')
        self.assertEqual(len(written), 1)

    async def test_permission_correct_domain_with_extra_field_is_rejected(self):
        for extra in ({'limit': 1}, {'account_id': 'other-account'}, {'training_id': 'different-training'}):
            with self.subTest(extra=extra):
                row, written = await self.profile_permission({'conversation_id': 'fixture', 'domain_id': 'domain-verified'}, {'domain_id': 'domain-verified', **extra})
                self.assertEqual(row['error'], 'permission_not_expected_read')
                self.assertEqual(len(written), 1)

    async def test_permission_without_selected_domain_accepts_only_empty_arguments(self):
        row, written = await self.profile_permission({'conversation_id': 'fixture'}, {})
        self.assertEqual(row['outcome'], 'completed')
        self.assertEqual(len(written), 2)
        row, written = await self.profile_permission({'conversation_id': 'fixture'}, {'domain_id': 'invented'})
        self.assertEqual(row['error'], 'permission_not_expected_read')
        self.assertEqual(len(written), 1)

    async def test_correct_domain_permission_denial_remains_a_rejection(self):
        row, written = await self.profile_permission({'conversation_id': 'fixture', 'domain_id': 'domain-verified'}, {'domain_id': 'domain-verified'}, allowed=False)
        self.assertEqual(row['outcome'], 'completed')
        self.assertEqual(row['permissions'], 1)
        self.assertEqual(json.loads(written[1])['result']['outcome']['optionId'], 'deny')

    async def test_stop_owned_uses_helper_and_collects_nonleader_fallback(self):
        process = SimpleNamespace(returncode=None, kill=MagicMock(), wait=AsyncMock())
        def kill():
            process.returncode = -9
        process.kill.side_effect = kill
        receipt = {'graceful_eof': False, 'forced': True, 'reaped': False, 'exit_code': None}
        fake_probe = SimpleNamespace(stop_process=AsyncMock(return_value=receipt))
        result = await resident.stop_owned(process, fake_probe)
        fake_probe.stop_process.assert_awaited_once_with(process, 5)
        process.kill.assert_called_once()
        process.wait.assert_awaited_once()
        self.assertTrue(result['reaped'])


class OutputTests(NoProcesses):
    def fixture_paths(self, folder):
        server, agent, model = (folder / name for name in ('server', 'agent', 'model.gguf'))
        for path in (server, agent, model):
            path.write_bytes(b'offline fixture, never executed')
        return ['probe', '--repo', str(folder), '--server', str(server), '--agent', str(agent), '--model', str(model)]

    def test_parse_output_existing_directory_is_rejected_before_run(self):
        with tempfile.TemporaryDirectory(prefix='resident-offline-tests-') as directory:
            folder = Path(directory)
            args = self.fixture_paths(folder) + ['--output', str(folder)]
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
                resident.parse_options(args)
            self.assertEqual(error.exception.code, 2)

    def test_parse_output_new_file_accepts_spaces_and_accents(self):
        with tempfile.TemporaryDirectory(prefix='resident-offline-tests-') as directory:
            folder = Path(directory)
            output = folder / 'rapport élève nouveau.json'
            options = resident.parse_options(self.fixture_paths(folder) + ['--output', str(output), '--welcome-turns', '100'])
            self.assertEqual(options.output, output)
            self.assertEqual(options.welcome_turns, 100)
            self.assertFalse(output.exists())

    def test_main_exclusive_write_never_overwrites_a_racing_report(self):
        with tempfile.TemporaryDirectory(prefix='resident-offline-tests-') as directory:
            output = Path(directory) / 'new.json'
            options = SimpleNamespace(command='probe', output=output)
            async def simulated_run(_):
                output.write_text('existing report must remain')
                return {'status': 'observations_complete', 'checksSummary': {}}
            with patch.object(resident, 'parse_options', return_value=options), patch.object(resident, 'run', side_effect=simulated_run), self.assertRaises(FileExistsError):
                resident.main([])
            self.assertEqual(output.read_text(), 'existing report must remain')

    def test_main_directory_write_never_replaces_a_directory(self):
        with tempfile.TemporaryDirectory(prefix='resident-offline-tests-') as directory:
            output = Path(directory)
            options = SimpleNamespace(command='probe', output=output)
            report = {'status': 'observations_complete', 'checksSummary': {}}
            with patch.object(resident, 'parse_options', return_value=options), patch.object(resident, 'run', new=AsyncMock(return_value=report)), self.assertRaises((IsADirectoryError, FileExistsError)):
                resident.main([])
            self.assertTrue(output.is_dir())

    def test_main_new_json_and_failure_status_are_preserved(self):
        with tempfile.TemporaryDirectory(prefix='resident-offline-tests-') as directory:
            output = Path(directory) / 'report.json'
            options = SimpleNamespace(command='probe', output=output)
            report = {'status': 'checks_failed_or_incomplete', 'checksSummary': {'observed': 1, 'passed': 0, 'failed': 1}, 'developmentModelFailures': [{'case': 'name_warm_model', 'check': 'exact_name_only_without_repair'}]}
            with patch.object(resident, 'parse_options', return_value=options), patch.object(resident, 'run', new=AsyncMock(return_value=report)), contextlib.redirect_stdout(io.StringIO()):
                result = resident.main([])
            self.assertEqual(result, 1)
            self.assertEqual(json.loads(output.read_text()), report)


if __name__ == '__main__':
    unittest.main(verbosity=2)
