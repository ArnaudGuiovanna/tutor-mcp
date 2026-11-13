#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Paired development probe of the ACP catalog cache with real disposable Tutor.

Run in a reserved execution slot, for example:
  python3 -B client/scripts/qualify-local-catalog.py --repo /path/to/repo \
    --agent /absolute/tutor-local-agent --server /absolute/tutor-mcp \
    --model /absolute/Qwen3-1.7B-Q4_K_M.gguf --output /new/catalog.json

Same native binary, MCP residence on and prefix cache off in both variants.
Only --catalog-cache changes. Five profile requests per variant: cold, warm,
denied, after configuration change, and another Tutor conversation. No model
inference, UI/usefulness measurement, p95, held-out or Windows/P2000 qualification.
All profiles, objectives and databases are synthetic and disposable.
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
import shutil
import sqlite3
import sys
import tempfile
import time
import uuid

sys.dont_write_bytecode = True
_spec = importlib.util.spec_from_file_location('catalog_resident_probe', Path(__file__).with_name('qualify-local-resident.py'))
resident = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(resident)
check = resident.add_check
CASE_ORDER = ('cold_profile', 'warm_profile', 'denied_profile', 'configuration_change', 'new_conversation')
MAX_JOURNAL_BYTES = 32 * 1024 * 1024
ZERO_MODEL_COUNTS = ('countInputCalls', 'countedInputTokens', 'generationCalls', 'templateCalls', 'tokenizationCalls', 'inputTokens', 'reusedTokens', 'evaluatedInputTokens', 'outputTokens', 'modelLoadAttempts', 'modelLoadCount', 'contextAllocationAttempts', 'contextAllocations')
UNUSED_MODEL_PHASES = ('modelOpenMs', 'modelHashMs', 'backendSetupMs', 'modelLoadMs', 'modelValidationMs', 'templateMs', 'tokenizationMs', 'contextAllocationMs', 'batchAllocationMs', 'prefillMs', 'samplerSetupMs', 'samplingMs', 'decodeMs', 'tokenConversionMs')


def check_profile(row, objective, allowed, connection_reused, catalog_reused):
    """Independent wire observations must agree with actual permissions and DTOs."""
    observation = row.get('observation') or {}
    mcp = observation.get('mcp') or {}
    connection = observation.get('mcpConnection') or {}
    catalog = connection.get('catalog') or {}
    delta = row.get('wireObservedDelta') or {}
    check(row, 'completed_end_turn', row.get('outcome') == 'completed' and row.get('stopReason') == 'end_turn')
    check(row, 'one_actual_permission', row.get('permissions') == 1)
    check(row, 'synthetic_objective_marker_or_nonempty_denial', objective in row.get('answerSynthetic', '') if allowed else bool(row.get('answerSynthetic', '').strip()) and objective not in row['answerSynthetic'])
    check(row, 'dispatch_matches_permission', delta.get('profile_dispatch', 0) == int(allowed) and mcp.get('callAttempts') == int(allowed))
    check(row, 'no_unexpected_tool_dispatch', delta.get('unexpected_tool_dispatch', 0) == 0)
    check(row, 'connection_reuse_matches_case', type(connection.get('connectionReused')) is bool and connection['connectionReused'] == connection_reused)
    check(row, 'catalog_reuse_matches_case', type(connection.get('catalogReused')) is bool and connection['catalogReused'] == catalog_reused)
    check(row, 'catalog_counters_match_case', all(type(catalog.get(key)) is int for key in ('schemaVersion', 'cacheHits', 'cacheMisses')) and catalog['schemaVersion'] == 1 and catalog['cacheHits'] == int(catalog_reused) and catalog['cacheMisses'] == int(not catalog_reused))
    if catalog_reused:
        check(row, 'cache_hit_sends_no_catalog_request', delta.get('catalog_request', 0) == 0 and delta.get('catalog_page', 0) == 0 and mcp.get('catalogAttempts') == 0 and mcp.get('catalogTotalMs') is None)
    else:
        check(row, 'miss_reads_real_catalog', delta.get('catalog_request', 0) >= 1 and delta.get('catalog_page', 0) >= 1 and mcp.get('catalogAttempts') == 1)
    check(row, 'initialize_only_on_connection_change', delta.get('initialize_success', 0) == int(not connection_reused))
    generation = row.get('generation') or {}
    counts = (observation.get('inference') or {}).get('counts') or {}
    check(row, 'no_model_or_token_work', generation.get('inputTokens') == 0 and generation.get('outputTokens') == 0 and all(type(counts.get(key)) is int and counts[key] == 0 for key in ZERO_MODEL_COUNTS))
    phases = (observation.get('inference') or {}).get('phases') or {}
    operations = (observation.get('inference') or {}).get('operations') or {}
    check(row, 'no_observed_model_phases_or_calls', all(phases.get(key) is None for key in UNUSED_MODEL_PHASES) and operations.get('countInputMs') is None and operations.get('generationMs') is None)
    identifiers = row.get('permissionOperationIds') or []
    updates = row.get('toolUpdates') or []
    check(row, 'matched_permission_and_update_ids', len(identifiers) == 1 and row.get('permissionRpcIds') == ['permission:' + identifiers[0]] and bool(updates) and all(update.get('operationId') == identifiers[0] for update in updates))
    check(row, 'no_observed_dispatch_before_permission', row.get('observedDispatchesBeforePermission') == [0])
    check(row, 'no_model_context_projection', row.get('context') is None)
    check(row, 'connection_retained_without_restart', connection.get('retained') is True and connection.get('restartRequired') is False)


