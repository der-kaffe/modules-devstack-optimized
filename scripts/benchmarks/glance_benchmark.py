#!/usr/bin/env python3
"""Bounded RF10 pilot instrument. No deployment, Git publication or official acceptance.

One implementation at a time, real token in private file, write-ahead ownership
ledger and independent streaming hashes. Recovery uses the same endpoint + ledger.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
import datetime
import fcntl
import hashlib
import http.client
import json
import math
import os
import re
from pathlib import Path
import signal
import shutil
import socket
import subprocess
import ssl
import stat
import sys
import threading
import time
from urllib.parse import urlencode, urlsplit
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parent))
from placement_http import summarize

CHUNK = 256 * 1024
JSON_LIMIT = 2 * 1024 * 1024
SCENARIOS = ('create', 'list', 'show', 'patch', 'delete', 'pagination', 'filter',
             'metadata-mix', 'crud', 'upload', 'download', 'range', 'binary-mix')
BINARY = ('upload', 'download', 'range', 'binary-mix')



def source_revision(root, explicit=None):
    if explicit is not None:
        if not re.fullmatch(r'[0-9a-f]{40}', explicit):
            raise ValueError('explicit full code SHA required for deployed artifact')
        return explicit
    result = subprocess.run(['git', 'rev-parse', 'HEAD'], cwd=root, stdout=subprocess.PIPE,
                            stderr=subprocess.DEVNULL, text=True, timeout=5)
    value = result.stdout.strip()
    if result.returncode or not re.fullmatch(r'[0-9a-f]{40}', value):
        raise ValueError('standalone deployment requires --code-sha and instrument hashes')
    return value


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def save(path, value):
    tmp = path.with_name(path.name + '.tmp')
    with tmp.open('w') as stream:
        json.dump(value, stream, indent=2, allow_nan=False)
        stream.write('\n')
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(tmp, path)


def private_token(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as stream:
        mode = os.fstat(stream.fileno())
        if not stat.S_ISREG(mode.st_mode) or mode.st_uid != os.getuid() or mode.st_mode & 0o077:
            raise ValueError('token must be an owned private regular file')
        raw = stream.read(65537)
    if len(raw) > 65536:
        raise ValueError('oversized token file')
    token = raw.decode('ascii').strip()
    if not token or any(ord(c) <= 32 or ord(c) >= 127 for c in token):
        raise ValueError('invalid token encoding')
    return token


def canonical_path(path):
    path = Path(path)
    if not path.is_absolute() or path != path.resolve():
        raise ValueError('canonical absolute path required')
    return path


@contextmanager
def exclusive_lock(path):
    path = canonical_path(path)
    fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        value = os.fstat(fd)
        if not stat.S_ISREG(value.st_mode) or value.st_uid != os.getuid() or value.st_mode & 0o077:
            raise ValueError('unsafe lock file')
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        os.close(fd)


def payload_info(path):
    """Independent complete-file digests in bounded memory, outside load window."""
    path = canonical_path(path)
    hashes = {k: hashlib.new(k) for k in ('sha256', 'sha512', 'md5')}
    prefix = hashlib.sha256()
    prefix_left = 65536
    size = 0
    with path.open('rb') as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise ValueError('regular payload required')
        while True:
            block = stream.read(CHUNK)
            if not block:
                break
            size += len(block)
            prefix.update(block[:prefix_left])
            prefix_left = max(0,prefix_left-len(block))
            for h in hashes.values():
                h.update(block)
    if size < 1:
        raise ValueError('nonempty payload required')
    return {'bytes': size, 'range_prefix_bytes':min(size,65536),'range_prefix_sha256':prefix.hexdigest(),
            **{k: h.hexdigest() for k, h in hashes.items()}}



def generate_payload(path, size, seed):
    path = canonical_path(path)
    if type(size) is not int or not 1 <= size <= 1024 ** 3 or type(seed) is not int or not 0 <= seed < 2**63:
        raise ValueError('bounded payload size and nonnegative seed required')
    if shutil.disk_usage(path.parent).free < size * 2 + 256 * 1024**2:
        raise ValueError('insufficient disk reserve')
    # Counter-domain-separated SHAKE256 blocks; no file-sized allocation.
    with path.open('xb') as stream:
        written, counter = 0, 0
        while written < size:
            amount = min(CHUNK, size - written)
            block = hashlib.shake_256(f'tt-glance-payload-v1:{seed}:{counter}'.encode('ascii')).digest(amount)
            stream.write(block)
            written += amount
            counter += 1
        stream.flush()
        os.fsync(stream.fileno())
    return dict(payload_info(path), seed=seed, generator='SHAKE256 domain-separated blocks v1', block_bytes=CHUNK)


class Client:
    def __init__(self, url, token, timeout, binary_limit):
        self.url = url
        self.endpoint = urlsplit(url)
        e = self.endpoint
        if (e.scheme not in ('http', 'https') or not e.hostname or e.username or e.password
                or e.query or e.fragment or e.path not in ('', '/', '/image', '/image/')):
            raise ValueError('base endpoint must be unversioned Image API without credentials')
        if e.hostname not in ('127.0.0.1', 'localhost', '::1', '192.168.122.10'):
            raise ValueError('only identified local laboratory endpoints allowed')
        if not math.isfinite(timeout) or not .05 <= timeout <= 600:
            raise ValueError('bounded timeout required')
        self.token, self.timeout, self.binary_limit = token, timeout, binary_limit
        self.connection = None

    def close(self):
        if self.connection:
            self.connection.close()
        self.connection = None

    def request(self, method, path, body=None, upload=None, binary=False, headers=None):
        if not path.startswith('/v2/images') or any(ord(c) <= 32 or ord(c) >= 127 for c in path):
            raise ValueError('invalid image request path')
        begin = time.monotonic_ns()
        deadline = time.monotonic() + self.timeout
        sample = {'begin_ns': begin, 'status': None, 'response_bytes': 0,
                  'request_bytes': 0, 'error_kind': None}
        value, digests = None, {k: hashlib.new(k) for k in ('sha256', 'sha512', 'md5')}
        finished = None

        def budget():
            left = deadline - time.monotonic()
            if left <= 0:
                raise TimeoutError()
            self.connection.timeout = left
            if self.connection.sock:
                self.connection.sock.settimeout(left)

        def abort_socket():
            connection = self.connection
            if connection and connection.sock:
                try:
                    connection.sock.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass

        timer = threading.Timer(self.timeout, abort_socket)
        timer.daemon = True
        timer.start()
        try:
            if self.connection is None:
                cls = http.client.HTTPSConnection if self.endpoint.scheme == 'https' else http.client.HTTPConnection
                kwargs = {'context': ssl.create_default_context()} if cls is http.client.HTTPSConnection else {}
                self.connection = cls(self.endpoint.hostname, self.endpoint.port, timeout=self.timeout, **kwargs)
            data = b'' if body is None else json.dumps(body, separators=(',', ':')).encode()
            supplied = {'X-Auth-Token': self.token, 'Accept': 'application/octet-stream' if binary else 'application/json',
                        'Content-Type': 'application/json'}
            supplied.update(headers or {})
            with open(upload, 'rb') if upload else _empty_context() as source:
                size = os.fstat(source.fileno()).st_size if source else len(data)
                if source and size > self.binary_limit:
                    raise ValueError('upload_limit')
                if source:
                    supplied['Content-Type'] = 'application/octet-stream'
                supplied['Content-Length'] = str(size)
                budget()
                self.connection.putrequest(method, self.endpoint.path.rstrip('/') + path)
                for key, val in supplied.items():
                    self.connection.putheader(key, val)
                self.connection.endheaders()
                if source:
                    while True:
                        block = source.read(CHUNK)
                        if not block:
                            break
                        if sample['request_bytes'] + len(block) > size:
                            raise ValueError('payload_changed')
                        budget()
                        self.connection.send(block)
                        sample['request_bytes'] += len(block)
                elif data:
                    budget()
                    self.connection.send(data)
                    sample['request_bytes'] = len(data)
            budget()
            response = self.connection.getresponse()
            sample['status'] = response.status
            expected_length = response.length
            content_range = response.getheader('Content-Range') if binary else None
            if content_range is not None and (len(content_range) > 96 or not content_range.startswith('bytes ')
                                              or any(c not in 'bytes 0123456789-/*' for c in content_range)):
                raise ValueError('invalid_range_header')
            sample['content_range'] = content_range
            limit = self.binary_limit if binary else JSON_LIMIT
            raw = bytearray()
            while True:
                budget()
                block = response.read1(min(CHUNK, limit + 1 - sample['response_bytes']))
                if not block:
                    break
                sample['response_bytes'] += len(block)
                if sample['response_bytes'] > limit:
                    raise ValueError('response_limit')
                if binary:
                    for h in digests.values():
                        h.update(block)
                else:
                    raw.extend(block)
            finished = time.monotonic_ns()
            timer.cancel()
            timer.join()
            if expected_length is not None and sample['response_bytes'] != expected_length:
                raise ValueError('incomplete_response')
            if not binary and raw:
                value = json.loads(raw)
            if binary:
                sample['digests'] = {k: h.hexdigest() for k, h in digests.items()}
        except (OSError, http.client.HTTPException, ValueError, UnicodeError) as error:
            sample['error_kind'] = 'timeout' if isinstance(error, TimeoutError) or time.monotonic() >= deadline else 'transport_or_invalid_response'
            self.close()
        finally:
            timer.cancel()
            timer.join()
        sample['finish_ns'] = finished if finished is not None else time.monotonic_ns()
        sample['elapsed_ns'] = sample['finish_ns'] - begin
        return sample, value


@contextmanager
def _empty_context():
    yield None


class Ledger:
    def __init__(self, root, campaign, endpoint):
        self.root, self.campaign, self.endpoint = root, str(uuid.UUID(campaign)), endpoint
        self.path = root / 'ledger.jsonl'
        self.lock = threading.Lock()
        self.records = []
        if self.path.exists():
            for line in self.path.read_text().splitlines():
                row = json.loads(line)
                if row['campaign'] != self.campaign or row['endpoint'] != endpoint:
                    raise ValueError('ledger ownership or endpoint mismatch')
                self.records.append(row)

    def append(self, row):
        row = dict(row, campaign=self.campaign, endpoint=self.endpoint, utc=utc())
        with self.lock:
            with self.path.open('a') as stream:
                stream.write(json.dumps(row, separators=(',', ':')) + '\n')
                stream.flush()
                os.fsync(stream.fileno())
            self.records.append(row)

    def intent(self, worker, sequence, extra_tags=()):
        ident = str(uuid.uuid5(uuid.UUID(self.campaign), f'{worker}:{sequence}'))
        name = f'tt-glance-{self.campaign}-{worker}-{sequence}'
        tags = [f'tt-campaign-{self.campaign}', *extra_tags]
        row = {'event': 'intent', 'id': ident, 'name': name, 'tags': tags}
        self.append(row)
        return row

    def pending(self):
        intents, deleted = {}, set()
        for row in self.records:
            if row['event'] == 'intent':
                intents[row['id']] = row
            elif row['event'] == 'absent':
                deleted.add(row['id'])
        return [r for ident, r in intents.items() if ident not in deleted]

    def cleanup(self, client):
        results = []
        for row in self.pending():
            ident = row['id']
            got, image = client.request('GET', '/v2/images/' + ident)
            if got['status'] == 404 and not got['error_kind']:
                self.append({'event': 'absent', 'id': ident})
                results.append(True)
                continue
            if (got['error_kind'] or got['status'] != 200 or not isinstance(image, dict)
                    or image.get('id') != ident or image.get('name') != row['name']
                    or not set(row['tags']).issubset(image.get('tags', []))):
                results.append(False)
                continue
            deleted, _ = client.request('DELETE', '/v2/images/' + ident)
            check, _ = client.request('GET', '/v2/images/' + ident)
            absent = deleted['status'] == 204 and not deleted['error_kind'] and check['status'] == 404 and not check['error_kind']
            if absent:
                self.append({'event': 'absent', 'id': ident})
            results.append(absent)
        return all(results)



def created_matches(value, body):
    if not isinstance(value, dict) or value.get('status') != 'queued':
        return False
    for key, expected in body.items():
        actual = value.get(key)
        if key == 'tags':
            if not isinstance(actual, list) or any(not isinstance(x, str) for x in actual):
                return False
            if sorted(actual) != sorted(expected):
                return False
        elif actual != expected:
            return False
    return True


class Worker:
    def __init__(self, index, args, ledger, record, stop):
        self.index, self.args, self.ledger, self.record, self.stop = index, args, ledger, record, stop
        self.client = Client(args.url, args.token, args.timeout, args.max_payload_bytes)
        self.sequence = 0
        self.seeds = []
        self.info = args.payload_info

    def call(self, operation, method, path, expected, validator=lambda v, s: True, measured=False, **kwargs):
        sample, value = self.client.request(method, path, **kwargs)
        try:
            sample['valid'] = (sample['error_kind'] is None and sample['status'] == expected
                               and bool(validator(value, sample)))
        except (ValueError, TypeError, KeyError, AttributeError):
            sample['valid'] = False
        if not sample['valid'] and not sample['error_kind']:
            sample['error_kind'] = 'unexpected_status_or_state'
        sample.update(operation=operation, worker=self.index)
        self.record(sample, measured)
        if not sample['valid']:
            self.stop.set()
            raise RuntimeError('functional gate or request validation failed')
        return value

    def create(self, measured=False, seed=False):
        row = self.ledger.intent(self.index, self.sequence,
                                 [f'tt-seed-{self.args.campaign_id}-{self.index}'] if seed else [])
        self.sequence += 1
        body = {k: row[k] for k in ('id', 'name', 'tags')}
        body.update(visibility='private', disk_format='raw', container_format='bare')
        self.call('create', 'POST', '/v2/images', 201,
                  lambda v, s: created_matches(v, body),
                  measured, body=body)
        return row

    def image(self, row, measured=False):
        return self.call('show', 'GET', '/v2/images/' + row['id'], 200,
                         lambda v, s: v.get('id') == row['id'] and v.get('name') == row['name']
                         and set(row['tags']).issubset(v.get('tags', [])) and v.get('visibility') == 'private', measured)

    def remove(self, row, measured=False):
        self.image(row)  # ownership check outside delete latency
        self.call('delete', 'DELETE', '/v2/images/' + row['id'], 204, measured=measured)
        self.call('absence', 'GET', '/v2/images/' + row['id'], 404)
        self.ledger.append({'event': 'absent', 'id': row['id']})

    def upload(self, row, measured=False):
        self.call('upload', 'PUT', '/v2/images/' + row['id'] + '/file', 204,
                  measured=measured, upload=self.args.payload)
        v = self.image(row)
        if (v.get('status') != 'active' or v.get('size') != self.info['bytes']
                or v.get('checksum') != self.info['md5'] or v.get('os_hash_algo') != 'sha512'
                or v.get('os_hash_value') != self.info['sha512']):
            self.stop.set()
            raise RuntimeError('upload metadata integrity failed')

    def download(self, row, measured=False, ranged=False):
        if ranged:
            length = min(self.info['bytes'], 65536)
            with self.args.payload.open('rb') as stream:
                digest = hashlib.sha256(stream.read(length)).hexdigest()
            self.call('range', 'GET', '/v2/images/' + row['id'] + '/file', 206,
                      lambda v, s: s['response_bytes'] == length and s['digests']['sha256'] == digest
                      and s['content_range'] == f'bytes 0-{length - 1}/{self.info["bytes"]}',
                      measured, binary=True, headers={'Range': f'bytes=0-{length - 1}'})
        else:
            self.call('download', 'GET', '/v2/images/' + row['id'] + '/file', 200,
                      lambda v, s: s['response_bytes'] == self.info['bytes']
                      and s['digests']['sha256'] == self.info['sha256']
                      and s['digests']['sha512'] == self.info['sha512'], measured, binary=True)

    def listing(self, measured=False, paged=False, filtered=False):
        ids = {r['id'] for r in self.seeds}
        # Exact IDs, names, tags and marker ordering: no foreign resources adopted.
        query = {'tag': f'tt-seed-{self.args.campaign_id}-{self.index}', 'limit': 1 if paged else 1000, 'sort': 'id:asc'}
        if filtered:
            query['name'] = self.seeds[0]['name']
            ids = {self.seeds[0]['id']}
        seen, marker = set(), None
        expected = {row['id']: row for row in self.seeds}
        for _ in range(self.args.concurrency * 2 + 2):
            if marker:
                query['marker'] = marker
            value = self.call('pagination' if paged else 'filter' if filtered else 'list',
                              'GET', '/v2/images?' + urlencode(query), 200,
                              lambda v, s: isinstance(v.get('images'), list) and all(
                                  isinstance(image, dict) and image.get('id') in expected
                                  and image.get('name') == expected[image['id']]['name']
                                  and set(expected[image['id']]['tags']).issubset(image.get('tags', []))
                                  for image in v['images']), measured)
            images = value['images']
            current = [v.get('id') for v in images]
            if any(not isinstance(i, str) for i in current) or current != sorted(set(current)) or seen.intersection(current):
                raise RuntimeError('invalid pagination identity/order')
            seen.update(current)
            if not paged or not images:
                break
            marker = current[-1]
        else:
            raise RuntimeError('pagination limit exceeded')
        # Every listing is restricted to this worker's stable seeded catalog.
        if seen != ids:
            raise RuntimeError('missing seeded images')

    def prepare(self):
        self.seeds = [self.create(seed=True), self.create(seed=True)]
        if self.args.scenario in BINARY:
            self.upload(self.seeds[0])
            self.download(self.seeds[0])
        self.image(self.seeds[0])
        self.listing()

    def cycle(self, measured, iteration):
        scenario = self.args.scenario
        if scenario in ('create', 'delete', 'crud', 'upload'):
            row = self.create(measured and scenario in ('create', 'crud'))
            if scenario == 'upload':
                self.upload(row, measured)
            if scenario == 'crud':
                self.image(row, measured)
                self.patch(row, measured, iteration)
            self.remove(row, measured and scenario in ('delete', 'crud'))
        elif scenario == 'show':
            self.image(self.seeds[0], measured)
        elif scenario in ('list', 'pagination', 'filter'):
            self.listing(measured, scenario == 'pagination', scenario == 'filter')
        elif scenario == 'patch':
            self.patch(self.seeds[0], measured, iteration)
        elif scenario == 'download':
            self.download(self.seeds[0], measured)
        elif scenario == 'range':
            self.download(self.seeds[0], measured, True)
        elif scenario == 'binary-mix':
            self.image(self.seeds[0], measured) if iteration % 4 == 0 else self.download(self.seeds[0], measured)
        elif scenario == 'metadata-mix':
            if iteration % 5 < 4:
                self.image(self.seeds[0], measured)
            else:
                row = self.create(measured)
                self.patch(row, measured, iteration)
                self.remove(row, measured)

    def patch(self, row, measured, iteration):
        value = str(iteration)
        self.call('patch', 'PATCH', '/v2/images/' + row['id'], 200,
                  lambda v, s: v.get('id') == row['id'] and v.get('tt_revision') == value,
                  measured, body=[{'op': 'add', 'path': '/tt_revision', 'value': value}],
                  headers={'Content-Type': 'application/openstack-images-v2.1-json-patch'})



def official_contract(args, protocol, raw):
    if (protocol.get('status') != 'frozen' or protocol.get('official_execution_enabled') is not True
            or protocol.get('repetitions') != 5 or args.duration != protocol.get('measurement_s')
            or args.warmup != protocol.get('warmup_s')):
        raise ValueError('frozen official protocol and exact measurement parameters required')
    case = [args.scenario, args.concurrency, args.payload_info['bytes'] if args.payload_info else None]
    if case not in protocol.get('cases', []):
        raise ValueError('case outside frozen campaign scope')
    receipt_path = getattr(args, 'entry_evidence', None)
    if receipt_path is None:
        raise ValueError('entry evidence receipt required')
    receipt = json.loads(receipt_path.read_text())
    if (receipt.get('protocol_sha256') != hashlib.sha256(raw).hexdigest()
            or receipt.get('instrument_sha256') != hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
            or receipt.get('code_sha') != source_revision(Path(__file__).resolve().parents[2], getattr(args, 'code_sha', None))
            or receipt.get('valid_until_utc_s', 0) <= time.time()):
        raise ValueError('entry evidence is expired or not pinned to these instruments and protocol')
    if protocol.get('require_environment') and (not args.environment_sha256 or receipt.get('environment_sha256') != args.environment_sha256):
        raise ValueError('sanitized deployment environment must be pinned')
    required = set(protocol['entry_gates'])
    if set(receipt.get('gates', {})) != required:
        raise ValueError('incomplete entry gate receipt')
    for gate in receipt['gates'].values():
        path = Path(gate['path'])
        if gate.get('passed') is not True or path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != gate['sha256']:
            raise ValueError('entry gate evidence missing or changed')
    args.entry_evidence_sha256 = hashlib.sha256(receipt_path.read_bytes()).hexdigest()


def validate(args):
    if not args.allow_mutations:
        raise ValueError('explicit --allow-mutations required, including read scenarios with seeds')
    args.output = canonical_path(args.output)
    args.lock = canonical_path(args.lock)
    if args.output.exists():
        raise ValueError('fresh output required; use cleanup for existing ledger')
    uuid.UUID(args.campaign_id)
    if (not 1 <= args.concurrency <= 16 or not .05 <= args.duration <= 120
            or not 0 <= args.warmup <= 30 or not 1 <= args.max_requests <= 100000
            or not 1 <= args.max_payload_bytes <= 1024 ** 3):
        raise ValueError('pilot limits exceeded')
    for val in (args.duration, args.warmup, args.timeout):
        if not math.isfinite(val):
            raise ValueError('finite limits required')
    protocol_path = getattr(args, 'protocol', Path(__file__).resolve().parents[2] / 'docs/benchmarks/glance/protocol-v1.json')
    protocol_bytes = protocol_path.read_bytes()
    protocol = json.loads(protocol_bytes)
    if protocol.get('schema') != 1:
        raise ValueError('unsupported protocol schema')
    if args.stage != 'official' and protocol.get('official_execution_enabled') is not False:
        raise ValueError('pilot runs require their own draft protocol')
    args.protocol_identity = {'name': protocol['protocol'], 'sha256': hashlib.sha256(protocol_bytes).hexdigest(),
                              'status': protocol['status']}
    args.token = private_token(args.token_file)
    Client(args.url, args.token, args.timeout, args.max_payload_bytes)
    environment = getattr(args, 'environment', None)
    args.environment_sha256 = hashlib.sha256(environment.read_bytes()).hexdigest() if environment else None
    args.payload_info = None
    if args.scenario in BINARY:
        if not args.payload:
            raise ValueError('binary scenario requires payload')
        args.payload = canonical_path(args.payload)
        args.payload_info = payload_info(args.payload)
        if args.payload_info['bytes'] > args.max_payload_bytes:
            raise ValueError('payload exceeds explicit storage budget')
    if args.stage == 'official':
        official_contract(args, protocol, protocol_bytes)
    elif args.stage not in ('pilot', 'local-test'):
        raise ValueError('unsupported experimental stage')


def run(args):
    validate(args)
    with exclusive_lock(args.lock):
        args.output.mkdir(mode=0o700, parents=False)
        ledger = Ledger(args.output, args.campaign_id, args.url)
        stop = getattr(args, 'stop_event', None) or threading.Event()
        raw_lock = threading.Lock()
        rows, measured_rows, errors = [], [], []
        manifest = {'schema': 1, 'run_id': str(uuid.uuid4()), 'campaign_id': args.campaign_id,
                    'stage': args.stage, 'implementation': args.implementation, 'resource_group': args.implementation, 'protocol': args.protocol_identity,
                    'scenario': args.scenario, 'concurrency': args.concurrency,
                    'started_utc': utc(), 'url': args.url, 'payload': args.payload_info,
                    'duration_s': args.duration, 'warmup_s': args.warmup,
                    'timeout_s': args.timeout, 'max_requests': args.max_requests,
                    'instrument_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                    'code_sha': source_revision(Path(__file__).resolve().parents[2], getattr(args, 'code_sha', None)),
                    'load_model': 'closed-loop; complete cycles; preparation and cleanup outside measurement',
                    'latency_boundary': 'client setup/send through bounded body read; streaming digest included; JSON/semantic validation excluded',
                    'rate_boundary': 'wall interval includes cycle helpers, validation, ledger and drain',
                    'entry_evidence_sha256': getattr(args, 'entry_evidence_sha256', None),
                    'environment_sha256': args.environment_sha256,
                    'official_accepted': False, 'state': 'RUNNING'}
        save(args.output / 'manifest.json', manifest)
        raw = (args.output / 'samples.jsonl').open('x')

        def record(sample, measured):
            sample['phase'] = 'measurement' if measured else 'control_or_warmup'
            with raw_lock:
                if len(rows) >= args.max_requests:
                    stop.set()
                    raise RuntimeError('sample budget exhausted')
                raw.write(json.dumps(sample, separators=(',', ':')) + '\n')
                raw.flush()
                rows.append(sample)
                if measured:
                    measured_rows.append(sample)

        workers = [Worker(i, args, ledger, record, stop) for i in range(args.concurrency)]
        previous_handlers = {}
        if threading.current_thread() is threading.main_thread():
            for sig in (signal.SIGINT, signal.SIGTERM):
                previous_handlers[sig] = signal.getsignal(sig)
                signal.signal(sig, lambda signum, frame: stop.set())
        elapsed = None
        try:
            # Seeds are prepared serially to avoid listing a half-prepared catalog.
            for worker in workers:
                worker.prepare()
            with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
                for label, duration in [('warmup', args.warmup), ('measurement', args.duration)]:
                    if not duration:
                        continue
                    barrier = threading.Barrier(args.concurrency + 1)
                    shared = {}
                    def phase(worker):
                        barrier.wait()
                        iteration = 0
                        while not stop.is_set() and time.monotonic_ns() < shared['deadline']:
                            worker.cycle(label == 'measurement', iteration)
                            iteration += 1
                    futures = [pool.submit(phase, worker) for worker in workers]
                    shared['begin'] = time.monotonic_ns()
                    shared['deadline'] = shared['begin'] + int(duration * 1e9)
                    manifest[label + '_start_monotonic_ns'] = shared['begin']
                    manifest[label + '_start_utc_ns'] = time.time_ns()
                    barrier.wait()
                    for f in futures:
                        try:
                            f.result()
                        except Exception as error:
                            stop.set()
                            errors.append(type(error).__name__)
                    manifest[label + '_end_utc_ns'] = time.time_ns()
                    if label == 'measurement':
                        elapsed = (time.monotonic_ns() - shared['begin']) / 1e9
                    if stop.is_set():
                        break
        except Exception as error:
            errors.append(type(error).__name__)
            stop.set()
        finally:
            raw.close()
            for worker in workers:
                worker.client.close()
            client = Client(args.url, args.token, args.timeout, args.max_payload_bytes)
            try:
                cleanup = ledger.cleanup(client)
            except Exception as error:
                errors.append('cleanup_' + type(error).__name__)
                cleanup = False
            finally:
                client.close()
                for sig, handler in previous_handlers.items():
                    signal.signal(sig, handler)
            manifest.update(finished_utc=utc(), cleanup_verified=cleanup, errors=errors,
                            state=('COMPLETE_WINDOW' if args.stage == 'official' else 'COMPLETE_PILOT') if cleanup and not errors and measured_rows and not stop.is_set() else 'NEEDS_ATTENTION')
            stats = summarize(measured_rows, elapsed) if elapsed and measured_rows else None
            by_operation = {op: summarize([s for s in measured_rows if s['operation'] == op], elapsed)
                            for op in sorted({s['operation'] for s in measured_rows})} if elapsed else {}
            save(args.output / 'report.json', {'manifest': manifest, 'http': stats,
                                              'operations': by_operation, 'official_accepted': False})
            save(args.output / 'manifest.json', manifest)
            checksums(args.output)
        return 0 if manifest['state'] in ('COMPLETE_PILOT', 'COMPLETE_WINDOW') else 1


def checksums(root):
    files = sorted(p for p in root.iterdir() if p.is_file() and p.name != 'SHA256SUMS')
    lines = []
    for path in files:
        h = hashlib.sha256()
        with path.open('rb') as stream:
            for block in iter(lambda: stream.read(CHUNK), b''):
                h.update(block)
        lines.append(h.hexdigest() + '  ' + path.name + '\n')
    (root / 'SHA256SUMS').write_text(''.join(lines))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='action', required=True)
    p = sub.add_parser('run')
    p.add_argument('--url', required=True)
    p.add_argument('--token-file', type=Path, required=True)
    p.add_argument('--output', type=Path, required=True)
    p.add_argument('--lock', type=Path, required=True)
    p.add_argument('--campaign-id', default=str(uuid.uuid4()))
    p.add_argument('--code-sha')
    p.add_argument('--entry-evidence', type=Path)
    p.add_argument('--environment', type=Path)
    p.add_argument('--implementation', choices=('python', 'go'), required=True)
    p.add_argument('--scenario', choices=SCENARIOS, required=True)
    p.add_argument('--stage', choices=('pilot', 'local-test', 'official'), default='pilot')
    p.add_argument('--concurrency', type=int, default=1)
    p.add_argument('--duration', type=float, default=4)
    p.add_argument('--warmup', type=float, default=1)
    p.add_argument('--timeout', type=float, default=30)
    p.add_argument('--max-requests', type=int, default=10000)
    p.add_argument('--max-payload-bytes', type=int, default=64 * 1024 ** 2)
    p.add_argument('--payload', type=Path)
    p.add_argument('--protocol', type=Path, default=Path(__file__).resolve().parents[2] / 'docs/benchmarks/glance/protocol-v1.json')
    p.add_argument('--allow-mutations', action='store_true')
    pld = sub.add_parser('payload')
    pld.add_argument('--output', type=Path, required=True)
    pld.add_argument('--bytes', type=int, required=True)
    pld.add_argument('--seed', type=int, default=20261031)
    c = sub.add_parser('cleanup')
    c.add_argument('--output', type=Path, required=True)
    c.add_argument('--token-file', type=Path, required=True)
    c.add_argument('--lock', type=Path, required=True)
    args = parser.parse_args()
    try:
        if args.action == 'run':
            return run(args)
        if args.action == 'payload':
            info = generate_payload(args.output, args.bytes, args.seed)
            with args.output.with_name(args.output.name + '.manifest.json').open('x') as stream:
                json.dump(info, stream, indent=2)
                stream.write('\n')
            return 0
        root = canonical_path(args.output)
        m = json.loads((root / 'manifest.json').read_text())
        with exclusive_lock(args.lock):
            client = Client(m['url'], private_token(args.token_file), 30, 1024 ** 3)
            try:
                cleaned = Ledger(root, m['campaign_id'], m['url']).cleanup(client)
            finally:
                client.close()
            save(root / 'recovery.json', {'utc': utc(), 'cleanup_verified': cleaned,
                                         'official_accepted': False})
            checksums(root)
            return 0 if cleaned else 1
    except Exception as error:
        print('Glance instrument failed: ' + type(error).__name__, file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
