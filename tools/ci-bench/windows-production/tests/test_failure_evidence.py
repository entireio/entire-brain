import contextlib
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import run_shard
import run_other


class ShardFailureEvidenceTests(unittest.TestCase):
    def run_fixture(self, first_exit, first_profile):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        repository, bundle, output = root / 'repo', root / 'bundle', root / 'results'
        repository.mkdir()
        (bundle / 'binaries').mkdir(parents=True)
        go = root / 'go.exe'
        go.write_bytes(b'fake go')
        assignments = []
        for i in range(2):
            binary = bundle / 'binaries' / f'package-{i}.exe'
            binary.write_bytes(f'binary {i}'.encode())
            assignments.append(dict(importPath=f'example/pkg{i}', binaryName=binary.name,
                                    binarySha256=hashlib.sha256(binary.read_bytes()).hexdigest(),
                                    packageDirectoryRelative='.', roots=['TestCase'], runRegex='^TestCase$'))
        plan = dict(schema=run_shard.PLAN_SCHEMA, targetEnvironment=run_shard.TARGET_ENVIRONMENT,
                    repositorySha='a' * 40, goVersion='go fixture', shardCount=1,
                    settings=dict(shuffle='off', timeout='20m', commandLineLimit=32767,
                                  coverageMode='atomic', coveragePackages='./...'),
                    shards=[dict(index=0, assignments=assignments)])
        (bundle / 'plan.json').write_text(json.dumps(plan))
        args = SimpleNamespace(repository=repository, bundle=bundle, output=output,
                               shard_index=0, go_command=str(go), expected_repository_sha='a' * 40)
        calls = []
        def execute(command, **kwargs):
            if command[1:3] == ['clean', '-testcache']:
                return subprocess.CompletedProcess(command, 0, b'', b'')
            index = len(calls)
            calls.append(command)
            kwargs['stdout'].write(f'{{"Package":"example/pkg{index}","Action":"output","Output":"evidence-{index}"}}\n'.encode())
            profile = next(arg.split('=', 1)[1] for arg in command if arg.startswith('-test.coverprofile='))
            content = first_profile if index == 0 else 'mode: atomic\nexample/pkg/file.go:1.1,1.2 1 1\n'
            if content is not None:
                Path(profile).write_text(content)
            return subprocess.CompletedProcess(command, first_exit if index == 0 else 0)
        metadata = {'invocations': [], 'errors': []}
        with contextlib.ExitStack() as stack:
            for owner, name, value in [
                (run_shard.sys, 'platform', 'win32'),
                (run_shard.shutil, 'which', mock.Mock(return_value=str(go))),
                (run_shard, 'checked_repository_sha', mock.Mock(return_value='a' * 40)),
                (run_shard, 'checked_go_version', mock.Mock(return_value='go fixture')),
                (run_shard, 'checked_go_environment', mock.Mock(return_value=(run_shard.TARGET_ENVIRONMENT, root))),
                (run_shard, 'require_tracked_worktree_clean', mock.Mock()),
                (run_shard.subprocess, 'run', execute),
            ]:
                stack.enter_context(mock.patch.object(owner, name, value))
            code = run_shard.run(args, metadata)
        return code, metadata, calls, output

    def test_failed_process_keeps_evidence_and_runs_remaining_packages_without_profile(self):
        for profile in (None, '', 'mode: set\ninvalid\n'):
            with self.subTest(profile=profile):
                code, metadata, calls, output = self.run_fixture(66, profile)
                self.assertEqual(code, 66)
                self.assertEqual(len(calls), 2)
                self.assertEqual([row['exitCode'] for row in metadata['invocations']], [66, 0])
                failed, successful = metadata['invocations']
                self.assertIsNone(failed['coverageProfile'])
                self.assertIsNone(failed['coverageSha256'])
                self.assertIn('coverage', failed['coverageError'])
                self.assertTrue(successful['coverageProfile'].endswith('process-01.out'))
                events = (output / 'shard-events.jsonl').read_text()
                self.assertIn('evidence-0', events)
                self.assertIn('evidence-1', events)
                persisted = json.loads((output / 'shard-metadata.json').read_text())
                self.assertEqual(len(persisted['invocations']), 2)

    def test_success_without_coverage_still_fails_closed(self):
        with self.assertRaisesRegex(RuntimeError, 'coverage profile is missing'):
            self.run_fixture(0, None)

    def test_failed_process_retains_valid_profile_for_diagnosis(self):
        code, metadata, calls, output = self.run_fixture(1, 'mode: atomic\nexample/pkg/file.go:1.1,1.2 1 1\n')
        self.assertEqual(code, 1)
        self.assertEqual(len(calls), 2)
        self.assertTrue(metadata['invocations'][0]['coverageProfile'])
        self.assertNotIn('coverageError', metadata['invocations'][0])