def check_shutdown(target, receipt):
    check(target, 'agent_gracefully_reaped', receipt.get('reaped') is True and receipt.get('forced') is False and receipt.get('graceful_eof') is True and type(receipt.get('exit_code')) is int and receipt['exit_code'] == 0)


def inspect_journal(root, sessions, rows, trainings, objective, context, probe):
    result = {'checks': [], 'sessions': {}}
    paths = list(root.glob('*/*.sqlite3'))
    check(result, 'one_journal_per_session', len(paths) == len(sessions))
    for label, session in sessions.items():
        matching = [path for path in paths if path.stem == session]
        probe.require(len(matching) == 1, 'catalog_journal_missing')
        probe.require(matching[0].stat().st_size <= MAX_JOURNAL_BYTES, 'catalog_journal_file_budget')
        with closing(sqlite3.connect(matching[0].as_uri() + '?mode=ro&immutable=1', uri=True)) as db:
            db.execute('PRAGMA query_only=ON')
            deadline = time.monotonic() + 10
            db.set_progress_handler(lambda: int(time.monotonic() > deadline), 1000)
            volume = sum(db.execute('SELECT COALESCE(SUM(length(payload)),0) FROM ' + table).fetchone()[0] for table in ('turns', 'events'))
            volume += db.execute('SELECT COALESCE(SUM(length(o.arguments)+COALESCE(length(r.payload),0)),0) FROM operations o LEFT JOIN results r ON r.operation_id=o.id').fetchone()[0]
            probe.require(volume <= MAX_JOURNAL_BYTES, 'catalog_journal_payload_budget')
            turns = db.execute('SELECT id,payload,payload_sha256 FROM turns ORDER BY sequence LIMIT 33').fetchall()
            events = db.execute('SELECT turn_id,kind,payload,payload_sha256 FROM events ORDER BY sequence LIMIT 4097').fetchall()
            operations = db.execute('SELECT o.id,o.turn_id,o.tool,o.arguments,o.state,o.mutating,r.payload FROM operations o LEFT JOIN results r ON r.operation_id=o.id ORDER BY o.created_at_ms,o.id LIMIT 33').fetchall()
            transitions = db.execute('SELECT operation_id,from_state,to_state FROM transitions ORDER BY sequence LIMIT 97').fetchall()
            probe.require(len(turns) <= 32 and len(events) <= 4096 and len(operations) <= 32, 'catalog_journal_bounds')
            probe.require(len(transitions) <= 96, 'catalog_transition_budget')
            probe.require(all(hashlib.sha256(payload).digest() == digest for _, payload, digest in turns), 'catalog_turn_digest_invalid')
            probe.require(all(hashlib.sha256(payload).digest() == digest for _, _, payload, digest in events), 'catalog_event_digest_invalid')
            decoded = [(turn, kind, json.loads(payload)) for turn, kind, payload, _ in events]
            expected = [row for row in rows if row['sessionLabel'] == label]
            check(result, label + '_all_accepted_turns_durable', len(turns) == len(expected) and all(row['outcome'] == 'completed' for row in expected))
            check(result, label + '_exact_users_and_answers', len(turns) == len(expected) and all(json.loads(turn[1]) == {'user': row['prompt']} and ''.join(value['text'] for owner, kind, value in decoded if owner == turn[0] and kind == 'assistant_chunk') == row['answerSynthetic'] for turn, row in zip(turns, expected)))
            check(result, label + '_one_complete_commit_each', all([value for owner, kind, value in decoded if owner == turn[0] and kind == 'turn_finished'] == ['complete'] for turn in turns))
            binding = [value for _, kind, value in decoded if kind == 'tutor_conversation_binding']
            check(result, label + '_exact_conversation_binding', len(binding) == 1 and binding[0].get('conversation_id') == trainings[label]['conversation_id'])
            check(result, label + '_no_model_projection', not any(kind == 'context_projection' for _, kind, _ in decoded))
            states = [operation[4] for operation in operations]
            check(result, label + '_dispatch_and_denial_receipts', len(operations) == len(expected) and states.count('completed') == sum(row['permissionAllowed'] for row in expected) and states.count('rejected') == sum(not row['permissionAllowed'] for row in expected))
            check(result, label + '_no_mutation_unknown_or_other_tool', all(operation[2] == 'tutor/get_learner_state' and operation[5] == 0 and operation[4] in ('completed', 'rejected') for operation in operations))
            expected_args = resident.expected_profile_arguments(trainings[label])
            check(result, label + '_exact_verified_arguments', all(json.loads(operation[3]) == expected_args for operation in operations))
            linked = len(turns) == len(expected)
            permissions_match, transitions_match = True, True
            facts_match = True
            for turn, row in zip(turns, expected):
                own = [operation for operation in operations if operation[1] == turn[0]]
                linked = linked and len(own) == 1 and own[0][4] == ('completed' if row['permissionAllowed'] else 'rejected')
                permissions_match = permissions_match and len(own) == 1 and row.get('permissionOperationIds') == [own[0][0]]
                actual_transitions = [(before, after) for operation_id, before, after in transitions if len(own) == 1 and operation_id == own[0][0]]
                expected_transitions = [(None, 'prepared'), ('prepared', 'dispatched'), ('dispatched', 'completed')] if row['permissionAllowed'] else [(None, 'prepared'), ('prepared', 'rejected')]
                transitions_match = transitions_match and actual_transitions == expected_transitions
                if len(own) != 1:
                    facts_match = False
                elif row['permissionAllowed']:
                    if type(own[0][6]) is not bytes:
                        facts_match = False
                    else:
                        try:
                            payload = context.payload(json.loads(own[0][6]))
                            facts_match = facts_match and type(payload.get('profile')) is dict and payload.get('objective') == objective
                        except (ValueError, probe.QualificationError):
                            facts_match = False
                else:
                    facts_match = facts_match and own[0][6] is None
            check(result, label + '_one_operation_linked_to_each_actual_turn', linked)
            check(result, label + '_permission_ids_match_executed_operations', permissions_match)
            check(result, label + '_dispatch_or_denial_transitions_exact', transitions_match)
            check(result, label + '_actual_result_facts_match_or_denial_has_no_result', facts_match)
            result['sessions'][label] = {'storedTurns': len(turns), 'completedReadOperations': states.count('completed'), 'rejectedOperations': states.count('rejected'), 'events': len(events)}
    return result


