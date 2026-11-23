#!/usr/bin/env python3
"""Timed Glance cycles with complete state/byte validation and owned cleanup."""
from concurrent.futures import ThreadPoolExecutor
import datetime
import gzip
import json
from pathlib import Path
import threading
import time
from urllib.parse import urlencode
import uuid

from glance_http import Client, metadata_matches
from placement_http import percentile


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def run(config, token, output, *, duration=30, warmup=10, abort_check=None):
    """One complete cycle may drain past deadline; its full time is counted."""
    scenarios = {'catalog-list', 'catalog-show', 'catalog-lifecycle',
                 'upload-1m', 'upload-16m', 'upload-64m', 'download-1m', 'download-16m', 'download-64m',
                 'import-direct-16m', 'upload-qcow2', 'nova-mix'}
    if config['scenario'] not in scenarios:
        raise ValueError('unsupported scenario before any request')
    Client(config['url'], token, timeout=config.get('metadata_timeout_s', 10), binary_timeout=config.get('binary_timeout_s', 120)).close()  # Validate the endpoint before worker barriers.
    if config['scenario'] in ('catalog-list', 'catalog-show', 'nova-mix') or config['scenario'].startswith('download-'):
        ids = [i['id'] for i in config['images']]
        if not ids or len(set(ids)) != len(ids):
            raise ValueError('unique seeded images required')
    if config['scenario'].startswith(('upload-', 'download-', 'import-')) or config['scenario'] == 'nova-mix':
        expected = config['payload']
        if type(expected['size']) is not int or not 0 < expected['size'] <= 64 * 1024**2:
            raise ValueError('controlled payload size required')
        if config['scenario'].startswith(('upload-', 'import-')) and Path(config['payload_file']).stat().st_size != expected['size']:
            raise ValueError('prepared payload length changed')
    output = Path(output)
    output.mkdir(parents=True, exist_ok=False)
    concurrency = config['concurrency']
    if concurrency not in (1, 4, 8, 16) or not .05 <= duration <= 3600 or not 0 <= warmup <= 3600:
        raise ValueError('invalid controlled window')
    namespace = uuid.UUID(config['namespace'])
    tag, owner = config['tag'], config['owner']
    expected = config.get('payload')
    upload = config.get('payload_file')
    pending, lock = {}, threading.Lock()
    cleanup_errors = []
    report = {'schema': 1, 'run_id': config['run_id'], 'scenario': config['scenario'],
              'implementation': config['implementation'], 'concurrency': concurrency,
              'planned_duration_s': duration, 'warmup_s': warmup,
              'load_model': 'closed-loop; one cycle per worker',
              'drain': 'complete cycle started before deadline; denominator includes entire cycle drain',
              'latency_boundary': 'before send through complete response; validation/logging outside request latency',
              'seed_scope': 'selected metadata fields, image identity/state, complete payload bytes and hashes',
              'valid': False}

    def owned(image, ident, name):
        return metadata_matches(image, {'id': ident, 'owner': owner, 'name': name, 'tags': [tag]})

    def phase(seconds, label):
        start, started_utc = [None], [None]
        failed = threading.Event()
        def release():
            started_utc[0], start[0] = utc(), time.monotonic_ns()
        barrier = threading.Barrier(concurrency + 1, action=release)
        with gzip.open(output / (label + '-samples.jsonl.gz'), 'xt') as raw, gzip.open(output / (label + '-intents.jsonl.gz'), 'xt') as intents:
            def worker(index):
                client = Client(config['url'], token, timeout=config.get('metadata_timeout_s', 10), binary_timeout=config.get('binary_timeout_s', 120))
                samples, cycles, validated_bytes = [], 0, 0
                def call(op, method, path, status, *, body=None, kind='application/json', download=False, file=None, validate=None, ident=None):
                    result, value = client.request(method, path, body=body, content_type=kind,
                                                    upload=file, expected_payload=expected if download else None)
                    valid = result['status'] == status and result['error_kind'] is None
                    if valid and validate is not None:
                        try:
                            valid = bool(validate(value))
                        except (KeyError, ValueError, TypeError, AttributeError):
                            valid = False
                    item = {'run_id': config['run_id'], 'worker': index, 'cycle': cycles,
                            'operation': op, 'method': method, 'path': path, 'image_id': ident,
                            'offset_ns': result['begin_ns'] - start[0], 'elapsed_ns': result['elapsed_ns'],
                            'ttfb_ns': result['ttfb_ns'] if download and valid else None,
                            'status': result['status'], 'response_bytes': result['response_bytes'],
                            'request_bytes': expected['size'] if file is not None else 0,
                            'valid': valid, 'error_kind': result['error_kind'] or (None if valid else 'unexpected_response')}
                    samples.append(item)
                    with lock:
                        raw.write(json.dumps(item, separators=(',', ':')) + '\n')
                    if not valid:
                        failed.set()
                        raise ValueError('request/state validation failed')
                    return value
                barrier.wait()
                deadline = start[0] + int(seconds * 1e9)
                try:
                    while time.monotonic_ns() < deadline and not failed.is_set():
                        if abort_check is not None and abort_check():
                            failed.set()
                            break
                        scenario = config['scenario']
                        if scenario == 'catalog-list':
                            seeds = {i['id']: i for i in config['images']}
                            def listing(value):
                                if not isinstance(value, dict) or not isinstance(value.get('images'), list) or value.get('next'):
                                    return False
                                images = value['images']
                                return (len(images) == len(seeds) and len({i.get('id') for i in images}) == len(seeds)
                                        and [i.get('id') for i in images] == [i['id'] for i in sorted(seeds.values(), key=lambda item: item['name'])]
                                        and all(i.get('id') in seeds and metadata_matches(i, seeds[i['id']]) for i in images))
                            call('catalog-list', 'GET', '/v2/images?' + urlencode({'tag': tag, 'limit': 1000, 'sort': 'name:asc'}), 200, validate=listing)
                        elif scenario == 'catalog-show':
                            seed = config['images'][index % len(config['images'])]
                            call('catalog-show', 'GET', '/v2/images/' + seed['id'], 200,
                                 validate=lambda value: metadata_matches(value, seed), ident=seed['id'])
                        elif scenario == 'nova-mix':
                            # What Nova does to boot from an image: show it,
                            # download it, list active images.
                            seed = config['images'][0]
                            call('show', 'GET', '/v2/images/' + seed['id'], 200,
                                 validate=lambda value: metadata_matches(value, seed), ident=seed['id'])
                            call('download', 'GET', '/v2/images/' + seed['id'] + '/file', 200, download=True, ident=seed['id'])
                            validated_bytes += expected['size']
                            call('list', 'GET', '/v2/images?' + urlencode({'status': 'active', 'visibility': 'private', 'tag': tag}), 200,
                                 validate=lambda value: [i.get('id') for i in value['images']] == [seed['id']])
                        elif scenario.startswith('download-'):
                            seed = config['images'][index % len(config['images'])]
                            call('download', 'GET', '/v2/images/' + seed['id'] + '/file', 200, download=True, ident=seed['id'])
                            validated_bytes += expected['size']
                        else:
                            ident = str(uuid.uuid5(namespace, f'{label}-{index}-{cycles}'))
                            name = f'tt-glance-{label}-w{index}-c{cycles}'
                            base = '/v2/images/' + ident
                            call('fresh-id', 'GET', base, 404, ident=ident)
                            image = {'id': ident, 'name': name, 'owner': owner, 'tags': [tag],
                                     'disk_format': 'qcow2' if scenario == 'upload-qcow2' else 'raw', 'container_format': 'bare', 'visibility': 'private'}
                            # Record the exclusive precondition/intent before POST; a lost reply
                            # can be cleaned only after rechecking these exact ownership fields.
                            with lock:
                                pending[ident] = name
                                intents.write(json.dumps({'id': ident, 'name': name, 'owner': owner, 'tag': tag, 'worker': index, 'cycle': cycles}) + '\n')
                                intents.flush()
                            call('create', 'POST', '/v2/images', 201, body={k: v for k, v in image.items() if k != 'owner'},
                                 validate=lambda value: metadata_matches(value, {**image, 'status': 'queued'}), ident=ident)
                            if scenario == 'catalog-lifecycle':
                                renamed = name + '-updated'
                                call('patch', 'PATCH', base, 200, body=[{'op': 'replace', 'path': '/name', 'value': renamed}],
                                     kind='application/openstack-images-v2.1-json-patch',
                                     validate=lambda value: metadata_matches(value, {**image, 'name': renamed, 'status': 'queued'}), ident=ident)
                                name = renamed
                                with lock:
                                    pending[ident] = name
                                call('verify', 'GET', base, 200, validate=lambda value: owned(value, ident, name) and value.get('status') == 'queued', ident=ident)
                            elif scenario.startswith('upload-'):
                                call('upload', 'PUT', base + '/file', 204, file=upload, kind='application/octet-stream', ident=ident)
                                want = {**image, 'status': 'active', 'size': expected['size'],
                                        'checksum': expected['md5'], 'os_hash_algo': 'sha512', 'os_hash_value': expected['sha512']}
                                if 'virtual_size' in expected:
                                    want['virtual_size'] = expected['virtual_size']
                                call('verify-upload', 'GET', base, 200, validate=lambda value: metadata_matches(value, want), ident=ident)
                            elif scenario.startswith('import-'):
                                # Interoperable import: stage, glance-direct, wait for active.
                                call('stage', 'PUT', base + '/stage', 204, file=upload, kind='application/octet-stream', ident=ident)
                                call('import', 'POST', base + '/import', 202, body={'method': {'name': 'glance-direct'}}, ident=ident)
                                while True:
                                    # Glance Go releases the import lock to 'queued' for an instant
                                    # before it reserves the upload (the original goes importing ->
                                    # active): 'queued' is an in-flight state only while the image
                                    # still holds os_glance_import_task.
                                    state = call('import-wait', 'GET', base, 200,
                                                 validate=lambda value: value.get('status') in ('uploading', 'importing', 'saving', 'active')
                                                 or (value.get('status') == 'queued' and bool(value.get('os_glance_import_task'))), ident=ident)
                                    if state.get('status') == 'active' and not state.get('os_glance_import_task'):
                                        break
                                    time.sleep(config['import_poll_pause_s'])
                                call('verify-import', 'GET', base, 200,
                                     validate=lambda value: metadata_matches(value, {**image, 'status': 'active', 'size': expected['size'],
                                                                                  'checksum': expected['md5'], 'os_hash_algo': 'sha512', 'os_hash_value': expected['sha512']}), ident=ident)
                            else:
                                raise ValueError('unsupported scenario')
                            call('delete', 'DELETE', base, 204, ident=ident)
                            call('verify-absent', 'GET', base, 404, ident=ident)
                            with lock:
                                pending.pop(ident)
                            if scenario.startswith(('upload-', 'import-')):
                                validated_bytes += expected['size']
                        cycles += 1
                except (ValueError, KeyError, TypeError):
                    failed.set()
                finally:
                    work_finished_ns = time.monotonic_ns()
                    client.close()
                return samples, cycles, validated_bytes, work_finished_ns
            with ThreadPoolExecutor(max_workers=concurrency) as pool:
                futures = [pool.submit(worker, i) for i in range(concurrency)]
                barrier.wait()
                results = [f.result() for f in futures]
        samples = [s for rows, _, _, _ in results for s in rows]
        finish = max(end for _, _, _, end in results)
        elapsed = max(seconds, (finish - start[0]) / 1e9)
        latencies = sorted(s['elapsed_ns'] / 1e6 for s in samples if s['valid'])
        correct = sum(s['valid'] for s in samples)
        total_cycles = sum(n for _, n, _, _ in results)
        bytes_validated = sum(n for _, _, n, _ in results)
        return {'start_utc': started_utc[0], 'end_utc': (datetime.datetime.fromisoformat(started_utc[0]) + datetime.timedelta(seconds=elapsed)).isoformat(), 'elapsed_s': elapsed,
                'requests': len(samples), 'correct': correct, 'errors': len(samples) - correct,
                'cycles': total_cycles, 'correct_ops_s': correct / elapsed,
                'complete_cycles_s': total_cycles / elapsed, 'validated_payload_bytes': bytes_validated,
                'validated_payload_mib_s': bytes_validated / elapsed / 1024**2,
                'latency_ms': {k: percentile(latencies, q) for k, q in [('p50', .5), ('p95', .95), ('p99', .99)]},
                'p99_tail_supported': len(latencies) >= 1000, 'failed': failed.is_set()}

    def cleanup():
        client = Client(config['url'], token, timeout=config.get('metadata_timeout_s', 10), binary_timeout=config.get('binary_timeout_s', 120))
        try:
            for ident, name in list(pending.items()):
                result, image = client.request('GET', '/v2/images/' + ident)
                if result['status'] == 404 and result['error_kind'] is None:
                    pending.pop(ident)
                    continue
                if result['status'] != 200 or result['error_kind'] is not None or not owned(image, ident, name):
                    raise ValueError('unproven image ownership; retain state for repair')
                result, _ = client.request('DELETE', '/v2/images/' + ident)
                if result['status'] != 204 or result['error_kind']:
                    raise ValueError('owned image cleanup failed')
                result, _ = client.request('GET', '/v2/images/' + ident)
                if result['status'] != 404 or result['error_kind']:
                    raise ValueError('owned image cleanup not verified')
                pending.pop(ident)
        finally:
            client.close()

    try:
        report['warmup'] = phase(warmup, 'warmup') if warmup else None
        cleanup()
        report['measurement'] = phase(duration, 'measurement') if report['warmup'] is None or not report['warmup']['failed'] else None
    finally:
        try:
            cleanup()
        except ValueError as error:
            cleanup_errors.append(str(error))
        report['cleanup_errors'] = cleanup_errors
        report['pending_images'] = [{'id': ident, 'name': name} for ident, name in pending.items()]
        report['cleanup_ok'] = not pending and not cleanup_errors
        report['external_abort'] = bool(abort_check and abort_check())
        measured = report.get('measurement')
        report['valid'] = bool(measured and not measured['failed'] and measured['cycles'] > 0
                               and measured['errors'] == 0 and report['cleanup_ok'] and not report['external_abort'])
        (output / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
    return report
