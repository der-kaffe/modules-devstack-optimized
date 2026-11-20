#!/usr/bin/env python3
"""Read-only acceptance of a predeclared official subset after full restoration.

Recomputes every window from checksummed raw data. A completed subset never
implies completion of every case in the frozen protocol.
"""
import argparse
import csv
import hashlib
import json
from pathlib import Path
import sys
import uuid

from experiment_analysis import glance_run, paired, host_capacity_summary


def validate_window(manifest, protocol, receipt_sha, case):
    if (manifest['stage'] != 'official' or manifest['state'] != 'COMPLETE_WINDOW'
            or not manifest['cleanup_verified'] or manifest['errors']
            or manifest.get('entry_evidence_sha256') != receipt_sha
            or manifest['protocol']['status'] != 'frozen'
            or manifest['duration_s'] != protocol['measurement_s']
            or manifest['warmup_s'] != protocol['warmup_s']
            or manifest['timeout_s'] != protocol['request_timeout_s']
            or [manifest['scenario'], manifest['concurrency'], manifest['payload']['bytes'] if manifest['payload'] else None] != case):
        raise ValueError('window does not satisfy frozen official parameters')
    for label, seconds in [('warmup', protocol['warmup_s']), ('measurement', protocol['measurement_s'])]:
        if (manifest[label + '_end_utc_ns'] - manifest[label + '_start_utc_ns']) / 1e9 < seconds - .1:
            raise ValueError('truncated official window')


