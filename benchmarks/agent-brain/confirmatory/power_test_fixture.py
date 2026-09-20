"""Archived exploratory diagnostics for offline pending-contract unit tests.

This fixture does not recreate the missing source panel or authorize its reuse.
Tests of the real calibration loader bypass this substitution explicitly.
"""
import copy
import json
from pathlib import Path
from unittest import mock


def patch_archived_calibration(module):
    original = module.build_calibration_diagnostics
    archived = json.loads((Path(__file__).parent / 'power-analysis.json').read_text())

    def diagnostics(*args, **kwargs):
        if args or kwargs:
            return original(*args, **kwargs)
        return copy.deepcopy(archived['exploratory_calibration'])

    return mock.patch.object(module, 'build_calibration_diagnostics', side_effect=diagnostics)
