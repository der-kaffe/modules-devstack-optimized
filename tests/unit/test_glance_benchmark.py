"""Independent HTTP protocol double: instrument tests, never Glance acceptance."""
import argparse
from contextlib import contextmanager
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
from pathlib import Path
import tempfile
import threading
import time
import unittest
from urllib.parse import parse_qs, urlsplit
import uuid

SPEC = importlib.util.spec_from_file_location('glance_benchmark', Path(__file__).resolve().parents[2] / 'scripts/benchmarks/glance_benchmark.py')
g = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(g)


class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args):
        pass

    def answer(self, status, body=None, raw=None, headers=None):
        data = raw if raw is not None else json.dumps(body).encode() if body is not None else b''
        self.send_response(status)
        self.send_header('Content-Length', str(len(data)))
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def body(self):
        return self.rfile.read(int(self.headers.get('Content-Length', 0)))

    def do_POST(self):
        image = json.loads(self.body())
        with self.server.guard:
            if image['id'] in self.server.images:
                return self.answer(409)
            image['status'] = 'queued'
            self.server.images[image['id']] = image
        self.answer(201, image)

    def do_GET(self):
        parsed = urlsplit(self.path)
        parts = parsed.path.split('/')
        with self.server.guard:
            if parsed.path == '/v2/images':
                q = parse_qs(parsed.query)
                images = list(self.server.images.values())
                for tag in q.get('tag', []):
                    images = [i for i in images if tag in i['tags']]
                if 'name' in q:
                    images = [i for i in images if i['name'] == q['name'][0]]
                images = sorted(images, key=lambda i: i['id'])
                if 'marker' in q:
                    images = [i for i in images if i['id'] > q['marker'][0]]
                images = images[:int(q.get('limit', ['1000'])[0])]
                return self.answer(200, {'images': images})
            image = self.server.images.get(parts[3]) if len(parts) > 3 else None
            if not image:
                return self.answer(404)
            if self.server.wrong_id:
                return self.answer(200, dict(image, id=str(uuid.uuid4())))
            if len(parts) > 4:
                data = self.server.blobs[image['id']]
                if self.server.corrupt:
                    data = b'X' + data[1:]
                rang = self.headers.get('Range')
                if rang:
                    start, end = map(int, rang.removeprefix('bytes=').split('-'))
                    return self.answer(206, raw=data[start:end+1], headers={'Content-Range': f'bytes {start}-{end}/{len(data)}'})
                return self.answer(200, raw=data)
            self.answer(200, image)

    def do_PATCH(self):
        changes = json.loads(self.body())
        with self.server.guard:
            image = self.server.images[self.path.split('/')[3]]
            for change in changes:
                image[change['path'][1:]] = change['value']
            self.answer(200, image)

    def do_PUT(self):
        data = self.body()
        with self.server.guard:
            image = self.server.images[self.path.split('/')[3]]
            self.server.blobs[image['id']] = data
            image.update(status='active', size=len(data), checksum=hashlib.md5(data).hexdigest(),
                         os_hash_algo='sha512', os_hash_value=hashlib.sha512(data).hexdigest())
        self.answer(204)

    def do_DELETE(self):
        with self.server.guard:
            ident = self.path.split('/')[3]
            self.server.images.pop(ident, None)
            self.server.blobs.pop(ident, None)
        self.answer(204)


@contextmanager
def server(handler=Handler):
    class QuietServer(ThreadingHTTPServer):
        def handle_error(self, request, client_address):
            # Expected reset when the client enforces its bounded response cap.
            pass
    s = QuietServer(('127.0.0.1', 0), handler)
    s.daemon_threads = True
    s.images, s.blobs, s.guard = {}, {}, threading.Lock()
    s.wrong_id, s.corrupt = False, False
    t = threading.Thread(target=s.serve_forever, daemon=True)
    t.start()
    try:
        yield s, 'http://127.0.0.1:' + str(s.server_port)
    finally:
        s.shutdown()
        s.server_close()
        t.join()


class GlanceBenchmarkTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.token = self.root / 'token'
        self.token.write_text('SECRET_TEST_TOKEN')
        self.token.chmod(0o600)
        self.payload = self.root / 'payload'
        self.payload.write_bytes(bytes(range(256)) * 1025)

    def args(self, url, scenario='show', concurrency=1):
        return argparse.Namespace(url=url, token_file=self.token, output=self.root / str(uuid.uuid4()),
                                  lock=self.root / 'lock', campaign_id=str(uuid.uuid4()), implementation='go',
                                  scenario=scenario, stage='local-test', concurrency=concurrency,
                                  duration=.08, warmup=.05, timeout=1, max_requests=1000,
                                  max_payload_bytes=1024**2, payload=self.payload, allow_mutations=True)

    def test_all_scenarios_validate_and_clean_own_resources(self):
        with server() as (s, url):
            foreign = str(uuid.uuid4())
            s.images[foreign] = {'id': foreign, 'name': 'foreign', 'tags': []}
            for scenario in g.SCENARIOS:
                with self.subTest(scenario=scenario):
                    a = self.args(url, scenario)
                    self.assertEqual(g.run(a), 0)
                    report = json.loads((a.output / 'report.json').read_text())
                    self.assertTrue(report['manifest']['cleanup_verified'])
                    self.assertFalse(report['official_accepted'])
                    self.assertEqual(report['http']['errors'], 0)
                    self.assertGreater(report['http']['correct'], 0)
                    self.assertEqual(set(s.images), {foreign})
                    self.assertNotIn('SECRET_TEST_TOKEN', ''.join(p.read_text() for p in a.output.iterdir()))
                    self.assertEqual(g.Ledger(a.output, a.campaign_id, url).pending(), [])

    def test_created_tags_have_set_semantics_only(self):
        body = {'id': 'owned', 'tags': ['b', 'a'], 'name': 'unchanged'}
        self.assertTrue(g.created_matches(dict(body, tags=['a', 'b'], status='queued'), body))
        self.assertFalse(g.created_matches(dict(body, tags=['a', 'a'], status='queued'), body))
        self.assertFalse(g.created_matches(dict(body, id=1, status='queued'), body))
        class Reordered(Handler):
            def do_POST(self):
                image = json.loads(self.body())
                image['tags'] = list(reversed(image['tags']))
                image['status'] = 'queued'
                self.server.images[image['id']] = image
                self.answer(201, image)
        with server(Reordered) as (_, url):
            self.assertEqual(g.run(self.args(url)), 0)

    def test_concurrency_and_pagination(self):
        with server() as (s, url):
            a = self.args(url, 'pagination', 4)
            self.assertEqual(g.run(a), 0)
            rows = [json.loads(r) for r in (a.output / 'samples.jsonl').read_text().splitlines()]
            self.assertEqual({r['worker'] for r in rows if r['phase'] == 'measurement'}, {0, 1, 2, 3})
            self.assertEqual(s.images, {})

    def test_corrupt_binary_does_not_pass(self):
        with server() as (s, url):
            s.corrupt = True
            a = self.args(url, 'download')
            self.assertEqual(g.run(a), 1)
            report = json.loads((a.output / 'report.json').read_text())
            self.assertEqual(report['manifest']['state'], 'NEEDS_ATTENTION')
            self.assertTrue(report['manifest']['cleanup_verified'])
            self.assertEqual(s.images, {})

    def test_wrong_state_after_success_not_accepted(self):
        with server() as (s, url):
            s.wrong_id = True
            a = self.args(url)
            self.assertEqual(g.run(a), 1)
            report = json.loads((a.output / 'report.json').read_text())
            self.assertFalse(report['manifest']['cleanup_verified'])
            # Recovery after correcting the double, idempotent twice.
            s.wrong_id = False
            client = g.Client(url, 'token', 1, 1024**2)
            ledger = g.Ledger(a.output, a.campaign_id, url)
            try:
                self.assertTrue(ledger.cleanup(client))
                self.assertTrue(ledger.cleanup(client))
            finally:
                client.close()
            self.assertEqual(s.images, {})

    def test_write_ahead_ledger_and_foreign_ownership(self):
        with server() as (s, url):
            root = self.root / 'ledger'
            root.mkdir()
            ledger = g.Ledger(root, str(uuid.uuid4()), url)
            row = ledger.intent(0, 0)
            s.images[row['id']] = dict(row, name='foreign')
            c = g.Client(url, 'token', 1, 1024)
            try:
                self.assertFalse(ledger.cleanup(c))
                self.assertIn(row['id'], s.images)
                s.images[row['id']] = dict(row)
                self.assertTrue(ledger.cleanup(c))
            finally:
                c.close()
            with self.assertRaises(ValueError):
                g.Ledger(root, str(uuid.uuid4()), url)

    def test_no_effects_for_invalid_inputs_or_official_stage(self):
        a = self.args('http://example.com')
        for key, value in [('allow_mutations', False), ('stage', 'official'), ('concurrency', 17), ('duration', float('nan'))]:
            b = argparse.Namespace(**vars(a))
            setattr(b, key, value)
            with self.assertRaises(ValueError):
                g.run(b)
            self.assertFalse(b.output.exists())
        a.url = 'http://127.0.0.1:9'
        self.token.chmod(0o644)
        with self.assertRaises(ValueError):
            g.run(a)
        self.assertFalse(a.output.exists())

    def test_standalone_deployment_requires_explicit_revision(self):
        self.assertEqual(g.source_revision(self.root, 'a'*40), 'a'*40)
        with self.assertRaises(ValueError):
            g.source_revision(self.root)
        with self.assertRaises(ValueError):
            g.source_revision(self.root, 'short-label')

    def test_lock_and_token_symlink_rejected(self):
        with g.exclusive_lock(self.root / 'lock'):
            with self.assertRaises(BlockingIOError):
                with g.exclusive_lock(self.root / 'lock'):
                    pass
        link = self.root / 'link'
        link.symlink_to(self.token)
        with self.assertRaises(OSError):
            g.private_token(link)

    def test_payload_reproducible_and_never_overwrites(self):
        first, second = self.root / 'one', self.root / 'two'
        a = g.generate_payload(first, g.CHUNK + 37, 20261031)
        b = g.generate_payload(second, g.CHUNK + 37, 20261031)
        self.assertEqual(a, b)
        self.assertEqual(a['bytes'], g.CHUNK + 37)
        with self.assertRaises(FileExistsError):
            g.generate_payload(first, 100, 1)
        self.assertEqual(g.payload_info(first)['sha256'], a['sha256'])

    def test_streaming_hash_and_response_cap(self):
        info = g.payload_info(self.payload)
        self.assertEqual(info['bytes'], self.payload.stat().st_size)
        self.assertEqual(info['sha256'], hashlib.sha256(self.payload.read_bytes()).hexdigest())
        self.assertEqual(info['range_prefix_bytes'], min(self.payload.stat().st_size,65536))
        self.assertEqual(info['range_prefix_sha256'], hashlib.sha256(self.payload.read_bytes()[:65536]).hexdigest())
        class Large(Handler):
            def do_GET(self):
                self.answer(200, raw=b'x' * 4096)
        with server(Large) as (_, url):
            c = g.Client(url, 'token', .2, 1024)
            try:
                sample, _ = c.request('GET', '/v2/images/test/file', binary=True)
                self.assertIsNotNone(sample['error_kind'])
                self.assertEqual(sample['response_bytes'], 1025)
            finally:
                c.close()

    def test_timeout_is_bounded_and_sanitized(self):
        class Slow(Handler):
            def do_GET(self):
                time.sleep(.3)
                self.answer(200, {'secret': 'SECRET_TEST_TOKEN'})
        with server(Slow) as (_, url):
            c = g.Client(url, 'token', .05, 1024)
            try:
                start = time.monotonic()
                sample, value = c.request('GET', '/v2/images')
                self.assertLess(time.monotonic() - start, .25)
                self.assertEqual(sample['error_kind'], 'timeout')
                self.assertIsNone(value)
                self.assertNotIn('SECRET', json.dumps(sample))
            finally:
                c.close()


if __name__ == '__main__':
    unittest.main()
