#!/usr/bin/env python3
"""Reproducible raw Glance summaries and paired/factorial block analysis.

Never upgrades a pilot/partial run to official. Intervals resample independent
repetitions; request percentiles remain per run, never averaged into a percentile.
"""
import argparse
import csv
import hashlib
import json
import math
from pathlib import Path
import random
import statistics
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
from placement_http import percentile, summarize
from placement_resources import interval_summary


def distribution(values, seed=20261031, resamples=10000):
    if not values or any(type(v) not in (int, float) or not math.isfinite(v) for v in values):
        raise ValueError('finite independent block values required')
    result = {'n': len(values), 'median': statistics.median(values), 'min': min(values), 'max': max(values),
              'mean': statistics.mean(values), 'stdev': statistics.stdev(values) if len(values) > 1 else None,
              'ci95_median': None, 'unit': 'independent repetitions'}
    if len(values) >= 5:
        rng = random.Random(seed)
        draws = [statistics.median(rng.choices(values, k=len(values))) for _ in range(resamples)]
        result['ci95_median'] = [percentile(draws, .025), percentile(draws, .975)]
    return result


def verify_checksums(root):
    lines = (root / 'SHA256SUMS').read_text().splitlines()
    files = set()
    for line in lines:
        digest, name = line.split('  ', 1)
        path = root / name
        if path.parent != root or path.is_symlink() or not path.is_file() or name in files:
            raise ValueError('unsafe checksum manifest')
        h = hashlib.sha256()
        with path.open('rb') as stream:
            for block in iter(lambda: stream.read(256 * 1024), b''):
                h.update(block)
        if h.hexdigest() != digest:
            raise ValueError('artifact checksum mismatch')
        files.add(name)
    actual = {p.name for p in root.iterdir() if p.is_file() and p.name != 'SHA256SUMS'}
    if actual != files:
        raise ValueError('unmanifested artifact')
    if not {'manifest.json', 'report.json', 'ledger.jsonl', 'samples.jsonl'}.issubset(files):
        raise ValueError('incomplete raw artifact checksums')



def validate_binary_samples(manifest, rows):
    payload = manifest.get('payload')
    if not payload:
        return
    for sample in rows:
        if sample.get('valid') is not True:
            continue
        if sample.get('operation') == 'download':
            if (sample['response_bytes'] != payload['bytes'] or
                    any(sample.get('digests',{}).get(key) != payload[key] for key in ('sha256','sha512'))):
                raise ValueError('raw download integrity mismatch')
        elif sample.get('operation') == 'range':
            length = min(payload['bytes'],65536)
            if sample['response_bytes'] != length or sample.get('content_range') != f"bytes 0-{length-1}/{payload['bytes']}":
                raise ValueError('raw range length/header mismatch')
            if payload.get('range_prefix_sha256') and sample.get('digests',{}).get('sha256') != payload['range_prefix_sha256']:
                raise ValueError('raw range digest mismatch')


