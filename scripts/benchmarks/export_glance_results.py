#!/usr/bin/env python3
"""Export verified real or test raw runs without changing their acceptance stage."""
import argparse
import csv
import hashlib
import json
from pathlib import Path
import sys

from experiment_analysis import glance_run


def export(runs, output, http_only=False):
    # Parse every input before creating output; bad checksums/clock fail closed.
    rows = [glance_run(p, include_resources=not http_only) for p in runs]
    if not rows or any(not row['valid'] for row in rows):
        raise ValueError('all input runs must pass raw validation')
    stages = {r['stage'] for r in rows}
    if len(stages) != 1:
        raise ValueError('one stage per export required')
    import matplotlib
    matplotlib.use('Agg')
    import matplotlib.pyplot as plt
    output.mkdir(exist_ok=False)
    (output / 'summary.json').write_text(json.dumps(rows, indent=2) + '\n')
    with (output / 'operations.csv').open('x') as stream:
        writer = csv.writer(stream)
        writer.writerow(['run_id', 'stage', 'implementation', 'scenario', 'concurrency', 'payload_bytes',
                         'operation', 'correct', 'errors', 'p50_ms', 'p95_ms', 'p99_ms', 'goodput_ops_s', 'goodput_mib_s',
                         'mean_latency_ms','median_latency_ms','stdev_latency_ms','error_rate'])
        for row in rows:
            for op, values in row['operations'].items():
                writer.writerow([row['run_id'], row['stage'], row['implementation'], row['scenario'], row['concurrency'],
                                 row['payload']['bytes'] if row['payload'] else 0, op, values['correct'], values['errors'],
                                 *[values['correct_latency_ms'][q] for q in ('p50', 'p95', 'p99')],
                                 values['correct_ops_s'], values['goodput_mib_s'],
                                 *[values[k] for k in ['mean_latency_ms','median_latency_ms','stdev_latency_ms','error_rate']]])
    with (output / 'resources.csv').open('x') as stream:
        writer = csv.writer(stream)
        keys = ['cpu_seconds','covered_s','correct_requests_common_interval','cpu_s_per_correct_request',
                'pss_mean_bytes','pss_peak_observed_bytes','rss_peak_observed_bytes','samples',
                'collector_cpu_seconds','max_read_duration_ms']
        writer.writerow(['run_id','stage','implementation','scenario','concurrency','payload_bytes','scope',*keys])
        for row in rows:
            metrics = row['resources'] or {}
            writer.writerow([row['run_id'],row['stage'],row['implementation'],row['scenario'],row['concurrency'],
                             row['payload']['bytes'] if row['payload'] else 0,row['validation_scope'],
                             *[metrics.get(k) for k in keys]])
    # Each point is one independent run. Never average percentiles or silently
    # pool multiple runs; repeated cases remain distinct observations.
    labels = [f"{r['scenario']} c{r['concurrency']} {r['payload']['bytes']//1024**2 if r['payload'] else 0}MiB {r['implementation']}" for r in rows]
    for name, unit, values in [
        ('latency-p50', 'p50 request latency (ms)', [r['http']['correct_latency_ms']['p50'] for r in rows]),
        ('goodput', 'correct requests / s', [r['http']['correct_ops_s'] for r in rows])]:
        fig, axis = plt.subplots(figsize=(12, max(5, len(rows) * .26)))
        axis.barh(range(len(rows)), values, color=['#446b9e' if r['implementation'] == 'python' else '#42836a' for r in rows])
        axis.set_yticks(range(len(rows)), labels, fontsize=8)
        axis.invert_yaxis()
        axis.set_xlabel(unit)
        axis.set_title(f"Glance {next(iter(stages))}; each bar = one run; no confidence interval")
        fig.text(.01, .005, 'Client wall time; authentication enabled; closed loop; see provenance.json for raw sources and limitations.', fontsize=8)
        fig.tight_layout(rect=(0, .025, 1, 1))
        for suffix in ('png', 'pdf'):
            fig.savefig(output / (name + '.' + suffix), dpi=180)
        plt.close(fig)
    provenance = {'stage': next(iter(stages)), 'official_accepted': False,
                  'raw_sources': [str(p.resolve()) for p in runs], 'http_only': http_only,
                  'instrument_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                  'limitations': ['one bar per run, not a paired effect estimate',
                                  'short pilots are not official performance conclusions',
                                  'CPU/PSS omitted when clock alignment is not accepted',
                                  'implementation comparison cannot isolate language causality']}
    (output / 'provenance.json').write_text(json.dumps(provenance, indent=2) + '\n')
    (output / 'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest() + '  ' + p.name + '\n' for p in sorted(output.iterdir()) if p.is_file()))
    return rows


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('runs', nargs='+', type=Path)
    p.add_argument('--output', type=Path, required=True)
    p.add_argument('--http-only', action='store_true')
    args = p.parse_args()
    try:
        export(args.runs, args.output, args.http_only)
        return 0
    except Exception as error:
        print('export rejected: ' + type(error).__name__, file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
