"""Verify real stalled recall children are stopped before run_arm returns."""
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
SCRIPT = r'''
import os, pathlib, sys, time, types
from unittest import mock
import verify_engines as v
root = pathlib.Path(sys.argv[1])
pid_path = root / 'child.pid'
mode = sys.argv[2]
v.RECALL_TIMEOUT_SECONDS = 0.4
settings = types.SimpleNamespace(output_dir=root, repo_root=root, server_start_timeout_seconds=1)
prepared = types.SimpleNamespace(arm='synthetic')
code = 'import os,pathlib,time; pathlib.Path(%r).write_text(str(os.getpid())); time.sleep(30)' % str(pid_path)
def observe(phase):
    deadline = time.monotonic() + 2
    while not pid_path.exists() and time.monotonic() < deadline:
        time.sleep(.01)
    raise v.VerificationError('synthetic health failure')
with mock.patch.object(v, 'recall_command', return_value=[sys.executable, '-c', code]), mock.patch.object(v, 'safe_runtime_environment', return_value=dict(os.environ)):
    try:
        v.run_arm(settings, {}, prepared, None, {}, {}, during_observer=observe if mode == 'health' else None, recall_window_observer=lambda a,b: None)
    except v.VerificationError as exc:
        print(str(exc), flush=True)
'''


class RecallLifetimeTest(unittest.TestCase):
    def test_stalled_recall_is_bounded_and_health_failure_cancels_child(self):
        for mode in ('timeout', 'health'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as raw:
                pid_path = Path(raw) / 'child.pid'
                proc = subprocess.Popen([sys.executable, '-B', '-c', SCRIPT, raw, mode], cwd=HERE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                timed_out = False
                try:
                    stdout, stderr = proc.communicate(timeout=4)
                except subprocess.TimeoutExpired:
                    timed_out = True
                    if pid_path.exists():
                        try:
                            os.kill(int(pid_path.read_text()), signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                    proc.kill()
                    stdout, stderr = proc.communicate()
                self.assertFalse(timed_out, f'{mode}: recall prevented cleanup')
                self.assertEqual(proc.returncode, 0, stderr)
                self.assertIn('health failure' if mode == 'health' else 'timed out', stdout)
                self.assertTrue(pid_path.exists())
                with self.assertRaises(ProcessLookupError):
                    os.kill(int(pid_path.read_text()), 0)


if __name__ == '__main__':
    unittest.main()
