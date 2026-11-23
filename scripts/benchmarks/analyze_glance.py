#!/usr/bin/env python3
"""Reproduce Glance byte/contract throughput and aligned cgroup CPU/PSS."""
import argparse
import gzip
import hashlib
import json
from pathlib import Path
import statistics

from analyze_placement import bootstrap, nanos
from glance_campaign import schedule, validate_protocol
from placement_http import percentile
from placement_resources import interval_summary




def cgroup_memory(path, implementation, start, end):
    """Unit memory from glance_memory.py (10 Hz cgroup v2) inside the window:
    private anon mean/peak and page cache ('file') mean. Absent file: {}."""
    if not path.exists():
        return {}
    anon, file = [], []
    for line in path.read_text().splitlines():
        row = json.loads(line)
        value = row['groups'].get(implementation)
        if value and start <= row['utc_ns'] <= end and 'anon' in value:
            anon.append(value['anon'])
            file.append(value.get('file', 0))
    if len(anon) < 3:
        return {}
    return {'cgroup_anon_mean_bytes': statistics.mean(anon), 'cgroup_anon_peak_bytes': max(anon),
            'cgroup_file_mean_bytes': statistics.mean(file), 'cgroup_samples': len(anon)}

def memory_extras(records, implementation, start, end, warmup_start):
    """RAM beyond PSS, from the same 1 Hz samples: the service's RSS (mean and
    observed peak) inside the window, and the VM's RAM used beyond its idle
    level before warm-up (idle MemAvailable minus the window's minimum)."""
    def window(row):
        return start <= row['read_finished_utc_ns'] <= end
    rss = [sum(proc['rss_bytes'] for proc in row['groups'][implementation]['processes']) for row in records
           if window(row) and row['groups'][implementation]['processes']
           and all(proc.get('rss_bytes') is not None for proc in row['groups'][implementation]['processes'])]
    idle = [row['vm_mem_available_kib'] * 1024 for row in records if row['read_finished_utc_ns'] < warmup_start and row.get('vm_mem_available_kib')]
    busy = [row['vm_mem_available_kib'] * 1024 for row in records if window(row) and row.get('vm_mem_available_kib')]
    out = {}
    if rss:
        out.update(rss_mean_bytes=statistics.mean(rss), rss_peak_observed_bytes=max(rss))
    if idle and busy:
        out.update(vm_mem_available_min_bytes=min(busy), vm_ram_used_extra_peak_bytes=max(0, statistics.mean(idle) - min(busy)))
    return out

def distribution(values):
    if not values:
        return None
    return {'n': len(values), **{key: percentile(values, q) for key, q in [('p50', .5), ('p95', .95), ('p99', .99)]},
            'p99_tail_supported': len(values) >= 1000}


def validate_fd_stability(resources, maximum):
    if any(type(resources.get(k)) is not int for k in ['open_fds_start', 'open_fds_end', 'open_fds_peak']):
        raise ValueError('descriptor counts required for v3')
    if resources['open_fds_peak'] - resources['open_fds_start'] > maximum:
        raise ValueError('descriptor accumulation exceeds frozen v3 window bound')


