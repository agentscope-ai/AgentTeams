"""Missing telemetry must not be presented as a zero-cost release baseline."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
ZERO = {key: 0 for key in ['input', 'output', 'cache_read', 'cache_write', 'total']}


class ReleaseMetricsTests(unittest.TestCase):
    def summary(self, agents, names=('sample',)):
        with tempfile.TemporaryDirectory() as directory:
            metrics = {'test_name': 'sample', 'agents': agents,
                       'totals': {'llm_calls': 0, 'tokens': ZERO}}
            (Path(directory) / 'metrics-sample.json').write_text(json.dumps(metrics))
            result = subprocess.check_output(['bash', '-c',
                'source tests/lib/agent-metrics.sh; TEST_OUTPUT_DIR="$FIXTURE"; generate_metrics_summary "$@"',
                'fixture', *names], cwd=ROOT, text=True,
                env={**os.environ, 'FIXTURE': directory})
            return json.loads(result)

    def test_missing_agent_data_is_not_zero_cost_measurement(self):
        result = self.summary({})
        self.assertFalse(result['data_quality']['complete'])
        self.assertEqual(result['data_quality']['tests_without_agents'], ['sample'])

    def test_observed_zero_is_valid_when_agent_metrics_exist(self):
        result = self.summary({'manager': {'llm_calls': 0, 'tokens': ZERO}})
        self.assertTrue(result['data_quality']['complete'])

    def test_missing_tests_and_unsupported_runtimes_are_explicit(self):
        result = self.summary({'manager': {'llm_calls': 0, 'tokens': ZERO, 'metrics_supported': False}}, ('sample', 'missing'))
        self.assertFalse(result['data_quality']['complete'])
        self.assertEqual(result['data_quality']['missing_tests'], ['missing'])
        self.assertEqual(result['data_quality']['tests_with_unsupported_agents'], ['sample'])

    def test_incomplete_historical_baseline_disables_comparison(self):
        current = self.summary({'manager': {'llm_calls': 0, 'tokens': ZERO}})
        baseline = {'tests': [{'test_name': 'sample', 'agents': {}}], 'totals': {'llm_calls': 0, 'tokens': ZERO}}
        result = subprocess.check_output(['bash', '-c',
            'source tests/lib/agent-metrics.sh; compare_metrics_with_baseline "$CURRENT" "$BASELINE"'],
            cwd=ROOT, text=True, env={**os.environ, 'CURRENT': json.dumps(current), 'BASELINE': json.dumps(baseline)})
        compared = json.loads(result)
        self.assertFalse(compared['baseline_available'])
        self.assertEqual(compared['comparison_unavailable_reason'], 'incomplete_metrics')
        markdown = subprocess.check_output(['bash', '-c',
            'source tests/lib/agent-metrics.sh; generate_comparison_markdown "$COMPARISON"'],
            cwd=ROOT, text=True, env={**os.environ, 'COMPARISON': result})
        self.assertIn('Metrics incomplete', markdown)
        self.assertNotIn('No baseline available', markdown)
        self.assertNotIn('| Metric |', markdown)


if __name__ == '__main__':
    unittest.main()
