"""Protocol evidence is distinct from model-generated business quality and latency."""


def protocol_passed(record):
    required = ('sourceInputVerified', 'leaderAcceptedVerified', 'requiredGraphVerified',
                'originalRoomDeliveryVerified', 'traceCoverageVerified')
    return (all(record.get(field) is True for field in required)
            and record.get('manualIntervention') is False)
