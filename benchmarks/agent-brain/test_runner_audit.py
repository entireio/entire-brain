"""Offline launcher, patch-capture, cache and usage regression tests."""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import run

HERE = Path(__file__).resolve().parent


class RunnerAuditTest(unittest.TestCase):
    def test_launchers_propagate_child_failures_and_validate_parallelism(self):
        for name, arguments in (
            ('run_cli_matrix.sh', ['codex']),
            ('run_cli_parallel.sh', ['codex', '100']),
            ('run_opus_matrix.sh', ['100']),
        ):
            with self.subTest(launcher=name), tempfile.TemporaryDirectory() as raw:
                root = Path(raw)
                script = root / name
                script.write_text((HERE / name).read_text().replace('/tmp/', str(root) + '/'))
                binary = root / 'python3'
                binary.write_text('#!/bin/sh\ncase "$FAKE_STATUS" in mixed) case "$*" in *-rv-*) exit 17;; *) exit 0;; esac;; *) exit "$FAKE_STATUS";; esac\n')
                binary.chmod(0o755)
                env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'])
                for status in ('17', 'mixed', '0'):
                    with self.subTest(status=status):
                        env['FAKE_STATUS'] = status
                        result = subprocess.run(['bash', str(script), *arguments], env=env, capture_output=True, text=True, timeout=15)
                        self.assertEqual(result.returncode == 0, status == '0', result.stdout)
                if name != 'run_cli_matrix.sh':
                    for limit in ('0', '-1', 'bad'):
                        args = ['codex', limit] if name == 'run_cli_parallel.sh' else [limit]
                        result = subprocess.run(['bash', str(script), *args], env=env, capture_output=True, text=True, timeout=2)
                        self.assertEqual(result.returncode, 2)

    def test_staged_and_unstaged_edits_are_retained_against_head(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            def git(*args):
                return subprocess.run(['git', *args], cwd=root, check=True, capture_output=True, text=True).stdout
            git('init', '-q')
            git('config', 'user.name', 'Synthetic')
            git('config', 'user.email', 'synthetic@example.invalid')
            (root / 'source.txt').write_text('before\n')
            git('add', '.')
            git('commit', '-qm', 'base')
            (root / 'source.txt').write_text('staged\n')
            (root / 'new.txt').write_text('new file\n')
            git('add', '.')
            for value in ('staged\n', 'working tree\n'):
                (root / 'source.txt').write_text(value)
                patch, metadata = run.capture_agent_patch(root)
                self.assertIn('source.txt', run.changed_files(root))
                self.assertIn('new.txt', run.changed_files(root))
                self.assertIn('+' + value.strip(), patch)
                self.assertIn('+new file', patch)
                self.assertGreater(run.diff_stat(root)['bytes'], 0)
                self.assertEqual(metadata['bytes'], len(patch.encode()))

    def test_optional_graph_is_not_hashed_for_nonsemantic_preparation(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            brain = root / 'brain'
            brain.write_text('synthetic executable')
            tools = {'brain': brain, 'graph': root / 'absent-graph'}
            task = {'id': 'fixture', 'repo_path': str(root), 'base_commit': 'base', 'prepare_semantic': False}
            payload = run.brain_cache_payload(task, 'semantic_brain', root, tools, 0)
            self.assertIsNone(payload['graph_sha256'])
            task['prepare_semantic'] = True
            with self.assertRaises(FileNotFoundError):
                run.brain_cache_payload(task, 'semantic_brain', root, tools, 0)

    def test_attempt_sum_preserves_accounting_identity_and_total_input(self):
        usage = run.extract_usage('codex', json.dumps({'type': 'turn.completed', 'usage': {'input_tokens': 100, 'output_tokens': 20, 'cached_input_tokens': 10}}), '')
        aggregate = run.aggregate_agent_attempt_usage([{'usage': usage}, {'usage': usage}])
        for field in ('accounting_version', 'accounting_source', 'accounting_rule'):
            self.assertEqual(aggregate[field], usage[field])
        self.assertEqual(aggregate['total_input_tokens'], 200)
        self.assertEqual(aggregate['total_tokens'], 240)
        self.assertTrue(aggregate['usage_report']['complete'])
        for field in ('accounting_version', 'accounting_source', 'accounting_rule'):
            with self.subTest(field=field):
                incompatible = copy.deepcopy(usage)
                incompatible[field] = 'other accounting'
                result = run.aggregate_agent_attempt_usage([{'usage': usage}, {'usage': incompatible}])
                self.assertFalse(result['usage_report']['complete'])
                self.assertIsNone(result['total_tokens'])


if __name__ == '__main__':
    unittest.main()
