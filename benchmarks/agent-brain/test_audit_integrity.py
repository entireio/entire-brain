"""Offline regressions for measurement and release-evidence integrity."""
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import audit_codex
import audit_release_matrix
import ci_tests

HERE = Path(__file__).resolve().parent


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


class IntegrityTest(unittest.TestCase):
    def test_quarantine_does_not_accept_arbitrary_process_failure(self):
        relative = next(iter(ci_tests.QUARANTINE))
        for output in ('SyntaxError: broken source\n', 'new unrelated assertion failure\n'):
            with self.subTest(output=output), mock.patch.object(ci_tests, 'modules', return_value=[ci_tests.ROOT / relative]), mock.patch.object(ci_tests, 'run', return_value=(False, output)), contextlib.redirect_stdout(io.StringIO()) as stdout, contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(ci_tests.main(), 1)
                self.assertIn(output, stdout.getvalue())

    def test_detector_failures_are_not_negative_observations(self):
        with mock.patch.object(sys, 'argv', ['regression_eval.py', '/repo', '/cache']):
            module = load('radar_integrity_test', HERE / 'regression_eval.py')
        for rc, output in ((17, ''), (17, '{"anomalies":[]}'), (0, 'broken'), (0, '{}'), (0, '{"anomalies":null}')):
            with self.subTest(rc=rc, output=output), mock.patch.object(module.subprocess, 'run', return_value=subprocess.CompletedProcess([], rc, output, 'detector failed')):
                with self.assertRaises((RuntimeError, ValueError)):
                    module.run_detect(Path('/bin'), 'query', [])
        with mock.patch.object(module.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, '{"anomalies":[]}', '')):
            self.assertEqual(module.run_detect(Path('/bin'), 'query', []), [])

    def test_false_alarm_rate_counts_cases_not_anomalies(self):
        with mock.patch.object(sys, 'argv', ['regression_eval.py', '/repo', '/cache']):
            module = load('radar_rate_test', HERE / 'regression_eval.py')
        with mock.patch.object(Path, 'exists', return_value=True), mock.patch.object(module, 'build', return_value=Path('/synthetic')), mock.patch.object(module, 'run_detect', return_value=[{'file': 'other'}, {'file': 'other'}]), mock.patch.object(module, 'with_mutation', side_effect=lambda rel, old, new, fn: fn()), mock.patch.object(module.shutil, 'rmtree'), contextlib.redirect_stdout(io.StringIO()) as output:
            module.main()
        self.assertIn('6/6 cases fired', output.getvalue())

    def test_temporal_clis_help_without_external_pythonpath(self):
        env = dict(os.environ)
        env.pop('PYTHONPATH', None)
        for script in ('generate_report.py', 'generate_sealed_report.py'):
            with self.subTest(script=script):
                result = subprocess.run([sys.executable, '-B', str(HERE / 'temporal-memory' / script), '--help'], cwd=HERE.parents[1], env=env, capture_output=True, text=True, timeout=20)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_failed_task_is_valid_measurement_but_execution_failure_is_not(self):
        module = load('sealed_integrity_test', HERE / 'temporal-memory/generate_sealed_report.py')
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            task = root / 'task.json'
            task.write_text('{}')
            digest = hashlib.sha256(task.read_bytes()).hexdigest()
            record = {'task_id': 'positive', 'condition': 'no_brain', 'runner': {'id': 'runner'}, 'provenance': {'task': {'config_sha256': digest}}, 'validation': {'ok': False, 'results': [{'returncode': 1}]}, 'ok': False, 'agent_info': {'returncode': 0, 'usage': {'total_tokens': 100}}, 'agent_leak_audit': {'ok': True}, 'agent_secret_preflight': {'ok': True}, 'changed_files': []}
            path = root / 'record.json'
            (root / 'agent.stdout').write_text('')
            (root / 'agent.stderr').write_text('')
            def read():
                path.write_text(json.dumps(record))
                with mock.patch.object(module.HARNESS, 'extract_agent_activity', return_value={}), mock.patch.object(module.HARNESS, 'temporal_memory_condition_audit', return_value={'ok': True}):
                    return module.row_from_record(path, {'sha256': digest, 'stratum': 'fact_positive'}, {'_path': str(task), 'expected_files': []}, {})
            row = read()
            self.assertTrue(row['integrity_ok'], row['integrity_findings'])
            self.assertFalse(row['validation_ok'])
            runner = module.HARNESS.parse_runner_spec('codex:gpt-5.5:low').id
            rows = []
            for stratum in ('fact_positive', 'stale_conflict', 'neutral'):
                for condition in module.CONDITIONS:
                    rows.append(dict(task_id=stratum, stratum=stratum, runner_id=runner,
                                     condition=condition, validation_ok=True, protocol_ok=True,
                                     integrity_ok=True, total_tokens=100, patch_artifact_ok=True,
                                     harness_dirty=False, source_dirty=False))
            rows[0].update(validation_ok=row['validation_ok'], integrity_ok=row['integrity_ok'])
            gates = module.build_gates(rows, {'tasks': [{}, {}, {}], 'runners': ['codex:gpt-5.5:low']},
                                       {'distillation': {'token_usage_available': True, 'reported_token_usage': 100}}, True)
            self.assertTrue(all(gate['passed'] for gate in gates), gates)
            record['agent_info']['returncode'] = 17
            self.assertFalse(read()['integrity_ok'])
            record['agent_info']['returncode'] = 0
            record['validation'] = {'ok': False, 'error': 'validation setup failed'}
            self.assertFalse(read()['integrity_ok'])

    def test_no_brain_rejects_server_calls_despite_zero_client_count(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            task = root / 'task.json'
            task.write_text('{"id":"audit-task"}')
            (root / 'run').mkdir()
            (root / 'run/mcp-server.log').write_text('message: tools/call\ntool: brain_query\ntool_result: brain_query ok\nresponse: tools/call\n')
            rec = {'condition': 'no_brain', 'run_id': 'run', 'task_id': 'audit-task', 'repetition': 1, 'runner': {'id': 'runner'}, 'ok': True, 'agent_info': {'activity': {'mcp_tool_calls': 0, 'direct_brain_cli_calls': 0, 'used_brain': False}}, 'score': {'total': 100}, 'validation': {'ok': True, 'results': [{}]}, 'provenance': {'schema': 1, 'harness': {'head': {'commit': 'a'*40}}, 'source': {'base': {'commit': 'b'*40}, 'head': {'commit': 'b'*40}, 'base_ref': 'b'*40, 'base_ref_source': 'source_head'}, 'task': {'id': 'audit-task', 'path': str(task), 'config_sha256': hashlib.sha256(task.read_bytes()).hexdigest()}, 'run_config': {'condition': 'no_brain', 'repetition': 1, 'runner': {'id': 'runner'}, 'fingerprint': 'c'*64}, 'fingerprint': 'd'*64, 'tools': {k: {'sha256': 'e'*64} for k in ('brain', 'entire', 'graph')}}}
            result = audit_codex.audit_record(rec, root)
            self.assertIn('A:no_brain_server_tool_calls(1)', result['flags'])
            self.assertFalse(result['pass'])

    def test_release_proof_requires_passing_evidence_and_explicit_scopes(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            reports = {'release': {'gate_status': {'status': 'pass', 'release_evidence': True, 'claim_policy': 'proof_required'}, 'totals': {'hard_flags': 0, 'proof_ready_comparisons': 1, 'proof_ready_comparisons_by_scope': {'local': 1}, 'named_tool_proof_ready_comparisons_by_scope': {'brain': 1}}}, 'radar_tool': {'ok': True, 'claim_scope': 'mcp_radar_tool_contract', 'claims': ['QMD']}, 'workspace_radar': {'status': 'pass', 'release_evidence': True, 'claim_policy': 'no_release_claim', 'claimable_workspace_radar': False}, 'distill': {'status': 'pass', 'release_evidence': True, 'target': {'claim_scope': 'current-repo'}}, 'facts': {'status': 'pass', 'release_evidence': True, 'claim_policy': 'no_release_claim'}}
            for key, doc in reports.items():
                (root / (key + '.json')).write_text(json.dumps(doc))
            (root / 'mise.toml').write_text('[tasks."semantic:evidence"]\nrun="true"\n[tasks."release:readiness"]\nrun="semantic:evidence"\n')
            (root / 'press.md').write_text('entire-brain entire-graph entire-replay-lab Future Claims We Should Not Make Yet Release Checklist')
            manifest = {'schema': 1, 'repo_root': '.', 'reports': {k: k+'.json' for k in reports}, 'docs': {'press_release': 'press.md'}, 'mise': 'mise.toml', 'required_release_proof_scopes': ['local'], 'required_named_tool_proof_scopes': ['brain']}
            path = root / 'manifest.json'
            def audit():
                path.write_text(json.dumps(manifest))
                return audit_release_matrix.audit_manifest(path)['rows'][0]
            self.assertTrue(audit()['claimable'])
            for key, value in (('status', 'fail'), ('release_evidence', False)):
                doc = json.loads(json.dumps(reports['release']))
                doc['gate_status'][key] = value
                (root / 'release.json').write_text(json.dumps(doc))
                with self.subTest(field=key):
                    self.assertFalse(audit()['claimable'])
            doc = json.loads(json.dumps(reports['release']))
            doc['totals']['proof_ready_comparisons'] = 0
            (root / 'release.json').write_text(json.dumps(doc))
            self.assertFalse(audit()['claimable'])
            (root / 'release.json').write_text(json.dumps(reports['release']))
            for value in (None, [], 'local'):
                manifest['required_release_proof_scopes'] = value
                with self.subTest(scopes=value):
                    self.assertFalse(audit()['claimable'])


if __name__ == '__main__':
    unittest.main()