class OtherFailureEvidenceTests(unittest.TestCase):
    def run_fixture(self, exit_code, profile):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        repository, bundle, output = root / 'repo', root / 'bundle', root / 'results'
        repository.mkdir()
        bundle.mkdir()
        go = root / 'go.exe'
        go.write_bytes(b'fake go')
        plan = dict(schema=run_other.PLAN_SCHEMA, targetEnvironment=run_other.TARGET_ENVIRONMENT,
                    repositorySha='a' * 40, goVersion='go fixture', packages=[],
                    settings=dict(shuffle='off', timeout='20m', commandLineLimit=32767,
                                  coverageMode='atomic', coveragePackages='./...'))
        inventory = dict(schema=run_other.PACKAGE_SCHEMA, goos='windows', goarch='amd64',
                         cgoEnabled='1', repositorySha='a' * 40, goVersion='go fixture',
                         packages=[dict(importPath='example/other', heavy=False)])
        (bundle / 'plan.json').write_text(json.dumps(plan))
        (bundle / 'package-inventory.json').write_text(json.dumps(inventory))
        def execute(command, **kwargs):
            if command[1:3] == ['clean', '-testcache']:
                return subprocess.CompletedProcess(command, 0, b'', b'')
            kwargs['stdout'].write(b'{"Action":"output","Output":"original panic evidence"}\n')
            kwargs['stderr'].write(b'original stderr evidence')
            coverage = next(arg.split('=', 1)[1] for arg in command if arg.startswith('-coverprofile='))
            if profile is not None:
                Path(coverage).write_text(profile)
            return subprocess.CompletedProcess(command, exit_code)
        args = ['--repository', str(repository), '--bundle', str(bundle), '--output', str(output),
                '--expected-repository-sha', 'a' * 40, '--go-command', str(go)]
        with contextlib.ExitStack() as stack:
            patches = [
                (run_other.sys, 'platform', 'win32'),
                (run_other.shutil, 'which', mock.Mock(return_value=str(go))),
                (run_other, 'checked_output', lambda command, *unused: 'go fixture' if command[1:] == ['version'] else 'a' * 40),
                (run_other, 'checked_target_environment', mock.Mock(return_value=run_other.TARGET_ENVIRONMENT)),
                (run_other, 'require_tracked_worktree_clean', mock.Mock()),
                (run_other.subprocess, 'run', execute),
            ]
            for owner, name, value in patches:
                stack.enter_context(mock.patch.object(owner, name, value))
            code = run_other.main(args)
        return code, json.loads((output / 'other-metadata.json').read_text()), output

    def test_failed_process_preserves_original_exit_events_and_metadata(self):
        for profile in (None, '', 'mode: set\ninvalid\n'):
            with self.subTest(profile=profile):
                code, metadata, output = self.run_fixture(66, profile)
                self.assertEqual(code, 66)
                self.assertEqual(metadata['exitCode'], 66)
                self.assertEqual(metadata['testExitCode'], 66)
                self.assertIsNone(metadata['coverageProfile'])
                self.assertIsNone(metadata['coverageSha256'])
                self.assertIn('coverage', metadata['coverageError'])
                self.assertTrue(metadata['trackedWorktreeCleanAfter'])
                self.assertIn('original panic evidence', (output / 'other-events.jsonl').read_text())
                self.assertEqual((output / 'other.stderr.log').read_text(), 'original stderr evidence')

    def test_success_missing_profile_still_fails_integrity(self):
        code, metadata, _ = self.run_fixture(0, None)
        self.assertEqual(code, 2)
        self.assertEqual(metadata['testExitCode'], 0)
        self.assertIn('coverage profile is missing', '\n'.join(metadata['errors']))
        self.assertNotIn('coverageError', metadata)

    def test_failed_process_keeps_valid_profile_for_diagnosis(self):
        code, metadata, _ = self.run_fixture(1, 'mode: atomic\nexample/other/file.go:1.1,1.2 1 1\n')
        self.assertEqual(code, 1)
        self.assertEqual(metadata['coverageProfile'], 'coverage/other.out')
        self.assertTrue(metadata['coverageSha256'])
        self.assertNotIn('coverageError', metadata)