async def variant(options, enabled, root, seed, trainings, objective, context, probe, report):
    label = 'catalogOn' if enabled else 'catalogOff'
    folder = root / label
    folder.mkdir()
    data, server_data, audit = folder / 'agent', folder / 'server', folder / 'wire.jsonl'
    shutil.copytree(seed, server_data)
    result = {'variant': label, 'requested': {'catalogCache': enabled, 'prefixCache': False, 'reuseMcp': True}, 'cases': [], 'checks': [], 'errors': []}
    report['variants'].append(result)
    process, sessions = None, {}
    try:
        argv = [str(options.agent), '--model', str(options.model), '--sha256', resident.MODEL_SHA, '--data-dir', str(data), '--context', '2048', '--max-output', '96', '--threads', '4', '--batch', '128', '--gpu-layers', '0', '--prefix-cache', 'off', '--reuse-mcp', 'on', '--catalog-cache', 'on' if enabled else 'off']
        process = await asyncio.wait_for(asyncio.create_subprocess_exec(*argv, cwd=folder, env=resident.safe_env(), stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL, limit=probe.MAX_FRAME, start_new_session=os.name != 'nt'), 30)
        acp = resident.Acp(process, probe, audit, report['_deadline'])
        initialized = await acp.rpc('initialize', {'protocolVersion': 1, 'clientCapabilities': {}})
        probe.require(initialized.get('protocolVersion') == 1 and initialized.get('authMethods') == [], 'catalog_agent_initialize_invalid')
        configs = {key: [resident.server_config(options, server_data, value['conversation_id'], audit)] for key, value in trainings.items()}
        params = resident.session_params(folder, configs['A'])
        created = await acp.rpc('session/new', params)
        sessions['A'] = created['sessionId']
        async def turn(session_label, case, allowed, connection_reused, hit):
            row = await acp.prompt(sessions[session_label], trainings[session_label], resident.PROFILE_PROMPT, case, allowed, options.turn_timeout, result['cases'], session_label)
            check_profile(row, objective, allowed, connection_reused, hit)
            probe.require(row['outcome'] == 'completed', 'catalog_prompt_incomplete')
        await turn('A', 'cold_profile', True, False, False)
        before = resident.audit_counts(audit, probe)
        await acp.rpc('_tutor/rebind', {'sessionId': sessions['A'], **params})
        check(result, 'identical_rebind_has_no_wire_or_replay', not any(resident.audit_delta(before, resident.audit_counts(audit, probe)).values()))
        await turn('A', 'warm_profile', True, True, enabled)
        await turn('A', 'denied_profile', False, True, enabled)
        changed_cwd = folder / 'changed-working-directory'
        changed_cwd.mkdir()
        await acp.rpc('_tutor/rebind', {'sessionId': sessions['A'], **resident.session_params(changed_cwd, configs['A'])})
        await turn('A', 'configuration_change', True, False, False)
        rejected = await acp.rpc('_tutor/rebind', {'sessionId': sessions['A'], **resident.session_params(folder, configs['B'])}, allow_error=True)
        check(result, 'existing_session_rejects_other_conversation', rejected.get('errorCode') == -32602)
        created = await acp.rpc('session/new', resident.session_params(folder, configs['B']))
        sessions['B'] = created['sessionId']
        await turn('B', 'new_conversation', True, False, False)
    except probe.QualificationError as error:
        result['errors'].append(error.args[0])
    except asyncio.CancelledError:
        result['errors'].append('catalog_variant_interrupted')
        raise
    except asyncio.TimeoutError:
        result['errors'].append('catalog_timeout')
    except (OSError, ValueError, KeyError, TypeError, RuntimeError):
        result['errors'].append('catalog_local_execution_error')
    finally:
        if process is not None:
            result['shutdown'] = await resident.stop_owned(process, probe)
            check_shutdown(result, result['shutdown'])
        if result.get('shutdown', {}).get('reaped'):
            try:
                result['journal'] = inspect_journal(data, sessions, result['cases'], trainings, objective, context, probe)
                result['wireObservedTotals'] = dict(resident.audit_counts(audit, probe))
                totals = result['wireObservedTotals']
                check(result, 'expected_wire_initializations', totals.get('initialize_success', 0) == 3)
                check(result, 'expected_wrapper_fetches_and_wire_pages', sum(((row.get('observation') or {}).get('mcp') or {}).get('catalogAttempts', 0) for row in result['cases']) == (3 if enabled else 5) and totals.get('catalog_request', 0) == totals.get('catalog_page', 0) and totals.get('catalog_request', 0) >= (3 if enabled else 5))
                check(result, 'four_authorized_reads_only', totals.get('profile_dispatch', 0) == 4 and totals.get('unexpected_tool_dispatch', 0) == 0)
                check(result, 'no_relay_protocol_error', totals.get('relay_error', 0) == 0)
            except (OSError, sqlite3.Error, ValueError, KeyError, probe.QualificationError):
                result['errors'].append('catalog_journal_or_wire_inspection_failed')