def validate(config_path, restoration_path, output):
    config = json.loads(config_path.read_text())
    root = Path(config['output'])
    campaign = json.loads((root / 'campaign.json').read_text())
    raw = Path(config['protocol']).read_bytes()
    protocol = json.loads(raw)
    protocol_sha = hashlib.sha256(raw).hexdigest()
    receipt_raw = Path(config['entry_evidence']).read_bytes()
    receipt = json.loads(receipt_raw)
    receipt_sha = hashlib.sha256(receipt_raw).hexdigest()
    if set(receipt['gates']) != set(protocol['entry_gates']):
        raise ValueError('incomplete frozen entry gates')
    for gate in receipt['gates'].values():
        if gate['passed'] is not True or hashlib.sha256(Path(gate['path']).read_bytes()).hexdigest() != gate['sha256']:
            raise ValueError('entry evidence changed')
    restore = json.loads(restoration_path.read_text())
    if (restore.get('restored') is not True or restore.get('errors') != []
            or restore.get('vm_state') != 'shut off' or not restore.get('evidence')):
        raise ValueError('verified final laboratory restoration required')
    for item in restore['evidence']:
        if hashlib.sha256(Path(item['path']).read_bytes()).hexdigest() != item['sha256']:
            raise ValueError('restoration evidence changed')
    if protocol['status'] != 'frozen' or protocol['official_execution_enabled'] is not True:
        raise ValueError('frozen official protocol required')
    if campaign['protocol_sha256'] != protocol_sha or receipt['protocol_sha256'] != protocol_sha:
        raise ValueError('campaign/protocol identity mismatch')
    if campaign['state'] != 'BLOCK_COMPLETE_PENDING_FULL_RESTORATION' or campaign['failures']:
        raise ValueError('partial or failed block cannot be accepted')
    cases = config['cases']
    if not cases or len({tuple(c) for c in cases}) != len(cases) or any(c not in protocol['cases'] for c in cases):
        raise ValueError('invalid declared subset')
    expected = []
    for repetition in range(1, protocol['repetitions'] + 1):
        for case in cases:
            for implementation in (['python','go'] if repetition % 2 else ['go','python']):
                expected.append((repetition, case, implementation))
    if len(campaign['runs']) != len(expected) or campaign['completed_windows'] != len(expected) or campaign['expected_windows'] != len(expected):
        raise ValueError('missing or additional official windows')
    environment_sha = None
    if protocol.get('require_environment'):
        environment_sha = hashlib.sha256(Path(config['environment']).read_bytes()).hexdigest()
        if receipt.get('environment_sha256') != environment_sha:
            raise ValueError('deployment environment receipt changed')
    host_checks = []
    host_records = [json.loads(s) for s in (root/'host-controls.jsonl').read_text().splitlines()] if protocol.get('host_capacity_policy') else []
    verified = []
    for run, (repetition, case, implementation) in zip(campaign['runs'], expected):
        scenario, concurrency, size = case
        key = f'{scenario}-c{concurrency}-b{size or 0}-r{repetition}'
        expected_path = root / (key + '-' + implementation)
        if (Path(run['path']) != expected_path or expected_path.is_symlink()
                or run['repetition'] != repetition or run['implementation'] != implementation or run['exit_code'] != 0):
            raise ValueError('changed order, repetition or window identity')
        manifest = json.loads((expected_path / 'manifest.json').read_text())
        validate_window(manifest, protocol, receipt_sha, case)
        if protocol.get('require_raw_binary_integrity') and manifest.get('payload') and not manifest['payload'].get('range_prefix_sha256'):
            raise ValueError('independent expected range digest missing')
        if environment_sha is not None and manifest.get('environment_sha256') != environment_sha:
            raise ValueError('deployment environment differs between windows')
        if (manifest['protocol']['sha256'] != protocol_sha or manifest['instrument_sha256'] != receipt['instrument_sha256']
                or manifest['code_sha'] != receipt['code_sha']
                or manifest['campaign_id'] != str(uuid.uuid5(uuid.UUID(campaign['campaign_id']), key))):
            raise ValueError('code, protocol, pair or instrument identity changed')
        if protocol.get('host_capacity_policy'):
            host_checks.append({'path':str(expected_path),'control':host_capacity_summary(host_records,manifest['measurement_start_utc_ns'],manifest['measurement_end_utc_ns'],protocol['host_capacity_policy'])})
        result = glance_run(expected_path)
        if not result['valid'] or result['resources'] is None:
            raise ValueError('raw window validation rejected')
        # Preserve swapping even when below the watchdog threshold. An accepted
        # CPU/memory comparison conservatively requires zero swap-out per window.
        records = [json.loads(s) for s in (expected_path / 'resources.jsonl').read_text().splitlines()]
        if records[-1]['swap_counters']['pswpout'] != records[0]['swap_counters']['pswpout']:
            raise ValueError('swap-out contaminates window')
        verified.append((repetition, case, implementation, result))
    metrics = {
        'latency_p50_ms': lambda r:r['http']['correct_latency_ms']['p50'],
        'latency_p95_ms': lambda r:r['http']['correct_latency_ms']['p95'],
        'latency_p99_ms': lambda r:r['http']['correct_latency_ms']['p99'],
        'goodput_ops_s': lambda r:r['http']['correct_ops_s'],
        'completed_requests_s': lambda r:r['http']['completed_requests_s'],
        'error_rate': lambda r:r['http']['errors']/r['http']['requests'],
        'cpu_s_per_correct_request': lambda r:r['resources']['cpu_s_per_correct_request'],
        'cpu_seconds_common_interval': lambda r:r['resources']['cpu_seconds'],
        'pss_mean_mib': lambda r:r['resources']['pss_mean_bytes']/1024**2,
        'pss_peak_observed_mib': lambda r:r['resources']['pss_peak_observed_bytes']/1024**2,
        'rss_peak_observed_mib': lambda r:r['resources']['rss_peak_observed_bytes']/1024**2,
        'goodput_mib_s': lambda r:sum(v['goodput_mib_s'] for v in r['operations'].values())}
    comparisons = []
    for case in cases:
        for metric, extract in metrics.items():
            blocks = []
            for repetition in range(1, protocol['repetitions']+1):
                values = {implementation:extract(result) for rep, selected, implementation, result in verified if rep == repetition and selected == case}
                blocks.append({'repetition':repetition,'stage':'official','protocol_sha256':protocol_sha,'values':values})
            result = paired(blocks,metric)
            result.update(case=case, selected_block_accepted=True)
            comparisons.append(result)
    report = {'schema':1,'stage':'official','selected_block_accepted':True,
              'full_protocol_complete':set(map(tuple,cases)) == set(map(tuple,protocol['cases'])),
              'selected_cases':cases,'protocol_cases':len(protocol['cases']),'validated_windows':len(verified),
              'independent_pairs_per_case':protocol['repetitions'],'protocol_sha256':protocol_sha,
              'restoration_sha256':hashlib.sha256(restoration_path.read_bytes()).hexdigest(),
              'host_capacity_checks':host_checks,
              'host_controls_sha256':hashlib.sha256((root/'host-controls.jsonl').read_bytes()).hexdigest() if host_checks else None,
              'comparisons':comparisons,'limitations':['subset acceptance does not complete Glance benchmark',
                'per-run percentile comparisons; percentiles are not pooled or averaged',
                'bootstrap intervals descriptive with only five independent pairs',
                'implementation, DB schema, middleware and driver differences remain',
                'CPU common interval and end-to-end HTTP measurement boundaries differ slightly']}
    output.mkdir(exist_ok=False)
    (output/'acceptance.json').write_text(json.dumps(report,indent=2)+'\n')
    with (output/'paired-comparisons.csv').open('x') as stream:
        writer=csv.writer(stream)
        writer.writerow(['scenario','concurrency','bytes','metric','n_pairs','median_paired_difference_go_minus_python','ci95_low','ci95_high','median_ratio_go_over_python'])
        for row in comparisons:
            d=row['difference_go_minus_python'];ci=d['ci95_median'] or [None,None]
            writer.writerow([*row['case'],row['metric'],d['n'],d['median'],*ci,row['ratio_go_over_python']['median'] if row['ratio_go_over_python'] else None])
    return report


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--config',type=Path,required=True)
    p.add_argument('--restoration',type=Path,required=True)
    p.add_argument('--output',type=Path,required=True)
    args=p.parse_args()
    try:
        validate(args.config,args.restoration,args.output)
        return 0
    except Exception as exc:
        print('campaign acceptance rejected: '+type(exc).__name__+': '+str(exc),file=sys.stderr)
        return 2


if __name__=='__main__':
    sys.exit(main())
