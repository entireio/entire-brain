"""Offline regressions for provider configuration and undefined comparisons."""
import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))
from brainmark.agents.claude_adapter import ClaudeAdapter
from brainmark.vendor import graphmark_metrics as metrics
import test_codex_provider_shim as shim_tests
from brainmark.agents import codex_provider_shim as shim
from brainmark.agents.codex_adapter import CodexAdapter


class ClaudeUsageAuditTest(unittest.TestCase):
    def test_camelcase_usage_is_authoritative_and_missing_is_not_zero(self):
        adapter = ClaudeAdapter({})
        with tempfile.TemporaryDirectory() as raw:
            stream = pathlib.Path(raw) / 'stream.jsonl'
            result = {'type': 'result', 'subtype': 'success', 'modelUsage': {'model': {
                'inputTokens': 100, 'outputTokens': 25,
                'cacheReadInputTokens': 10, 'cacheCreationInputTokens': 5}},
                'usage': {'input_tokens': 999, 'output_tokens': 999}}
            stream.write_text(json.dumps(result) + '\n')
            _, usage, _ = adapter.normalize_result(stream, 'model', 0, 1)
            self.assertEqual(usage['tokens']['total_tokens'], 140)
            self.assertTrue(usage['complete'])
            for row, complete, total in (({}, False, None), ({'inputTokens': 0, 'outputTokens': 0, 'cacheReadInputTokens': 0, 'cacheCreationInputTokens': 0}, True, 0)):
                with self.subTest(row=row):
                    result['modelUsage']['model'] = row
                    stream.write_text(json.dumps(result) + '\n')
                    _, usage, _ = adapter.normalize_result(stream, 'model', 0, 1)
                    self.assertEqual(usage['complete'], complete)
                    self.assertEqual(usage['tokens']['total_tokens'], total)


class ProviderVersionAuditTest(unittest.TestCase):
    setUp = shim_tests.ProviderShimTest.setUp
    tearDown = shim_tests.ProviderShimTest.tearDown
    _run = shim_tests.ProviderShimTest._run

    def test_api_version_configuration_matches_measured_sessions(self):
        for send in (True, False):
            with self.subTest(send_api_version=send):
                azure = dict(shim_tests.AZURE, api_version='2025-04-01-preview', send_api_version=send)
                env, _ = shim.install(self.shim_dir, azure, self.env)
                recorded = self._run(env, 'exec', 'PROMPT')
                expected = CodexAdapter({}).azure_config_args(azure)
                self.assertEqual(recorded[1:1 + len(expected)], expected)


class LeaveOneOutAuditTest(unittest.TestCase):
    def test_undefined_deletions_are_reported_and_not_sorted_against_numbers(self):
        with tempfile.TemporaryDirectory() as raw:
            ids = ['p1', 'p2', 'p3']
            for tag in ('ref', 'treat'):
                for instance in ids:
                    metrics._mk_session(raw, tag, instance, 1, 100, 0, 0, 10)
            comparison = metrics.Comparison('synthetic', raw, 'ref', 'treat', ids, {},
                                            ref_resolved={'p1'}, treat_resolved={'p1'}, draws=5)
            comparison.compute()
            loo = comparison.per_resolved_ratio('usd')['loo']
            self.assertEqual(loo['undefined'], 1)
            self.assertEqual(loo['valid'], 2)
            self.assertFalse(loo['sign_holds'])
            self.assertEqual((loo['min'], loo['max']), (0.0, 0.0))
        all_missing = metrics.loo_range([1, 2], lambda rows: None)
        self.assertEqual(all_missing['undefined'], 2)
        self.assertIsNone(all_missing['min'])
        self.assertFalse(all_missing['sign_holds'])


if __name__ == '__main__':
    unittest.main()