async def run(options):
    context, probe = resident.helpers(options.repo)
    report = {'schemaVersion': 1, 'status': 'running', 'scope': 'Development paired native ACP and unchanged disposable Tutor; catalog cache only, no LLM inference.', 'windowsP2000Qualified': False, 'pedagogyQualified': False, 'usefulResponseMeasured': False, 'heldOutCorpusUsed': False, 'startedAt': datetime.now(timezone.utc).isoformat(), '_deadline': time.monotonic() + options.timeout, 'variants': [], 'checks': [], 'errors': [], 'configuration': {'variantOrder': options.order, 'catalogTtlSeconds': 30, 'prefixCache': False, 'reuseMcp': True, 'context': 2048, 'maxOutput': 96, 'gpuLayers': 0, 'threads': 4, 'batch': 128, 'timeoutSeconds': options.timeout, 'turnTimeoutSeconds': options.turn_timeout}, 'limits': ['Two development variants, one pass each; no p95 or population speed claim.', 'Transparent Python stdio relay adds startup/forwarding cost.', 'Synthetic host Owner and HostContext are not institution authentication.', 'First text only; no UI or usefulness measurement.', 'No native model generation or P2000/Windows/coaching qualification.', 'The answer oracle checks only a synthetic objective marker and nonempty refusal, not every rendered fact.', 'Permissions are automatic controlled fixture responses, not measured human approval.']}
    sources = ['client/scripts/qualify-local-catalog.py', 'client/scripts/qualify-local-resident.py', 'client/scripts/qualify-local-context.py', 'client/scripts/qualify-local-agent.py', 'client/local-agent/Cargo.toml', 'client/local-agent/Cargo.lock', 'client/local-agent/model-profile.json']
    sources += ['client/local-agent/src/' + name + '.rs' for name in ('main', 'lib', 'config', 'acp', 'runtime', 'harness', 'fast_harness', 'mcp_session', 'mcp_client', 'mcp', 'mcp_stdio', 'session_store', 'host_context', 'context', 'executor', 'call_validation', 'journal')]
    try:
        probe.require(Path(__file__).resolve() == (options.repo / 'client/scripts/qualify-local-catalog.py').resolve() and Path(resident.__file__).resolve() == (options.repo / 'client/scripts/qualify-local-resident.py').resolve(), 'catalog_imported_source_differs_from_repository')
        report['artifactsBefore'] = {name: probe.fingerprint(getattr(options, name), report['_deadline']) for name in ('agent', 'server', 'model')}
        probe.require(report['artifactsBefore']['model']['sha256'] == resident.MODEL_SHA and report['artifactsBefore']['model']['bytes'] == resident.MODEL_BYTES, 'catalog_model_pin_mismatch')
        if options.agent_sha256 is not None:
            probe.require(report['artifactsBefore']['agent']['sha256'] == options.agent_sha256.lower(), 'catalog_agent_hash_mismatch')
        report['sourceFiles'] = {name: probe.fingerprint(options.repo / name, report['_deadline']) for name in sources}
        with tempfile.TemporaryDirectory(prefix='tutor-catalog-probe-') as temporary:
            root = Path(temporary).resolve()
            seed, nonce, trainings = root / 'seed', uuid.uuid4().hex[:10], {}
            objective = 'Jardin catalogue ' + nonce
            for key in ('A', 'B'):
                trainings[key] = await resident.seed_fixture(options, seed, probe, context, str(uuid.uuid4()), 'Catalogue ' + key + ' ' + nonce, objective if key == 'A' else None, report)
            report['fixture'] = {'syntheticObjective': objective, 'trainingNamesSynthetic': [training['name'] for training in trainings.values()], 'pairedDatabaseCopiedAfterSeedShutdown': True}
            for label in options.order:
                await variant(options, label == 'catalogOn', root, seed, trainings, objective, context, probe, report)
        report['artifactsAfter'] = {name: probe.fingerprint(getattr(options, name), report['_deadline']) for name in ('agent', 'server', 'model')}
        check(report, 'artifacts_unchanged', report['artifactsBefore'] == report['artifactsAfter'])
        check(report, 'sources_unchanged', report['sourceFiles'] == {name: probe.fingerprint(options.repo / name, report['_deadline']) for name in sources})
    except probe.QualificationError as error:
        report['errors'].append(error.args[0])
    except asyncio.CancelledError:
        report['errors'].append('catalog_campaign_interrupted')
    except asyncio.TimeoutError:
        report['errors'].append('catalog_campaign_timeout')
    except (OSError, ValueError, KeyError, TypeError, RuntimeError):
        report['errors'].append('catalog_campaign_local_error')
    finally:
        report.pop('_deadline', None)
        check(report, 'two_complete_variants', len(report['variants']) == 2 and all(tuple(row['case'] for row in value['cases']) == CASE_ORDER for value in report['variants']))
        checks = report['checks'] + [item for value in report['variants'] for item in value['checks']] + [item for value in report['variants'] for row in value['cases'] for item in row['checks']] + [item for value in report['variants'] for item in value.get('journal', {}).get('checks', [])]
        errors = report['errors'] + [error for value in report['variants'] for error in value['errors']]
        report['checksSummary'] = {'observed': len(checks), 'passed': sum(item['passed'] for item in checks), 'failed': sum(not item['passed'] for item in checks)}
        report['status'] = 'observations_complete' if not errors and checks and all(item['passed'] for item in checks) else 'checks_failed_or_incomplete'
        report['missingCases'] = [{'variant': label, 'case': case} for label in options.order for case in CASE_ORDER if not any(value['variant'] == label and any(row['case'] == case for row in value['cases']) for value in report['variants'])]
        report['completedAt'] = datetime.now(timezone.utc).isoformat()
    return report


