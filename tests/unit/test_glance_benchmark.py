"""Independent HTTP fixture: full byte validation and bounded, private samples."""
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import sys
import tempfile
import threading
import unittest
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/benchmarks'))
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/integration'))
import glance_http


class GlanceBenchmarkTests(unittest.TestCase):
    def test_runtime_rejects_unsupported_domain_before_any_command(self):
        from types import SimpleNamespace
        from unittest.mock import patch
        import placement_runtime
        with patch.object(placement_runtime, 'command') as mutation:
            with self.assertRaises(ValueError):
                placement_runtime.run(SimpleNamespace(allow_mutations=True, root=Path('/not-used'), domain='unknown'))
            mutation.assert_not_called()

    def test_glance_route_is_only_fixed_image_prefix_and_bounded_port(self):
        import glance_switch
        text = glance_switch.replacement(19293).decode()
        self.assertIn('"/image"', text)
        self.assertNotIn('/placement', text)
        # Same Apache conditions as the original's site file.
        self.assertTrue(text.startswith('KeepAlive Off\nSetEnv proxy-sendchunked 1\n'))
        for port in [True, 80, 65536, '19293']:
            with self.assertRaises(ValueError):
                glance_switch.replacement(port)

    def test_payload_is_reproducible_and_refuses_existing_file(self):
        with tempfile.TemporaryDirectory() as folder:
            a, b = Path(folder) / 'a', Path(folder) / 'b'
            first = glance_http.payload(a, 100_003)
            self.assertEqual(first, glance_http.payload(b, 100_003))
            self.assertEqual(first['sha256'], hashlib.sha256(a.read_bytes()).hexdigest())
            with self.assertRaises(FileExistsError):
                glance_http.payload(a, 100_003)

    def test_streamed_round_trip_and_wrong_200_are_distinguished(self):
        state = {'body': b'', 'corrupt': False}
        class Handler(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'
            def log_message(self, *_):
                pass
            def do_PUT(self):
                state['body'] = self.rfile.read(int(self.headers['Content-Length']))
                self.send_response(204); self.send_header('Content-Length', '0'); self.end_headers()
            def do_GET(self):
                body = state['body'] if not state['corrupt'] else b'x' * len(state['body'])
                self.send_response(200); self.send_header('Content-Length', str(len(body))); self.end_headers(); self.wfile.write(body)
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
        try:
            with tempfile.TemporaryDirectory() as folder:
                p = Path(folder) / 'bytes'
                expected = glance_http.payload(p, 250_003)
                client = glance_http.Client(f'http://127.0.0.1:{server.server_port}/image', 'PRIVATE-TOKEN')
                sample, _ = client.request('PUT', '/v2/images/own/file', upload=p, content_type='application/octet-stream')
                self.assertEqual(sample['status'], 204)
                self.assertIsNone(sample['error_kind'])
                self.assertEqual(hashlib.sha256(state['body']).hexdigest(), expected['sha256'])
                sample, _ = client.request('GET', '/v2/images/own/file', expected_payload=expected)
                self.assertIsNone(sample['error_kind'])
                state['corrupt'] = True
                sample, _ = client.request('GET', '/v2/images/own/file', expected_payload=expected)
                self.assertEqual(sample['error_kind'], 'payload_mismatch')
                self.assertNotIn('PRIVATE-TOKEN', json.dumps(sample))
                client.close()
        finally:
            server.shutdown(); server.server_close(); thread.join()

    def test_metadata_never_coerces_size_or_ignores_state(self):
        expected = {'size': 1024, 'status': 'active', 'tags': ['owned', 'seed']}
        self.assertTrue(glance_http.metadata_matches({'size': 1024, 'status': 'active', 'tags': ['seed', 'owned']}, expected))
        self.assertFalse(glance_http.metadata_matches({'size': 1024.0, 'status': 'active', 'tags': ['seed', 'owned']}, expected))
        self.assertFalse(glance_http.metadata_matches({'size': 1024, 'status': 'saving', 'tags': ['seed', 'owned']}, expected))

    def test_real_cycle_removes_owned_images_and_never_deletes_wrong_owner(self):
        import gzip
        import glance_window
        images, mode, deletions = {}, {'wrong_owner': False}, []
        class Handler(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'
            def log_message(self, *_):
                pass
            def reply(self, status, body=None):
                data = json.dumps(body).encode() if body is not None else b''
                self.send_response(status); self.send_header('Content-Length', str(len(data))); self.end_headers(); self.wfile.write(data)
            def do_GET(self):
                ident = self.path.rsplit('/', 1)[-1]
                self.reply(200, images[ident]) if ident in images else self.reply(404)
            def do_POST(self):
                image = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                image.update(owner='foreign' if mode['wrong_owner'] else 'project', status='queued')
                images[image['id']] = image
                self.reply(201, image)
            def do_PATCH(self):
                patch = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                ident = self.path.rsplit('/', 1)[-1]
                images[ident]['name'] = patch[0]['value']; self.reply(200, images[ident])
            def do_DELETE(self):
                ident = self.path.rsplit('/', 1)[-1]; deletions.append(ident); images.pop(ident); self.reply(204)
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
        try:
            with tempfile.TemporaryDirectory() as folder:
                config = {'url': f'http://127.0.0.1:{server.server_port}/image', 'run_id': 'fixture',
                          'scenario': 'catalog-lifecycle', 'implementation': 'fixture', 'concurrency': 1,
                          'namespace': str(uuid.uuid4()), 'tag': 'owned', 'owner': 'project'}
                out = Path(folder) / 'ok'
                result = glance_window.run(config, 'PRIVATE-TOKEN', out, duration=.05, warmup=0)
                self.assertTrue(result['valid'])
                self.assertFalse(images)
                self.assertGreater(result['measurement']['cycles'], 0)
                with gzip.open(out / 'measurement-samples.jsonl.gz', 'rt') as f:
                    self.assertNotIn('PRIVATE-TOKEN', f.read())
                mode['wrong_owner'] = True
                before = len(deletions)
                config['namespace'] = str(uuid.uuid4())
                result = glance_window.run(config, 'PRIVATE-TOKEN', Path(folder) / 'foreign', duration=.05, warmup=0)
                self.assertFalse(result['valid'])
                self.assertFalse(result['cleanup_ok'])
                self.assertEqual(len(deletions), before)
                self.assertIn('unproven image ownership', result['cleanup_errors'][0])
        finally:
            server.shutdown(); server.server_close(); thread.join()


class GlanceCampaignTests(unittest.TestCase):
    @staticmethod
    def protocol():
        return json.loads((Path(__file__).resolve().parents[2] / 'docs/benchmarks/glance-protocol-compact-v2.json').read_text())

    def test_matrix_is_complete_unique_paired_and_reproducible(self):
        import glance_campaign
        p, namespace = self.protocol(), str(uuid.uuid4())
        plan = glance_campaign.schedule(p, namespace)
        self.assertEqual(len(plan), 54)
        self.assertEqual(plan, glance_campaign.schedule(p, namespace))
        self.assertEqual(len({row['namespace'] for row in plan}), 54)
        self.assertTrue(all(set(row['order']) == {'python', 'go'} for row in plan))
        self.assertEqual(len(glance_campaign.schedule(p, namespace, True)), 18)
        p['scenarios'][-1]['size_bytes'] = 1
        with self.assertRaisesRegex(ValueError, 'payload size'):
            glance_campaign.schedule(p, namespace)

    def test_cleanup_never_adopts_foreign_uuid_owner_tag_or_name(self):
        from glance_lab import owned_descriptor
        namespace = uuid.uuid4()
        values = {'namespace': str(namespace), 'tag': 'own-tag', 'concurrency': 16, 'seed_count': 100}
        image = {'id': str(uuid.uuid5(namespace, 'measurement-15-12')), 'name': 'tt-glance-measurement-w15-c12-updated',
                 'tags': ['own-tag'], 'owner': 'own-project'}
        self.assertTrue(owned_descriptor(values, image, 'own-project'))
        for key, value in [('id', str(uuid.uuid4())), ('name', 'tt-glance-measurement-w16-c12'),
                           ('owner', 'foreign'), ('tags', ['foreign'])]:
            self.assertFalse(owned_descriptor(values, {**image, key: value}, 'own-project'))
        seed = {**image, 'name': 'tt-glance-seed-99', 'id': str(uuid.uuid5(namespace, 'seed-99'))}
        self.assertTrue(owned_descriptor(values, seed, 'own-project'))
        self.assertFalse(owned_descriptor(values, {**seed, 'name': 'tt-glance-seed-100', 'id': str(uuid.uuid5(namespace, 'seed-100'))}, 'own-project'))

    def test_analyzer_reconstructs_cycles_and_pilot_is_never_official_acceptance(self):
        """Synthetic resource fixture only: never represented as lab evidence."""
        import datetime
        import gzip
        import analyze_glance
        import glance_campaign
        from glance_campaign import save
        p, campaign = self.protocol(), str(uuid.uuid4())
        plan = glance_campaign.schedule(p, campaign, True)
        timestamp = datetime.datetime(2026, 10, 31, tzinfo=datetime.timezone.utc)
        t = int(timestamp.timestamp() * 1e9)
        def utc(seconds):
            return (timestamp + datetime.timedelta(seconds=seconds)).isoformat()
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); (root / 'lab').mkdir()
            manifest = {'campaign_id': campaign, 'protocol': p, 'stage': 'pilot', 'sequence': plan, 'valid': True,
                        'cleanup': True, 'git_base': 'fixture', 'protocol_sha256': 'fixture', 'runs': []}
            for pair in plan:
                for impl in ('python', 'go'):
                    ident = pair['pair_id'] + '-' + impl
                    folder = root / ident; folder.mkdir()
                    ops = (['fresh-id', 'create', 'patch', 'verify', 'delete', 'verify-absent'] if pair['scenario'] == 'catalog-lifecycle'
                           else ['fresh-id', 'create', 'upload', 'verify-upload', 'delete', 'verify-absent'] if pair['scenario'].startswith('upload-')
                           else ['download'] if pair['scenario'].startswith('download-') else [pair['scenario']])
                    size = pair['size_bytes'] or 0
                    samples = [{'run_id': ident, 'worker': 0, 'cycle': 0, 'operation': op, 'offset_ns': (index+1)*100_000_000,
                                'elapsed_ns': 10_000_000, 'ttfb_ns': 1_000_000 if op == 'download' else None,
                                'valid': True, 'error_kind': None, 'request_bytes': size if op == 'upload' else 0,
                                'response_bytes': size if op == 'download' else 0} for index, op in enumerate(ops)]
                    with gzip.open(folder / 'measurement-samples.jsonl.gz', 'wt') as f:
                        for row in samples:
                            f.write(json.dumps(row)+'\n')
                    report = {'run_id': ident, 'scenario': pair['scenario'], 'implementation': impl, 'concurrency': pair['concurrency'],
                              'valid': True, 'cleanup_ok': True, 'clock_valid': True, 'clock_drift_ns': 0,
                              'clocks': {key: {'best': {'remote_minus_host_ns': 0, 'rtt_ns': 1000}} for key in ('before','after')},
                              'warmup': {'start_utc': utc(-1)},
                              'measurement': {'start_utc': utc(0), 'end_utc': utc(4), 'elapsed_s': 4, 'requests': len(ops),
                                              'correct': len(ops), 'cycles': 1, 'errors': 0, 'failed': False,
                                              'correct_ops_s': len(ops)/4, 'complete_cycles_s': .25,
                                              'validated_payload_bytes': size, 'validated_payload_mib_s': size/4/1024**2,
                                              'latency_ms': dict.fromkeys(['p50','p95','p99'],10.0)}}
                    save(folder / 'report.json', report)
                    resources = root / 'lab' / (ident+'-resources'); resources.mkdir()
                    save(resources / 'collector.json', {'clock_ticks_s': 100, 'self_terminated_at_limit': False})
                    with (resources / 'resources.jsonl').open('w') as f:
                        for sec in range(-5, 7):
                            row = {'utc_ns': t+sec*10**9, 'read_finished_utc_ns': t+sec*10**9+1000000, 'collector_cpu_s': (sec+5)*.001,
                                   'groups': {impl: {'error': None, 'processes': [{'pid': 123,'start_ticks': 2,'cpu_ticks': (sec+5)*100,
                                                                                'pss_bytes': 1024**2,'rss_bytes': 2*1024**2}]}}}
                            f.write(json.dumps(row)+'\n')
                    manifest['runs'].append({'run_id': ident, 'valid': True})
            save(root / 'manifest.json', manifest)
            result = analyze_glance.analyze(root)
            self.assertTrue(result['pilot_validated'], result['errors'])
            self.assertFalse(result['accepted'])
            self.assertEqual(result['runs_observed'],36)
            with self.assertRaises(ValueError):
                analyze_glance.publish(result, root / 'must-not-publish.md')
            first = root / manifest['runs'][0]['run_id'] / 'report.json'
            report = json.loads(first.read_text()); report['measurement']['requests'] += 1; save(first, report)
            result = analyze_glance.analyze(root)
            self.assertFalse(result['complete_valid'])
            self.assertIn('counts', result['errors'][0]['error'])

    def test_catalog_same_ids_wrong_order_is_rejected_over_real_http(self):
        import glance_window
        namespace = str(uuid.uuid4())
        images = [{'id': str(uuid.uuid4()), 'name': name, 'owner': 'p', 'tags': ['t'], 'status': 'queued'} for name in ['a','b']]
        mode = {'reverse': False}
        class Handler(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'
            def log_message(self, *_):
                pass
            def do_GET(self):
                body = json.dumps({'images': list(reversed(images)) if mode['reverse'] else images}).encode()
                self.send_response(200); self.send_header('Content-Length', str(len(body))); self.end_headers(); self.wfile.write(body)
        server = ThreadingHTTPServer(('127.0.0.1',0), Handler)
        thread = threading.Thread(target=server.serve_forever,daemon=True); thread.start()
        try:
            with tempfile.TemporaryDirectory() as tmp:
                config = {'run_id':'catalog-fixture','scenario':'catalog-list','implementation':'fixture','concurrency':1,
                          'namespace':namespace,'tag':'t','owner':'p','images':images,'url':f'http://127.0.0.1:{server.server_port}/image'}
                result = glance_window.run(config,'private',Path(tmp)/'correct',duration=.05,warmup=0)
                self.assertTrue(result['valid'])
                mode['reverse'] = True
                result = glance_window.run(config,'private',Path(tmp)/'reversed',duration=.05,warmup=0)
                self.assertFalse(result['valid'])
                self.assertEqual(result['measurement']['errors'],1)
        finally:
            server.shutdown(); server.server_close(); thread.join()

    def test_noop_policy_only_changes_driver_and_requires_new_declared_protocol(self):
        from glance_notifications import noop_config
        from glance_campaign import validate_protocol
        original='[DEFAULT]\npassword = PRIVATE\n[oslo_messaging_notifications]\ndriver = messagingv2\n[other]\nvalue = keep\n'
        changed=noop_config(original)
        self.assertEqual(changed,original.replace('driver = messagingv2','driver = noop'))
        with self.assertRaises(ValueError):noop_config('[DEFAULT]\nother = keep\n')
        p=self.protocol();p['protocol']='glance-compact-v3'
        with self.assertRaises(ValueError):validate_protocol(p)
        p['notification_policy']='noop';p['maximum_open_fd_growth_per_window']=256;self.assertEqual(validate_protocol(p),108)

    def test_v4_repeats_v3_with_go_notifications_only(self):
        import json
        from pathlib import Path
        from glance_campaign import validate_protocol
        root = Path(__file__).resolve().parents[2] / 'docs/benchmarks'
        v3 = json.loads((root / 'glance-protocol-compact-v3.json').read_text())
        v4 = json.loads((root / 'glance-protocol-compact-v4.json').read_text())
        self.assertEqual(validate_protocol(v4), 108)
        same = ('scenarios', 'concurrency', 'repetitions', 'warmup_s', 'measurement_s', 'pilot', 'payload_sizes_bytes',
                'order_seed', 'catalog_seed_count', 'notification_policy', 'maximum_open_fd_growth_per_window',
                'transfer_admission_limit_go', 'host_free_minimum_bytes')
        self.assertEqual({k: v3[k] for k in same}, {k: v4[k] for k in same})
        self.assertEqual((v4['notification_policy'], v4['go_notification_policy']), ('noop', 'original'))
        for bad in ({**v4, 'go_notification_policy': 'noop'}, {**v4, 'notification_policy': 'original'},
                    {**v3, 'go_notification_policy': 'original'}):
            with self.assertRaises(ValueError):
                validate_protocol(bad)

    def test_remote_lab_receives_both_notification_policies(self):
        import tempfile
        from pathlib import Path
        from types import SimpleNamespace
        from unittest import mock
        import placement_campaign
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(placement_campaign.subprocess, 'Popen') as popen:
            args = SimpleNamespace(known_hosts=Path(tmp) / 'k', ssh_key=Path(tmp) / 'id', host='stack@192.168.122.10',
                                   remote_code=Path('/home/stack/code'), remote_work=Path('/home/stack/w'),
                                   remote_output=Path('/home/stack/o'), private_root=Path(tmp), domain='glance',
                                   notification_policy='noop', go_notification_policy='original')
            remote = placement_campaign.Remote(args)
            remote.stderr.close()  # Popen is mocked; close the private log it opened
            command = popen.call_args[0][0][-1]
            self.assertIn('--notification-policy noop', command)
            self.assertIn('--go-notifications original', command)
            args.go_notification_policy = 'everything'
            (Path(tmp) / 'remote-stderr.log').unlink()
            with self.assertRaises(ValueError):
                placement_campaign.Remote(args)

    def test_descriptor_guard_rejects_unobserved_or_leaking_counts(self):
        from analyze_glance import validate_fd_stability
        validate_fd_stability({'open_fds_start':40,'open_fds_end':44,'open_fds_peak':64},256)
        with self.assertRaises(ValueError):validate_fd_stability({},256)
        with self.assertRaises(ValueError):validate_fd_stability({'open_fds_start':40,'open_fds_end':1540,'open_fds_peak':1540},256)


class EffectiveGoSettingsTests(unittest.TestCase):
    def test_reads_last_effective_settings_line_from_the_journal(self):
        from glance_lab import effective_go_settings
        journal = "\n".join([
            'not json',
            '{"time":"t","level":"INFO","msg":"effective settings","sql_pool_open":4}',
            '{"time":"t","level":"INFO","msg":"image API listening"}',
            '{"time":"t","level":"INFO","msg":"effective settings","sql_pool_open":55,"sql_pool_idle":16,"token_cache_s":300}'])
        self.assertEqual(effective_go_settings(journal), {'sql_pool_open': 55, 'sql_pool_idle': 16, 'token_cache_s': 300})

    def test_missing_settings_line_fails_closed(self):
        from glance_lab import effective_go_settings
        with self.assertRaises(Exception):
            effective_go_settings('{"msg":"image API listening"}')


class MemoryExtrasTests(unittest.TestCase):
    def test_rss_and_vm_ram_come_from_the_window_samples(self):
        from analyze_glance import memory_extras
        row = lambda t, rss, avail: {'read_finished_utc_ns': t, 'vm_mem_available_kib': avail,
                                     'groups': {'go': {'processes': [{'rss_bytes': rss}]}}}
        records = [row(1, 10, 1000), row(2, 10, 1000), row(5, 30, 900), row(6, 50, 800), row(9, 99, 100)]
        out = memory_extras(records, 'go', 4, 7, 3)
        self.assertEqual((out['rss_mean_bytes'], out['rss_peak_observed_bytes']), (40, 50))
        self.assertEqual(out['vm_ram_used_extra_peak_bytes'], (1000 - 800) * 1024)


class CgroupMemoryTests(unittest.TestCase):
    def test_reader_and_window_summary(self):
        import json, tempfile
        from pathlib import Path
        import glance_memory
        from analyze_glance import cgroup_memory
        with tempfile.TemporaryDirectory() as tmp:
            g = Path(tmp) / 'system.slice' / 'unit'
            g.mkdir(parents=True)
            (g / 'memory.current').write_text('500\n')
            (g / 'memory.stat').write_text('anon 300\nfile 150\nkernel 40\nsock 0\nshmem 0\nother 9\n')
            self.assertEqual(glance_memory.read_group('/system.slice/unit', root=Path(tmp)),
                             {'current': 500, 'anon': 300, 'file': 150, 'kernel': 40, 'sock': 0, 'shmem': 0})
            rows = Path(tmp) / 'm.jsonl'
            rows.write_text('\n'.join(json.dumps({'utc_ns': t, 'groups': {'go': {'anon': a, 'file': 10}}})
                                      for t, a in [(1, 999), (5, 100), (6, 300), (7, 200), (20, 999)]))
            out = cgroup_memory(rows, 'go', 4, 10)
            self.assertEqual((out['cgroup_anon_mean_bytes'], out['cgroup_anon_peak_bytes'], out['cgroup_samples']), (200, 300, 3))
            self.assertEqual(cgroup_memory(Path(tmp) / 'missing', 'go', 0, 1), {})


class GlanceV7Tests(unittest.TestCase):
    def test_v7_matrix_is_placement_design(self):
        import glance_campaign
        p = json.loads((Path(__file__).resolve().parents[2] / 'docs/benchmarks/glance-protocol-v7.json').read_text())
        self.assertEqual(glance_campaign.validate_protocol(p), 144)
        plan = glance_campaign.schedule(p, str(uuid.uuid4()))
        self.assertEqual(len(plan), 72)
        self.assertEqual({row['concurrency'] for row in plan}, {1, 16})
        self.assertEqual({row['repetition'] for row in plan}, {1, 2, 3})
        p['concurrency'] = [1, 4, 8, 16]
        with self.assertRaisesRegex(ValueError, 'v7'):
            glance_campaign.validate_protocol(p)

    def test_import_and_nova_mix_cycles(self):
        import hashlib
        import glance_window
        data = b'x' * 4096
        digest = {'size': len(data), 'md5': hashlib.md5(data).hexdigest(), 'sha512': hashlib.sha512(data).hexdigest(), 'sha256': hashlib.sha256(data).hexdigest()}
        images, polls, quirk = {}, {}, {'queued': True, 'lock': True}
        seed = {'id': str(uuid.uuid4()), 'name': 'seed', 'tags': ['owned'], 'owner': 'project', 'status': 'active',
                'disk_format': 'raw', 'container_format': 'bare', 'visibility': 'private'}
        class Handler(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'
            def log_message(self, *_):
                pass
            def reply(self, status, body=None, raw=None):
                out = raw if raw is not None else (json.dumps(body).encode() if body is not None else b'')
                self.send_response(status); self.send_header('Content-Length', str(len(out))); self.end_headers(); self.wfile.write(out)
            def body(self):
                return self.rfile.read(int(self.headers.get('Content-Length', 0)))
            def do_GET(self):
                path = self.path.split('?')[0]
                if path.endswith('/v2/images'):
                    return self.reply(200, {'images': [seed]})
                if path.endswith('/file'):
                    return self.reply(200, raw=data)
                ident = path.rsplit('/', 1)[-1]
                if ident == seed['id']:
                    return self.reply(200, seed)
                if ident not in images:
                    return self.reply(404)
                image = images[ident]
                if image['status'] == 'importing':
                    polls[ident] = polls.get(ident, 0) + 1
                    if polls[ident] == 1 and quirk['queued']:
                        # Go's instant between releasing the import lock and reserving the upload
                        return self.reply(200, {**image, 'status': 'queued', 'os_glance_import_task': 'task'} if quirk['lock'] else {**image, 'status': 'queued'})
                    if polls[ident] >= 2:
                        image.update(status='active', size=len(data), checksum=digest['md5'], os_hash_algo='sha512', os_hash_value=digest['sha512'])
                self.reply(200, image)
            def do_PUT(self):
                self.body(); ident = self.path.split('/')[-2]; images[ident]['status'] = 'uploading'; self.reply(204)
            def do_POST(self):
                payload = json.loads(self.body())
                if self.path.endswith('/import'):
                    images[self.path.split('/')[-2]]['status'] = 'importing'; return self.reply(202, {})
                payload.update(owner='project', status='queued'); images[payload['id']] = payload; self.reply(201, payload)
            def do_DELETE(self):
                images.pop(self.path.rsplit('/', 1)[-1]); self.reply(204)
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
        try:
            with tempfile.TemporaryDirectory() as folder:
                f = Path(folder) / 'payload'; f.write_bytes(data)
                base = {'url': f'http://127.0.0.1:{server.server_port}/image', 'implementation': 'fixture', 'concurrency': 1,
                        'tag': 'owned', 'owner': 'project', 'payload': digest, 'payload_file': str(f), 'import_poll_pause_s': 0.001}
                for scenario in ('import-direct-16m', 'nova-mix'):
                    config = {**base, 'run_id': 'fixture-' + scenario, 'scenario': scenario, 'namespace': str(uuid.uuid4()), 'images': [seed]}
                    result = glance_window.run(config, 'PRIVATE-TOKEN', Path(folder) / scenario, duration=.05, warmup=0)
                    self.assertTrue(result['valid'], scenario)
                    self.assertGreater(result['measurement']['cycles'], 0)
                self.assertFalse(images)
                # 'queued' without the import lock is not an in-flight state
                quirk['lock'] = False
                config = {**base, 'run_id': 'fixture-nolock', 'scenario': 'import-direct-16m', 'namespace': str(uuid.uuid4()), 'images': [seed]}
                result = glance_window.run(config, 'PRIVATE-TOKEN', Path(folder) / 'nolock', duration=.05, warmup=0)
                self.assertFalse(result['valid'])
        finally:
            server.shutdown(); server.server_close(); thread.join()
