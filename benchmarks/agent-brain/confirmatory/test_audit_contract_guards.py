"""Real special-file and inert source-mutation checks for audit regressions."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import test_negative_control_execution_contract_v1 as execution_tests

HERE = Path(__file__).resolve().parent


class ContractGuardTest(unittest.TestCase):
    @unittest.skipUnless(hasattr(os, 'mkfifo'), 'requires POSIX FIFO')
    def test_power_reader_rejects_fifo_without_waiting_for_writer(self):
        with tempfile.TemporaryDirectory() as raw:
            fifo = Path(raw) / 'input.json'
            os.mkfifo(fifo)
            code = 'import pathlib,sys; import power_analysis_v4 as p; p.load_calibration(pathlib.Path(sys.argv[1]))'
            proc = subprocess.Popen([sys.executable, '-B', '-c', code, str(fifo)], cwd=HERE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                stdout, stderr = proc.communicate(timeout=3)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.communicate()
                self.fail('FIFO open blocked before file-type validation')
            self.assertNotEqual(proc.returncode, 0)
            self.assertIn('must be a regular file', stderr)

    def test_execution_surface_guard_detects_unexecuted_process_call(self):
        source = Path(execution_tests.contract.__file__).read_text()
        source += '\n\ndef inert_mutation():\n    os.system("never executed")\n'
        case = execution_tests.NegativeControlExecutionContractV1Test('test_module_has_build_check_only_and_no_execution_import_surface')
        with mock.patch.object(Path, 'read_text', return_value=source):
            with self.assertRaises(AssertionError):
                case.test_module_has_build_check_only_and_no_execution_import_surface()


if __name__ == '__main__':
    unittest.main()
