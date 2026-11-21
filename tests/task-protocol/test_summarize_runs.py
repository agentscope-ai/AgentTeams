import unittest

from summarize_runs import summarize


class RunSummaryTests(unittest.TestCase):
    def record(self, **fields):
        return {'runId': 'run-1', 'sourceCommit': 'source-a', 'stage': 'candidate-qwenpaw',
                'case': 'simple-csv', 'sourceEventId': '$request', **fields}

    def test_late_delivery_keeps_timeout_and_quality_pass_separate(self):
        result = summarize([(self.record(status='timeout', functionalDeliveryPassed=True,
            fullDeliverySeconds=321, latencyTargetMet=False, manualIntervention=False), None)])[0]
        self.assertEqual(result['qualityPassed'], 1)
        self.assertEqual(result['observationTimeouts'], 1)
        self.assertEqual(result['latencyTargetMissed'], 1)
        self.assertEqual(result['successfulDeliverySeconds'], {'count': 1, 'p50': 321, 'p95': 321})
        self.assertFalse(result['certified'])

    def test_failures_pending_and_unsent_are_not_silently_dropped(self):
        rows = [(self.record(functionalDeliveryPassed=False, manualIntervention=True), None),
                (self.record(runId='running', status='observing'), None),
                (self.record(runId='unsent', sourceEventId=None), None)]
        result = summarize(rows)[0]
        self.assertEqual(result['attempts'], 2)
        self.assertEqual(result['qualityFailed'], 1)
        self.assertEqual(result['pendingQualityAudit'], 1)
        self.assertEqual(result['manualInterventions'], 1)
        self.assertEqual(result['qualityPassRateAmongAudited'], 0)
        self.assertEqual(result['successfulDeliverySeconds']['count'], 0)

    def test_missing_trace_counts_remain_unknown(self):
        trace = {'generationCount': 4, 'inputTokens': 100, 'outputTokens': None,
                 'collaborationToolCalls': 2}
        rows = [(self.record(traceCoverageVerified=True), trace),
                (self.record(runId='missing', traceCoverageVerified=False), trace)]
        result = summarize(rows)[0]
        self.assertEqual(result['metrics']['generationCount'],
                         {'knownTotal': 4, 'measuredRuns': 1, 'unknownRuns': 1})
        self.assertEqual(result['metrics']['outputTokens'],
                         {'knownTotal': None, 'measuredRuns': 0, 'unknownRuns': 2})

    def test_sources_and_harness_combinations_are_separate(self):
        rows = [(self.record(), None), (self.record(sourceCommit='source-b'), None),
                (self.record(stage='cross-openclaw-qwenpaw'), None)]
        result = summarize(rows)
        self.assertEqual(len(result), 3)
        self.assertTrue(all(group['attempts'] == 1 for group in result))

    def test_concurrency_sizes_do_not_share_latency_percentiles(self):
        rows = [(self.record(concurrencySize=size, protocolRunPassed=True,
                             fullDeliverySeconds=seconds), None)
                for size, seconds in [(1, 100), (2, 200), (3, 300)]]
        groups = summarize(rows)
        self.assertEqual(len(groups), 3)
        self.assertEqual({g['concurrencySize']: g['protocolDeliverySeconds']['p50']
                          for g in groups}, {1: 100, 2: 200, 3: 300})

    def test_legacy_serial_records_group_with_explicit_serial_size(self):
        groups = summarize([(self.record(), None),
                            (self.record(concurrencySize=1), None)])
        self.assertEqual(len(groups), 1)
        self.assertEqual(groups[0]['concurrencySize'], 1)
        self.assertEqual(groups[0]['attempts'], 2)

    def test_protocol_success_can_coexist_with_business_failure(self):
        result = summarize([(self.record(protocolRunPassed=True, functionalDeliveryPassed=False,
            businessOracleVerified=True, businessOraclePassed=False, fullDeliverySeconds=340,
            status='timeout', latencyTargetMet=False), None)])[0]
        self.assertEqual(result['protocolPassed'], 1)
        self.assertEqual(result['businessOracleFailed'], 1)
        self.assertEqual(result['qualityFailed'], 1)
        self.assertEqual(result['protocolDeliverySeconds']['p50'], 340)
        self.assertEqual(result['observationTimeouts'], 1)


if __name__ == '__main__':
    unittest.main()
