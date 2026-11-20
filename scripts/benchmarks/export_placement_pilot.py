#!/usr/bin/env python3
"""Export a reanalyzed historical Placement pilot; never official acceptance."""
import argparse
import csv
import hashlib
import json
from pathlib import Path


def export(summary_path, output, source):
    raw = summary_path.read_bytes()
    result = json.loads(raw)
    if result['stage'] != 'pilot' or result['b17_accepted'] or result['b18_accepted'] or result['errors']:
        raise ValueError('valid historical pilot summary required')
    rows = list(result['runs'].values())
    if len(rows) != result['runs_observed'] or any(not r['b17_valid'] or not r['b18_valid'] for r in rows):
        raise ValueError('every raw HTTP and resource run must be verified')
    cells = [(r['scenario'], r['concurrency'], r['implementation']) for r in rows]
    if len(set(cells)) != len(cells):
        raise ValueError('this exporter requires n=1 per pilot cell; no implicit pooling')
    output.mkdir(parents=False, exist_ok=False)
    columns = ['scenario', 'concurrency', 'implementation', 'p50_ms', 'p95_ms', 'p99_ms', 'correct_ops_s',
               'error_rate', 'cpu_seconds', 'cpu_s_per_correct_request', 'pss_mean_bytes', 'pss_peak_observed_bytes',
               'rss_peak_observed_bytes', 'idle_pss_mean_bytes']
    with (output / 'placement-pilot.csv').open('x') as stream:
        writer = csv.DictWriter(stream, fieldnames=columns)
        writer.writeheader()
        for r in sorted(rows, key=lambda x: (x['scenario'], x['concurrency'], x['implementation'])):
            row = dict(r, **r['resources'])
            writer.writerow({k: row[k] for k in columns})
    import matplotlib
    matplotlib.use('Agg')
    import matplotlib.pyplot as plt
    scenarios = list(dict.fromkeys(r['scenario'] for r in rows))
    concurrencies = sorted({r['concurrency'] for r in rows})
    charts = [('latency', 'p95_ms', 'p95 HTTP (ms)', 1), ('goodput', 'correct_ops_s', 'Correct requests/s', 1),
              ('cpu', 'cpu_s_per_correct_request', 'CPU ms / correct request\n(common interval)', 1000),
              ('memory', 'pss_mean_bytes', 'Mean process PSS (MiB)', 1/1024**2)]
    for filename, metric, label, scale in charts:
        fig, axes = plt.subplots(len(concurrencies), 1, figsize=(11, 7), squeeze=False)
        for ax, concurrency in zip(axes.flat, concurrencies):
            x = list(range(len(scenarios)))
            for implementation, shift, color in [('python', -.18, '#376AA0'), ('go', .18, '#C56A2D')]:
                values = []
                for scenario in scenarios:
                    r = next(r for r in rows if r['scenario'] == scenario and r['concurrency'] == concurrency and r['implementation'] == implementation)
                    values.append((r.get(metric) if metric in r else r['resources'][metric]) * scale)
                ax.bar([i+shift for i in x], values, width=.36, label=implementation, color=color)
            ax.set_xticks(x, scenarios, rotation=15, ha='right')
            ax.set_ylabel(label)
            ax.set_title(f'Concurrency {concurrency}; n=1 paired run per cell')
            ax.grid(axis='y', alpha=.2)
            ax.legend()
        fig.suptitle('HISTORICAL PILOT 07 — not official results\nPlacement Python/Go; warm-up 1 s, load 4 s, closed-loop', fontsize=12)
        fig.text(.02, .01, 'Source: ' + source + '\nNo confidence intervals (n=1); deployment/auth/DB differences; observed sampled memory, not absolute peak.', fontsize=8)
        fig.tight_layout(rect=(0, .07, 1, .91))
        fig.savefig(output / (filename + '.png'), dpi=160)
        fig.savefig(output / (filename + '.pdf'))
        plt.close(fig)
    provenance = {'source': source, 'summary_sha256': hashlib.sha256(raw).hexdigest(), 'instrument_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(), 'stage': 'historical-pilot',
                  'official_accepted': False, 'runs': len(rows), 'n_per_cell': 1,
                  'confidence_intervals': 'not estimated; n=1',
                  'note': 'No new measurements; raw HTTP/process data reanalyzed with existing instrument'}
    (output / 'provenance.json').write_text(json.dumps(provenance, indent=2) + '\n')


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--summary', type=Path, required=True)
    p.add_argument('--source', required=True)
    p.add_argument('--output', type=Path, required=True)
    args = p.parse_args()
    export(args.summary, args.output, args.source)


if __name__ == '__main__':
    main()