def parse_options(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    for name in ('repo', 'agent', 'server', 'model', 'output'):
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--agent-sha256')
    parser.add_argument('--order', nargs=2, choices=('catalogOff', 'catalogOn'), default=['catalogOff', 'catalogOn'])
    parser.add_argument('--timeout', type=int, default=300)
    parser.add_argument('--turn-timeout', type=int, default=60)
    options = parser.parse_args(argv)
    for name in ('repo', 'agent', 'server', 'model'):
        setattr(options, name, getattr(options, name).resolve(strict=True))
    options.output = options.output.resolve()
    if not options.repo.is_dir() or any(not getattr(options, name).is_file() for name in ('agent', 'server', 'model')) or not options.output.parent.is_dir():
        parser.error('input_type_or_output_directory_invalid')
    if options.output.exists() or not 60 <= options.timeout <= 3600 or not 10 <= options.turn_timeout <= 300 or len(set(options.order)) != 2:
        parser.error('invalid_limits_order_or_output_exists')
    if options.agent_sha256 is not None and (len(options.agent_sha256) != 64 or any(value not in '0123456789abcdefABCDEF' for value in options.agent_sha256)):
        parser.error('invalid_agent_digest')
    return options


def publish_report(output, report):
    # Serialize before any file exists. Publish only complete bytes, and never
    # replace a concurrent file. Windows rename refuses an existing destination;
    # POSIX uses an exclusive hard link in the same directory/filesystem.
    encoded = (json.dumps(report, ensure_ascii=False, indent=2, allow_nan=False) + '\n').encode('utf-8')
    descriptor, name = tempfile.mkstemp(prefix='.tutor-catalog-report-', suffix='.part', dir=output.parent)
    temporary = Path(name)
    try:
        with os.fdopen(descriptor, 'wb') as stream:
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
        if os.name == 'nt':
            os.rename(temporary, output)
        else:
            os.link(temporary, output)
    finally:
        temporary.unlink(missing_ok=True)


def main(argv=None):
    options = parse_options(argv)
    report = asyncio.run(run(options))
    publish_report(options.output, report)
    print(json.dumps({'status': report['status'], 'checksSummary': report['checksSummary']}, ensure_ascii=False))
    return 0 if report['status'] == 'observations_complete' else 1


if __name__ == '__main__':
    raise SystemExit(main())