def glance_run(root, include_resources=True):
    verify_checksums(root)
    report = json.loads((root / 'report.json').read_text())
    m = json.loads((root / 'manifest.json').read_text())
    if report['manifest'] != m:
        raise ValueError('report/manifest mismatch')
    rows = [json.loads(s) for s in (root / 'samples.jsonl').read_text().splitlines()]
    validate_binary_samples(m, rows)
    selected = [s for s in rows if s['phase'] == 'measurement']
    if not selected or report['http'] is None:
        return {'run_id': m['run_id'], 'stage': m['stage'], 'valid': False, 'reason': 'no measurement', 'official_accepted': False}
    if any(type(s['valid']) is not bool or s['elapsed_ns'] != s['finish_ns'] - s['begin_ns'] for s in rows):
        raise ValueError('inconsistent sample timing or classification')
    summary = summarize(selected, report['http']['elapsed_s'])
    if summary != report['http']:
        raise ValueError('raw/summary mismatch')
    groups = {}
    for op in sorted({s['operation'] for s in selected}):
        samples = [s for s in selected if s['operation'] == op]
        valid = [s for s in samples if s['valid']]
        latencies = [s['elapsed_ns'] / 1e6 for s in valid]
        stats = summarize(samples, summary['elapsed_s'])
        if stats != report['operations'].get(op):
            raise ValueError('operation report does not match raw data')
        stats.update(mean_latency_ms=statistics.mean(latencies) if latencies else None,
                     median_latency_ms=statistics.median(latencies) if latencies else None,
                     stdev_latency_ms=statistics.stdev(latencies) if len(latencies) > 1 else None,
                     error_rate=stats['errors'] / stats['requests'],
                     transferred_bytes=sum(s['response_bytes'] if op in ('download', 'range') else s['request_bytes']
                                           if op == 'upload' else 0 for s in valid))
        stats['goodput_mib_s'] = stats['transferred_bytes'] / 1024**2 / summary['elapsed_s']
        groups[op] = stats
    result = {'run_id': m['run_id'], 'stage': m['stage'], 'implementation': m['implementation'],
              'scenario': m['scenario'], 'concurrency': m['concurrency'], 'payload': m['payload'],
              'valid': (m['state'] == 'COMPLETE_PILOT' and m['stage'] in ('pilot', 'local-test') or m['state'] == 'COMPLETE_WINDOW' and m['stage'] == 'official' and bool(m.get('entry_evidence_sha256'))) and m['cleanup_verified'] and summary['errors'] == 0,
              'http': summary, 'operations': groups, 'official_accepted': False,
              'resources': None, 'resource_limitation': 'no co-located process samples supplied'}
    # Optional cgroup collector output must be copied only after collector exit;
    # freeze a new checksum set explicitly. UTC alignment requires external clock
    # evidence if generator and collector ran on different machines.
    resource_path = root / 'resources.jsonl'
    clock_path = root / 'clock-alignment.json'
    result['validation_scope'] = 'HTTP; process data absent'
    if not include_resources:
        result['resource_limitation'] = 'explicit HTTP-only analysis; process alignment not accepted'
        result['validation_scope'] = 'HTTP only; resource metrics excluded explicitly'
    if include_resources and resource_path.exists() and clock_path.exists():
        clock = json.loads(clock_path.read_text())
        if clock.get('verified') is not True or clock.get('uncertainty_ns', 10**12) > 100_000_000:
            raise ValueError('unverified process clock alignment')
        offset = clock['remote_minus_generator_ns']
        records = [json.loads(s) for s in resource_path.read_text().splitlines()]
        begin = m['measurement_start_utc_ns'] + offset
        end = m['measurement_end_utc_ns'] + offset
        mono = m['measurement_start_monotonic_ns']
        completions = [begin + s['finish_ns'] - mono for s in selected if s['valid']]
        result['resources'] = interval_summary(records, m.get('resource_group', 'glance'), begin, end, completions,
                                               cores=clock['service_vcpus'])
        result['resource_limitation'] = 'CPU/PSS on common interior interval; other groups retained separately'
        result['validation_scope'] = 'HTTP and process alignment'
    return result




def host_capacity_summary(records, start_ns, end_ns, policy):
    """Apply predeclared host limits without treating swap occupancy as pressure."""
    rows = [r for r in records if start_ns <= r['utc_ns'] <= end_ns]
    if len(rows) < 3:
        raise ValueError('insufficient host control samples')
    minimum = min(r['mem_available_bytes'] for r in rows)
    full = [float(dict(token.split('=', 1) for token in r['pressure_memory'].split('full ')[1].split()).get('avg10')) for r in rows]
    if any(not math.isfinite(value) or value < 0 for value in full):
        raise ValueError('invalid host PSI sample')
    if minimum < policy['minimum_available_bytes']:
        raise ValueError('host memory reserve breached')
    if max(full) >= policy['maximum_full_psi_avg10_percent']:
        raise ValueError('host memory PSI threshold breached')
    if rows[-1]['oom_kill'] != rows[0]['oom_kill']:
        raise ValueError('host OOM during window')
    sustained = maximum_sustained = maximum_rate = total = 0
    for a, b in zip(rows, rows[1:]):
        seconds = (b['utc_ns']-a['utc_ns'])/1e9
        if seconds <= 0 or seconds > 6.5:
            raise ValueError('host control sampling gap')
        delta = b['pswpout']-a['pswpout']
        if delta < 0:
            raise ValueError('host swap counter reset')
        size = b.get('page_size_bytes', policy['page_size_bytes'])
        rate = delta*size/seconds
        total += delta*size
        maximum_rate = max(maximum_rate, rate)
        sustained = sustained+seconds if rate >= policy['sustained_swapout_bytes_per_s'] else 0
        maximum_sustained = max(maximum_sustained, sustained)
        if sustained >= policy['sustained_swapout_seconds']:
            raise ValueError('sustained host swap-out contaminates window')
    return {'samples':len(rows), 'minimum_available_bytes':minimum,
            'maximum_full_psi_avg10_percent':max(full), 'swapout_bytes':total,
            'maximum_swapout_bytes_per_s':maximum_rate, 'maximum_sustained_swapout_seconds':maximum_sustained,
            'policy':policy}


