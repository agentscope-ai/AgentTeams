"""Summarize retained trial evidence without certifying a Harness or merging cohorts."""
import argparse
from collections import defaultdict
import json
import math
from pathlib import Path


def percentiles(values):
    values = sorted(values)
    return {'count': len(values), **{name: values[math.ceil(q * len(values)) - 1]
            if values else None for name, q in [('p50', .5), ('p95', .95)]}}


def summarize(rows):
    groups = defaultdict(list)
    for record, trace in rows:
        if record.get('sourceEventId'):
            groups[(record['sourceCommit'], record['stage'], record['case'],
                    record.get('concurrencySize', 1))].append((record, trace))
    result = []
    for (source, stage, case, concurrency), samples in sorted(groups.items()):
        records = [record for record, _ in samples]
        passed = sum(r.get('functionalDeliveryPassed') is True for r in records)
        failed = sum(r.get('functionalDeliveryPassed') is False for r in records)
        audited = passed + failed
        protocol_good = [r for r in records if r.get('protocolRunPassed') is True]
        protocol_bad = sum(r.get('protocolRunPassed') is False for r in records)
        delivery = [r['fullDeliverySeconds'] for r in records
                    if r.get('functionalDeliveryPassed') is True
                    and type(r.get('fullDeliverySeconds')) in (int, float)
                    and math.isfinite(r['fullDeliverySeconds'])]
        metrics = {}
        for field in ['generationCount', 'inputTokens', 'outputTokens', 'collaborationToolCalls']:
            known = [trace[field] for r, trace in samples
                     if r.get('traceCoverageVerified') is True and trace is not None
                     and type(trace.get(field)) is int and trace[field] >= 0]
            metrics[field] = {'knownTotal': sum(known) if known else None,
                              'measuredRuns': len(known), 'unknownRuns': len(samples) - len(known)}
        result.append({'sourceCommit': source, 'stage': stage, 'case': case,
            'concurrencySize': concurrency,
            'attempts': len(samples), 'auditedRuns': audited, 'qualityPassed': passed,
            'qualityFailed': failed, 'pendingQualityAudit': len(samples) - audited,
            'qualityPassRateAmongAudited': passed / audited if audited else None,
            'protocolPassed': len(protocol_good), 'protocolFailed': protocol_bad,
            'pendingProtocolAudit': len(samples) - len(protocol_good) - protocol_bad,
            'businessOraclePassed': sum(r.get('businessOraclePassed') is True for r in records),
            'businessOracleFailed': sum(r.get('businessOraclePassed') is False
                                       and r.get('businessOracleVerified') is True for r in records),
            'protocolDeliverySeconds': percentiles([r['fullDeliverySeconds'] for r in protocol_good
                if type(r.get('fullDeliverySeconds')) in (int, float)
                and math.isfinite(r['fullDeliverySeconds'])]),
            'observationTimeouts': sum(r.get('status') == 'timeout' for r in records),
            'latencyTargetMet': sum(r.get('latencyTargetMet') is True for r in records),
            'latencyTargetMissed': sum(r.get('latencyTargetMet') is False for r in records),
            'manualInterventions': sum(r.get('manualIntervention') is True for r in records),
            'successfulDeliverySeconds': percentiles(delivery), 'metrics': metrics,
            'certified': False})
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('runs', type=Path)
    parser.add_argument('--source', help='Exact runtime source commit to include')
    args = parser.parse_args()
    rows = []
    for path in sorted(args.runs.glob('*/record.json')):
        record = json.loads(path.read_text())
        if args.source and record['sourceCommit'] != args.source:
            continue
        trace = path.parent / 'trace-metrics-window.json'
        rows.append((record, json.loads(trace.read_text()) if trace.exists() else None))
    print(json.dumps({'groups': summarize(rows),
        'notes': ['Sources and Harness combinations are separate; no automatic baseline comparison.',
                  'Serial, pair and triple request groups have separate latency and success statistics.',
                  'Unfinished/unaudited runs remain visible; pass rate denominator is auditedRuns.',
                  'P50/P95 use nearest rank over verified successful complete delivery times only.',
                  'Timeout records remain timeouts even when later delivery passes quality gates.',
                  'Unknown metrics are not zero; this summary is not Harness certification.']}, indent=2))


if __name__ == '__main__':
    main()
