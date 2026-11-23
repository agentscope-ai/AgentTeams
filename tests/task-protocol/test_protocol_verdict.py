import unittest

from protocol_verdict import protocol_passed


class ProtocolVerdictTests(unittest.TestCase):
    def record(self, **fields):
        return dict(sourceInputVerified=True, leaderAcceptedVerified=True,
                    requiredGraphVerified=True, originalRoomDeliveryVerified=True,
                    traceCoverageVerified=True, manualIntervention=False, **fields)

    def test_business_failure_is_reported_without_blocking_protocol(self):
        self.assertTrue(protocol_passed(self.record(businessOraclePassed=False)))

    def test_missing_transport_evidence_or_manual_recovery_cannot_pass(self):
        original = self.record()
        for field in original:
            with self.subTest(field=field):
                missing = dict(original)
                del missing[field]
                self.assertFalse(protocol_passed(missing))
                changed = dict(original)
                changed[field] = True if field == 'manualIntervention' else False
                self.assertFalse(protocol_passed(changed))

    def test_leader_defined_plan_uses_delivery_and_acceptance(self):
        record = self.record(planningPolicy='leader_decides')
        del record['requiredGraphVerified']
        self.assertTrue(protocol_passed(record))
        record['leaderAcceptedVerified'] = False
        self.assertFalse(protocol_passed(record))

    def test_late_delivery_does_not_change_original_timeout(self):
        record = self.record(status='timeout', withinFrozenBudget=False)
        self.assertTrue(protocol_passed(record))
        self.assertEqual(record['status'], 'timeout')
        self.assertFalse(record['withinFrozenBudget'])