def paired(blocks, metric):
    """Explicit Python/Go observations from complete independent repetitions."""
    seen, values = set(), []
    for b in blocks:
        key = (b['protocol_sha256'], b['stage'], b['repetition'])
        if key in seen or set(b['values']) != {'python', 'go'}:
            raise ValueError('unique complete Python/Go pairs required')
        seen.add(key)
        v = b['values']
        if any(type(x) not in (int, float) or not math.isfinite(x) for x in v.values()):
            raise ValueError('finite pair observations required')
        values.append(v)
    if not values or len({(b['protocol_sha256'], b['stage']) for b in blocks}) != 1:
        raise ValueError('one protocol and stage required')
    ratios = [v['go']/v['python'] for v in values] if all(v['python'] > 0 and v['go'] >= 0 for v in values) else None
    return {'metric': metric, 'stage': blocks[0]['stage'], 'n_complete_pairs': len(values),
            'official_accepted': False,
            'difference_go_minus_python': distribution([v['go']-v['python'] for v in values]),
            'ratio_go_over_python': distribution(ratios) if ratios else None,
            'percent_change': distribution([(x-1)*100 for x in ratios]) if ratios else None,
            'limitations': ['separate operations/sizes/concurrency before pairing; never pool requests as repetitions',
                            'protocol and deployment provenance verified separately; intervals descriptive']}


def factorial(blocks, metric):
    """One complete A/B/C/D observation per independent repetition."""
    observations, seen = [], set()
    for b in blocks:
        key = (b['protocol_sha256'], b['stage'], b['repetition'])
        if key in seen:
            raise ValueError('duplicate independent repetition')
        seen.add(key)
        v = b['values']
        if set(v) != set('ABCD') or any(type(x) not in (int, float) or not math.isfinite(x) for x in v.values()):
            raise ValueError('complete finite A/B/C/D block required')
        observations.append(v)
    if not observations or len({(b['protocol_sha256'], b['stage']) for b in blocks}) != 1:
        raise ValueError('one protocol and one stage required')
    result = {'metric': metric, 'stage': blocks[0]['stage'], 'n_complete_blocks': len(blocks),
              'official_accepted': False, 'comparisons': {}}
    for a, b in [('A', 'B'), ('A', 'C'), ('A', 'D'), ('B', 'D'), ('C', 'D')]:
        differences = [v[b] - v[a] for v in observations]
        ratios = [v[b] / v[a] for v in observations] if all(v[a] > 0 and v[b] >= 0 for v in observations) else None
        result['comparisons'][a + '_vs_' + b] = {'difference_second_minus_first': distribution(differences),
                                                'ratio_second_over_first': distribution(ratios) if ratios else None,
                                                'percent_change': distribution([(x - 1) * 100 for x in ratios]) if ratios else None}
    result['interaction_D_minus_B_minus_C_plus_A'] = distribution([v['D'] - v['B'] - v['C'] + v['A'] for v in observations])
    result['limitations'] = ['no causal attribution to language alone', 'bootstrap intervals descriptive; no automatic significance claim',
                             'configuration identity and functional gates must be established separately']
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest='action', required=True)
    g = sub.add_parser('glance')
    g.add_argument('runs', type=Path, nargs='+')
    g.add_argument('--output', type=Path, required=True)
    g.add_argument('--http-only', action='store_true')
    for name in ('factorial', 'paired'):
        f = sub.add_parser(name)
        f.add_argument('--blocks', type=Path, required=True)
        f.add_argument('--metric', required=True)
        f.add_argument('--output', type=Path, required=True)
    args = p.parse_args()
    try:
        if args.action == 'glance':
            results = [glance_run(root, include_resources=not args.http_only) for root in args.runs]
            args.output.mkdir(parents=False, exist_ok=False)
            (args.output / 'summary.json').write_text(json.dumps(results, indent=2) + '\n')
            with (args.output / 'operations.csv').open('x') as stream:
                writer = csv.writer(stream)
                writer.writerow(['run_id', 'stage', 'implementation', 'scenario', 'concurrency', 'valid', 'operation',
                                 'correct', 'errors', 'p50_ms', 'p95_ms', 'p99_ms', 'goodput_ops_s', 'goodput_mib_s'])
                for r in results:
                    for op, v in r.get('operations', {}).items():
                        writer.writerow([r['run_id'], r['stage'], r['implementation'], r['scenario'], r['concurrency'],
                                         r['valid'], op, v['correct'], v['errors'], *v['correct_latency_ms'].values(),
                                         v['correct_ops_s'], v['goodput_mib_s']])
        else:
            analyzer = factorial if args.action == 'factorial' else paired
            result = analyzer(json.loads(args.blocks.read_text()), args.metric)
            with args.output.open('x') as stream:
                json.dump(result, stream, indent=2)
                stream.write('\n')
    except Exception as error:
        print('analysis failed: ' + type(error).__name__, file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