def analyze(root):
    manifest = json.loads((root / 'manifest.json').read_text())
    p = manifest['protocol']; validate_protocol(p)
    pilot = manifest['stage'] == 'pilot'
    expected = schedule(p, manifest['campaign_id'], pilot)
    expected_ids = {pair['pair_id'] + '-' + impl for pair in expected for impl in ('python', 'go')}
    errors, runs = [], {}
    if manifest['sequence'] != expected:
        errors.append({'error': 'frozen randomized sequence differs from reproducible matrix'})
    for entry in manifest['runs']:
        ident = entry['run_id']
        try:
            if ident in runs or ident not in expected_ids:
                raise ValueError('duplicate or foreign run')
            report = json.loads((root / ident / 'report.json').read_text())
            m = report['measurement']
            if not report['valid'] or not entry['valid'] or not report['cleanup_ok'] or not report['clock_valid'] or not m or m['errors'] or m['failed']:
                raise ValueError('invalid HTTP/cleanup/clock run')
            if ident != report['run_id'] or ident != f"{report['scenario']}-c{report['concurrency']:02}-r{int(ident.split('-r')[1].split('-')[0]):02}-{report['implementation']}":
                raise ValueError('report identity mismatch')
            samples = []
            with gzip.open(root / ident / 'measurement-samples.jsonl.gz', 'rt') as stream:
                for line in stream:
                    samples.append(json.loads(line))
            if len(samples) != m['requests'] or not samples or m['correct'] != len(samples) or any(not s['valid'] or s['error_kind'] or s['run_id'] != ident for s in samples):
                raise ValueError('raw request counts/validity disagree')
            expected_operations = (['fresh-id', 'create', 'patch', 'verify', 'delete', 'verify-absent'] if report['scenario'] == 'catalog-lifecycle'
                                   else ['fresh-id', 'create', 'upload', 'verify-upload', 'delete', 'verify-absent'] if report['scenario'].startswith('upload-')
                                   else ['download'] if report['scenario'].startswith('download-')
                                   else ['show', 'download', 'list'] if report['scenario'] == 'nova-mix' else [report['scenario']])
            def cycle_ok(ops):
                if report['scenario'].startswith('import-'):
                    # import-wait repeats until the image is active
                    head, tail = ['fresh-id', 'create', 'stage', 'import'], ['verify-import', 'delete', 'verify-absent']
                    middle = ops[len(head):len(ops) - len(tail)]
                    return ops[:len(head)] == head and ops[len(ops) - len(tail):] == tail and middle and set(middle) == {'import-wait'}
                return ops == expected_operations
            cycles, per_operation, ttfb = {}, {}, []
            transfer = 0
            for s in samples:
                if s['offset_ns'] < 0 or s['elapsed_ns'] < 0 or not 0 <= s['worker'] < report['concurrency']:
                    raise ValueError('invalid worker/time sample')
                cycles.setdefault((s['worker'], s['cycle']), []).append(s['operation'])
                per_operation.setdefault(s['operation'], []).append(s['elapsed_ns'] / 1e6)
                if s['ttfb_ns'] is not None:
                    ttfb.append(s['ttfb_ns'] / 1e6)
                if s['operation'] == 'download':
                    transfer += s['response_bytes']
                if s['operation'] in ('upload', 'stage'):
                    transfer += s['request_bytes']
            if len(cycles) != m['cycles'] or any(not cycle_ok(ops) for ops in cycles.values()) or transfer != m['validated_payload_bytes']:
                raise ValueError('incomplete raw cycle or payload accounting mismatch')
            rates = {'correct_ops_s': len(samples) / m['elapsed_s'], 'complete_cycles_s': len(cycles) / m['elapsed_s'],
                     'validated_payload_mib_s': transfer / m['elapsed_s'] / 1024**2}
            if any(abs(m[k] - value) > max(1e-9, value * 1e-9) for k, value in rates.items()):
                raise ValueError('reported rate disagrees with raw samples')
            latencies = distribution([s['elapsed_ns'] / 1e6 for s in samples])
            if any(latencies[k] != m['latency_ms'][k] for k in ('p50', 'p95', 'p99')):
                raise ValueError('reported percentile differs from samples')
            resource_root = root / 'lab' / (ident + '-resources')
            raw = resource_root / 'resources.jsonl'
            with (raw.open('rt') if raw.exists() else gzip.open(str(raw) + '.gz', 'rt')) as stream:
                records = [json.loads(line) for line in stream]
            collector = json.loads((resource_root / 'collector.json').read_text())
            if collector['self_terminated_at_limit']:
                raise ValueError('collector duration limit reached')
            clocks = report['clocks']
            offset = sum(c['best']['remote_minus_host_ns'] for c in clocks.values()) // 2
            start, end = nanos(m['start_utc']) + offset, nanos(m['end_utc']) + offset
            completions = [start + s['offset_ns'] + s['elapsed_ns'] for s in samples]
            resources = interval_summary(records, report['implementation'], start, end, completions, ticks=collector['clock_ticks_s'])
            if p['protocol'] in ('glance-compact-v3', 'glance-compact-v4'):
                validate_fd_stability(resources, p['maximum_open_fd_growth_per_window'])
            resources['clock_uncertainty_ns'] = max(c['best']['rtt_ns'] // 2 for c in clocks.values()) + report['clock_drift_ns']
            warmup_start = nanos(report['warmup']['start_utc']) + offset
            idle = [sum(proc['pss_bytes'] for proc in row['groups'][report['implementation']]['processes']) for row in records
                    if row['read_finished_utc_ns'] < warmup_start and row['groups'][report['implementation']]['processes']
                    and all(proc['pss_bytes'] is not None for proc in row['groups'][report['implementation']]['processes'])]
            if len(idle) < 3:
                raise ValueError('insufficient idle PSS baseline')
            resources.update(idle_pss_mean_bytes=statistics.mean(idle), idle_samples=len(idle))
            resources.update(memory_extras(records, report['implementation'], start, end, warmup_start))
            resources.update(cgroup_memory(root / 'lab' / (ident + '-memory.jsonl'), report['implementation'], start, end))
            if 'memcached' in records[0]['groups']:
                cache = [sum(proc['pss_bytes'] for proc in row['groups']['memcached']['processes']) for row in records
                         if start <= row['read_finished_utc_ns'] <= end and row['groups']['memcached']['processes']
                         and all(proc['pss_bytes'] is not None for proc in row['groups']['memcached']['processes'])]
                if cache:
                    resources['memcached_pss_mean_bytes'] = statistics.mean(cache)
            controls = {}
            for label in records[0]['groups']:
                if label == report['implementation']:
                    continue
                try:
                    controls[label] = interval_summary(records, label, start, end, [], ticks=collector['clock_ticks_s'])
                except ValueError:
                    controls[label] = {'static_group_unavailable': True, 'raw_membership_preserved': True}
            runs[ident] = {'valid': True, 'scenario': report['scenario'], 'concurrency': report['concurrency'],
                           'implementation': report['implementation'], **rates,
                           **{k + '_ms': latencies[k] for k in ('p50', 'p95', 'p99')},
                           'latency_ms': latencies, 'per_operation_latency_ms': {k: distribution(v) for k, v in per_operation.items()},
                           'download_ttfb_ms': distribution(ttfb), 'requests': len(samples), 'cycles': len(cycles),
                           # A client with the former 10 s timeout would have failed these.
                           'requests_over_10s': sum(s['elapsed_ns'] > 10_000_000_000 for s in samples),
                           'max_request_s': max(s['elapsed_ns'] for s in samples) / 1e9,
                           'actual_elapsed_s': m['elapsed_s'], 'resources': resources, 'controls': controls}
        except (OSError, ValueError, KeyError, TypeError, ZeroDivisionError) as error:
            errors.append({'run_id': ident, 'error': str(error) if isinstance(error, ValueError) else type(error).__name__})
    observed_ids = [entry['run_id'] for entry in manifest['runs']]
    complete = (manifest['valid'] and manifest['cleanup'] and len(observed_ids) == len(expected_ids)
                and set(observed_ids) == expected_ids and set(runs) == expected_ids and not errors)
    groups = []
    metrics = ['correct_ops_s', 'complete_cycles_s', 'validated_payload_mib_s', 'p50_ms', 'p95_ms', 'p99_ms']
    resource_metrics = ['cpu_seconds', 'cpu_fraction_of_four_cores', 'cpu_s_per_correct_request', 'pss_mean_bytes',
                        'pss_peak_observed_bytes', 'idle_pss_mean_bytes', 'collector_core_fraction',
                        'rss_mean_bytes', 'rss_peak_observed_bytes', 'vm_ram_used_extra_peak_bytes', 'vm_mem_available_min_bytes',
                        'cgroup_anon_mean_bytes', 'cgroup_anon_peak_bytes', 'cgroup_file_mean_bytes', 'memcached_pss_mean_bytes']
    for scenario in p['scenarios']:
        for c in p['concurrency']:
            group = {'scenario': scenario['id'], 'concurrency': c, 'implementations': {}, 'paired_go_over_python': {}}
            for impl in ('python', 'go'):
                rows = [r for r in runs.values() if r['scenario'] == scenario['id'] and r['concurrency'] == c and r['implementation'] == impl]
                values = {k: [r[k] for r in rows] for k in metrics}
                values.update({k: [r['resources'][k] for r in rows if r['resources'].get(k) is not None] for k in resource_metrics})
                group['implementations'][impl] = {'runs': len(rows), **{k: bootstrap(v) for k, v in values.items() if v}}
            ratios = {}
            for pair in expected:
                if pair['scenario'] != scenario['id'] or pair['concurrency'] != c:
                    continue
                py, go = (runs.get(pair['pair_id'] + '-' + impl) for impl in ('python', 'go'))
                if not py or not go:
                    continue
                for k in metrics + resource_metrics:
                    a, b = (py[k], go[k]) if k in metrics else (py['resources'].get(k), go['resources'].get(k))
                    if a is not None and b is not None and a > 0:
                        ratios.setdefault(k, []).append(b / a)
            group['paired_go_over_python'] = {k: bootstrap(v) for k, v in ratios.items()}
            groups.append(group)
    return {'schema': 1, 'campaign_id': manifest['campaign_id'], 'stage': manifest['stage'],
            'complete_valid': complete, 'accepted': complete and not pilot, 'pilot_validated': complete and pilot,
            'runs_required': len(expected_ids), 'runs_observed': len(runs), 'errors': errors, 'runs': runs, 'groups': groups,
            'notification_policy': p.get('notification_policy', 'original'), 'go_notification_policy': p.get('go_notification_policy', 'noop'), 'protocol': p, 'measurement_head': manifest['git_base'], 'protocol_sha256': manifest['protocol_sha256'],
            'limitations': ['Notification policy: Python ' + p.get('notification_policy', 'original') + ', Go ' + p.get('go_notification_policy', 'noop') + '; Python without bus notifications (v3/v4); does not validate stock messagingv2 stability',
                            'Independent paired repetitions per cell; median and observed range, no statistical significance claim',
                            'Closed-loop complete cycles including drain; upload cycle includes create/verify/delete and is not pure PUT throughput',
                            'Download checks every full payload; upload load verifies active metadata/hash, worker canaries round-trip outside measured windows',
                            'Public Apache path fixed; Python caches/workers/shared DB differ from Go isolated DB/store; no language-only attribution',
                            'Python soft-delete tombstones are retained; only owned blobs and active metadata deleted via API, no SQL purge',
                            'CPU/op uses all correct contract requests in common sample interval, including verification/404; PSS at1Hz, observed peak',
                            'Per-operation percentiles preserve mixed-cycle meaning; n=3 boot or joint performance not measured here']}


def publish(summary, output):
    if not summary['accepted']:
        raise ValueError('only complete official Glance campaign can be published as accepted')
    lines = ['# Glance Python frente a Go: ' + summary['protocol']['protocol'], '',
             f"Resultado: {summary['runs_observed']}/{summary['runs_required']} ventanas válidas; HTTP/bytes/estado y CPU/PSS comprobados.",
             'Tres pares por celda; concurrencias1/16; calentamiento10s y carga30s más drenaje completo.',
             f"Fuente medida: `{summary['measurement_head']}`. Placement Python permanece como control.",
             'Política de notificaciones: Python ' + summary['protocol'].get('notification_policy', 'original') + ', Go ' + summary['protocol'].get('go_notification_policy', 'noop')
             + ('. La revisión4 repite la matriz v3; Go además envía sus notificaciones (más trabajo que Python).' if summary['protocol']['protocol'] == 'glance-compact-v4'
                else '. La revisión3 mide API/catálogo/bytes sin bus RabbitMQ; conserva caché Python y documenta por separado el fallo stock v2.'), '',
             '| Carga | C | Ciclos/s Python | Ciclos/s Go | MiB/s Python | MiB/s Go | PSS MiB Python | PSS MiB Go |',
             '|---|---:|---:|---:|---:|---:|---:|---:|']
    for g in summary['groups']:
        py, go = [g['implementations'][impl] for impl in ('python', 'go')]
        def val(row, key, scale=1):
            return f"{row[key]['median'] / scale:.3f}"
        lines.append(f"| {g['scenario']} | {g['concurrency']} | {val(py,'complete_cycles_s')} | {val(go,'complete_cycles_s')} | {val(py,'validated_payload_mib_s')} | {val(go,'validated_payload_mib_s')} | {val(py,'pss_mean_bytes',1024**2)} | {val(go,'pss_mean_bytes',1024**2)} |")
    lines += ['', 'Se publican medianas de tres corridas. Rangos, ratios pareados, percentiles por operación, TTFB, CPU/op y muestras completas están en summary.json y evidencia oficial.', '', '## Límites', '']
    lines += ['- ' + value for value in summary['limitations']]
    lines += ['', 'La campaña conjunta se postergó por instrucción del usuario. No se atribuyen resultados de Nova ni de ambas sustituciones a esta campaña.', '']
    output.write_text('\n'.join(lines))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('root', type=Path)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    result = analyze(args.root)
    with args.output.open('x') as stream:
        json.dump(result, stream, indent=2); stream.write('\n')
    print(json.dumps({k: result[k] for k in ['accepted', 'pilot_validated', 'runs_observed', 'runs_required', 'errors']}))


if __name__ == '__main__':
    main()
