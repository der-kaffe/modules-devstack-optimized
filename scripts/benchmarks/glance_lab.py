#!/usr/bin/env python3
"""Exclusive Glance VM RPC: owned seeds, public routing, resources, restoration."""
import argparse
import configparser
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import logging
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
from urllib.parse import urlencode
import uuid

HERE = Path(__file__).resolve().parent
sys.path[:0] = [str(HERE), str(HERE.parent / 'integration'), str(HERE.parent)]
import glance_switch
import lab_identity
from glance_notifications import Control
from glance_http import Client, metadata_matches, payload
from glance_preflight import Session
from placement_differential import read_token
from placement_suite import exclusive_run, require, save_json, sdk_connection

FIELDS = ['id', 'name', 'owner', 'status', 'size', 'checksum', 'os_hash_algo', 'os_hash_value',
          'tags', 'visibility', 'disk_format', 'container_format']


def projection(image):
    return {k: image.get(k) for k in FIELDS}


def owned_descriptor(values, image, owner):
    """Only IDs derivable from this pair's namespace and exact own name/tag."""
    if not isinstance(image, dict) or image.get('owner') != owner or image.get('tags') != [values['tag']]:
        return False
    name = image.get('name', '')
    seed = re.fullmatch(r'tt-glance-seed-(\d+)', name)
    if seed:
        key = 'seed-' + seed[1]
        if int(seed[1]) >= values['seed_count']:
            return False
    else:
        match = re.fullmatch(r'tt-glance-(warmup|measurement)-w(\d+)-c(\d+)(?:-updated)?', name)
        if not match or int(match[2]) >= values['concurrency']:
            return False
        key = f'{match[1]}-{match[2]}-{match[3]}'
    return image.get('id') == str(uuid.uuid5(uuid.UUID(values['namespace']), key))


def effective_go_settings(journal):
    """Last "effective settings" line the running Glance Go logged (non-secret)."""
    found = None
    for line in journal.splitlines():
        try:
            entry = json.loads(line)
        except ValueError:
            continue
        if isinstance(entry, dict) and entry.get('msg') == 'effective settings':
            found = {k: v for k, v in entry.items() if k not in ('time', 'level', 'msg')}
    require(found is not None, 'Glance Go effective settings logged')
    return found


class Lab(Session):
    def __init__(self, args):
        super().__init__(args)
        self.current, self.collector = None, None
        self.closed = False
        self.ledger = []
        self.token_metadata = None
        self.notifications = Control(self.root)

    def refresh(self):
        self.action('identity-refresh', lab_identity, action='refresh', private_root=self.auth)
        self.token = read_token(self.auth / 'admin.token')
        self.token_metadata = json.loads((self.auth / 'metadata.json').read_text())
        return {'token': self.token, 'metadata': self.token_metadata}

    def prepare(self):
        connection, _, _ = sdk_connection()
        require(list(connection.compute.servers(all_projects=True)) == [], 'idle Nova lab required')
        super().prepare()
        if self.args.notification_policy == 'noop':
            save_json(self.args.output / 'notification-policy.json', self.notifications.install())
            glance_switch.wait_auth(self.args.url)
        self.python_store_baseline = sorted(p.name for p in Path('/opt/stack/data/glance/images').iterdir())
        self.original_apparmor_sha = hashlib.sha256(Path('/etc/apparmor.d/usr.sbin.mysqld').read_bytes()).hexdigest()
        self.runtime_state = json.loads((self.runtime / 'state.json').read_text())
        for size, label in [(1024**2, '1m'), (16 * 1024**2, '16m'), (64 * 1024**2, '64m')]:
            self.cycle('python-ready-' + label, self.args.url, size)
            self.cycle('go-ready-' + label, 'http://127.0.0.1:19293', size)
        # Prove Go bytes survive a real executable restart, outside any window.
        ident = str(uuid.uuid4())
        image = {'id': ident, 'name': 'tt-glance-durability', 'tags': [self.tag], 'disk_format': 'raw',
                 'container_format': 'bare', 'visibility': 'private'}
        url, base = 'http://127.0.0.1:19293', '/v2/images/' + ident
        self.request(url, 'GET', base, 404)
        self.pending[('go-durability', ident)] = {'url': url, 'name': image['name']}
        self.request(url, 'POST', '/v2/images', 201, body=image)
        data = self.root / 'payload-1048576.bin'
        expected = json.loads((self.root / 'payload-1048576.json').read_text())
        self.request(url, 'PUT', base + '/file', 204, upload=data, content_type='application/octet-stream')
        subprocess.run(['sudo', 'systemctl', 'restart', self.runtime_state['go_unit']], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        glance_switch.wait_auth(url)
        require(metadata_matches(self.request(url, 'GET', base, 200), {'status': 'active', 'size': expected['size'],
                                'checksum': expected['md5'], 'os_hash_value': expected['sha512']}), 'durable image metadata')
        self.request(url, 'GET', base + '/file', 200, expected_payload=expected)
        self.request(url, 'DELETE', base, 204)
        self.request(url, 'GET', base, 404)
        self.pending.pop(('go-durability', ident))
        save_json(self.args.output / 'functional-readiness.json', {'valid': True, 'durable_after_go_restart': True, 'cases': self.cases})
        return {'original': self.original, 'runtime': json.loads((self.runtime / 'public-report.json').read_text()),
                'environment': self.environment(), 'readiness': True, 'placement_control': 'Python unchanged'}

    def environment(self):
        def cmd(values):
            return subprocess.check_output(values, text=True, stderr=subprocess.DEVNULL).strip()
        cfg = configparser.RawConfigParser(strict=False)
        cfg.read('/etc/glance/glance-api.conf')
        settings = {}
        for section, keys in {'DEFAULT': ['workers', 'show_image_direct_url', 'delayed_delete', 'image_cache_max_size'],
                              'database': ['max_pool_size', 'max_overflow'],
                              'keystone_authtoken': ['token_cache_time'],
                              'glance_store': ['stores', 'default_store', 'filesystem_store_datadir']}.items():
            settings[section] = {k: cfg.get(section, k) for k in keys if cfg.has_option(section, k)}
        uwsgi = configparser.RawConfigParser(strict=False)
        uwsgi.read('/etc/glance/glance-uwsgi.ini')
        from sqlalchemy import create_engine, text
        engine = create_engine(cfg.get('database', 'connection'))
        try:
            with engine.connect() as connection:
                original_mysql = dict(connection.execute(text('SELECT @@server_uuid AS server_uuid, @@version AS version, @@innodb_buffer_pool_size AS buffer_pool, @@innodb_flush_log_at_trx_commit AS flush_log, @@sync_binlog AS sync_binlog, @@log_bin AS log_bin, @@max_connections AS max_connections')).mappings().one())
        finally:
            engine.dispose()
        import pymysql
        # Glance Go serves the original's catalog: same server, database and store.
        require(self.runtime_state.get('catalog') == 'official', 'Glance Go on the original catalog')
        own_mysql = dict(original_mysql)
        return {'kernel': cmd(['uname', '-a']), 'cpu': cmd(['lscpu']), 'memory': Path('/proc/meminfo').read_text(),
                'python_sha': cmd(['git', '-C', '/opt/stack/glance', 'rev-parse', 'HEAD']),
                'binary_sha256': hashlib.sha256(self.args.binary.read_bytes()).hexdigest(),
                'python_config_explicit_allowlist': settings,
                'notification_policy': self.args.notification_policy,
                'go_notification_policy': getattr(self.args, 'go_notifications', 'noop'),
                'go_route': getattr(self.args, 'go_route', 'direct'),
                'go_catalog': 'official',
                'uwsgi': {k: uwsgi.get('uwsgi', k) for k in ['processes', 'threads', 'master', 'enable-threads', 'lazy-apps', 'max-requests', 'harakiri'] if uwsgi.has_option('uwsgi', k)},
                'mysql_python': original_mysql, 'mysql_go': own_mysql,
                'go_settings': effective_go_settings(cmd(['sudo', 'journalctl', '-u', self.runtime_state['go_unit'], '-o', 'cat', '--no-pager'])),
                'systemd': {unit: cmd(['systemctl', 'show', unit, '-p', 'ControlGroup', '-p', 'ExecMainPID',
                                     '-p', 'CPUQuotaPerSecUSec', '-p', 'MemoryMax', '-p', 'TasksMax'])
                            for unit in ['devstack@g-api.service', self.runtime_state['go_unit']]},
                'deployment_limit': 'Python original shared DB/store vs own native MySQL/store; cache/worker/config effects are not language-only effects'}

    def route(self, implementation):
        require(implementation in ('python', 'go'), 'supported route')
        self.restore_route()
        if implementation == 'go':
            self.action('route-go-' + str(time.time_ns()), glance_switch, action='activate', root=self.runtime,
                        expected_original_sha=self.original['apache_site_sha256'], public_url=self.args.url, lease_seconds=1800)
        expected = self.original['apache_site_sha256'] if implementation == 'python' else hashlib.sha256(glance_switch.replacement(19293)).hexdigest()
        require(hashlib.sha256(glance_switch.SITE_FILE.read_bytes()).hexdigest() == expected and
                glance_switch.active(glance_switch.ORIGINAL_UNIT) == (implementation == 'python'), 'route implementation demonstrated')
        return {'implementation': implementation, 'site_sha256': expected, 'original_active': implementation == 'python'}

    def restore_route(self):
        path = self.runtime / 'glance-switch-state.json'
        if path.exists():
            state = json.loads(path.read_text())
            if state['active'] or hashlib.sha256(glance_switch.SITE_FILE.read_bytes()).hexdigest() != state['original_sha']:
                super().restore_route()

    def target(self):
        # Both through Apache's /image (route() points it at the implementation
        # measured), as Nova and Horizon reach Glance. Up to v4 Go was measured
        # directly on its port while Python went through Apache.
        if getattr(self.args, 'go_route', 'direct') == 'public':
            return self.args.url
        return 'http://127.0.0.1:19293' if self.current['implementation'] == 'go' else self.args.url

    def catalog(self):
        value = self.request(self.target(), 'GET', '/v2/images?' + urlencode({'tag': self.current['tag'], 'limit': 1000, 'sort': 'name:asc'}), 200)
        require(not value.get('next') and isinstance(value.get('images'), list), 'complete bounded owned catalog')
        return value['images']

    def roundtrip(self, values):
        if not values['scenario'].startswith('upload-') or not values.get('images'):
            return
        def probe(image):
            client = Client(self.args.url, self.token)
            try:
                sample, _ = client.request('GET', '/v2/images/' + image['id'] + '/file', expected_payload=values['payload'])
                require(sample['status'] == 200 and sample['error_kind'] is None, 'worker canary full bytes')
                return {'id': image['id'], 'bytes': sample['response_bytes'], 'valid': True}
            finally:
                client.close()
        with ThreadPoolExecutor(max_workers=values['concurrency']) as pool:
            return list(pool.map(probe, values['images']))

    def seed(self, values):
        require(self.current is None, 'finish previous run')
        require(re.fullmatch(r'[a-z0-9-]+', values['run_id']) and str(uuid.UUID(values['namespace'])) == values['namespace'], 'controlled run identity')
        values = dict(values)
        values['tag'] = 'tt-glance-v2-' + uuid.UUID(values['namespace']).hex
        scenario = values['scenario']
        # v7: nova-mix reads one seeded 16 MiB image; upload-qcow2 and
        # import-direct-16m only create their own images (no seeds).
        values['seed_count'] = (100 if scenario in ('catalog-list', 'catalog-show') else 1 if scenario.startswith('download-') or scenario == 'nova-mix'
                                else 0 if scenario in ('upload-qcow2', 'import-direct-16m') else values['concurrency'] if scenario.startswith('upload-') else 0)
        binary = scenario.startswith(('upload-', 'download-')) or scenario in ('nova-mix', 'import-direct-16m')
        self.current = values
        self.ledger.append(values)
        save_json(self.root / 'ledger.json', self.ledger, private=True)
        self.cases = []
        save_json(self.args.output / (values['run_id'] + '-logs.json'), self.controls('bound'))
        routing = self.route(values['implementation'])
        images = []
        require(not self.catalog(), 'fresh own tag')
        if binary and values.get('size_bytes'):
            size = values['size_bytes']
            data = self.root / ('payload-' + str(size) + '.bin')
            if not data.exists():
                save_json(self.root / ('payload-' + str(size) + '.json'), payload(data, size), private=True)
            values['payload'] = json.loads((self.root / ('payload-' + str(size) + '.json')).read_text())
        for i in range(values['seed_count']):
            ident = str(uuid.uuid5(uuid.UUID(values['namespace']), 'seed-' + str(i)))
            base = '/v2/images/' + ident
            self.request(self.target(), 'GET', base, 404)
            image = {'id': ident, 'name': 'tt-glance-seed-' + str(i), 'tags': [values['tag']], 'visibility': 'private',
                     'disk_format': 'raw', 'container_format': 'bare'}
            if values['implementation'] == 'python':
                require(not (Path('/opt/stack/data/glance/images') / ident).exists(), 'fresh Python seed file')
            created = self.request(self.target(), 'POST', '/v2/images', 201, body=image)
            require(metadata_matches(created, {**image, 'owner': self.owner, 'status': 'queued'}), 'owned seed creation')
            if binary:
                self.request(self.target(), 'PUT', base + '/file', 204, upload=data, content_type='application/octet-stream')
                created = self.request(self.target(), 'GET', base, 200)
                p = values['payload']
                require(metadata_matches(created, {'status': 'active', 'size': p['size'], 'checksum': p['md5'],
                                                  'os_hash_algo': 'sha512', 'os_hash_value': p['sha512']}), 'seed hashes')
                self.request(self.target(), 'GET', base + '/file', 200, expected_payload=p)
            images.append(projection(created))
        values['images'] = images
        values['owner'] = self.owner
        before = self.roundtrip(values)
        return {**values, 'url': self.args.url, 'routing': routing, 'roundtrip_before': before,
                'seed_snapshot': sorted(images, key=lambda x: x['id'])}

    def finish(self, strict=True):
        if self.current is None:
            return {'removed': True}
        values = self.current
        actual = self.catalog()
        for image in actual:
            require(owned_descriptor(values, image, self.owner), 'unproven image; never adopt or delete')
        if strict and 'images' in values:
            require(sorted([projection(i) for i in actual], key=lambda i: i['id']) == sorted(values['images'], key=lambda i: i['id']), 'post-window exact seed state')
        roundtrip = self.roundtrip(values) if strict and 'images' in values else None
        for image in actual:
            self.request(self.target(), 'DELETE', '/v2/images/' + image['id'], 204)
            self.request(self.target(), 'GET', '/v2/images/' + image['id'], 404)
        require(not self.catalog(), 'own tag empty')
        # Verify all UUID files generated by this run, including successful cycles
        # which were deleted by the client and are no longer in the API listing.
        # Both implementations write the original's store directory.
        require(sorted(p.name for p in Path('/opt/stack/data/glance/images').iterdir()) == self.python_store_baseline,
                'all payloads released; unrelated files preserved')
        require(not any(p for p in (self.runtime / 'store').rglob('*') if p.suffix in ('.blob', '.part', '.ready')), 'Go store fully released between runs')
        save_json(self.args.output / (values['run_id'] + '-validation.json'), {'cases': self.cases, 'roundtrip_after': roundtrip, 'owned_tag_empty': True})
        self.current = None
        return {'removed': True, 'seed_state_equal': True, 'roundtrip_after': roundtrip, 'own_tag_empty': True}

    def start_collector(self, run_id):
        require(self.collector is None and run_id == self.current['run_id'], 'collector/run identity')
        units = [('python', 'devstack@g-api.service'), ('go', self.runtime_state['go_unit']),
                 ('mysql-python', 'mysql.service'), ('mysql-go', self.runtime_state.get('db_unit') or ''),
                 ('apache', 'apache2.service'), ('keystone', 'devstack@keystone.service'),
                 ('placement-control', 'devstack@placement-api.service'), ('nova-compute', 'devstack@n-cpu.service'), ('nova-scheduler', 'devstack@n-sch.service'),
                 ('memcached', 'memcached.service')]  # the original's token cache lives here
        units = [(label, unit) for label, unit in units if unit]  # official catalog: no separate MySQL unit
        groups = {label: subprocess.check_output(['systemctl', 'show', '--value', '-p', 'ControlGroup', unit], text=True).strip() for label, unit in units}
        groups = {k: v for k, v in groups.items() if v}
        require(self.current['implementation'] in groups, 'measured Glance cgroup')
        path = self.args.output / (run_id + '-groups.json')
        save_json(path, groups)
        output, stop = self.args.output / (run_id + '-resources'), self.root / (run_id + '.stop')
        log = (self.args.output / (run_id + '-collector.log')).open('x')
        process = subprocess.Popen(['sudo', sys.executable, str(HERE / 'placement_resources.py'), '--groups', str(path), '--output', str(output),
                                    '--stop-file', str(stop), '--maximum-seconds', '900'], stdout=log, stderr=log)
        memory = subprocess.Popen(['sudo', sys.executable, str(HERE / 'glance_memory.py'), '--groups', str(path),
                                   '--output', str(self.args.output / (run_id + '-memory.jsonl')), '--stop-file', str(stop),
                                   '--maximum-seconds', '900'], stdout=log, stderr=log)
        self.collector = process, log, output, stop
        self.memory_sampler = memory
        time.sleep(5)
        require(process.poll() is None and memory.poll() is None, 'collectors running')
        return {'groups': groups, 'idle_s': 5}

    def stop_collector(self):
        if self.collector is None:
            return {'stopped': True}
        process, log, output, stop = self.collector
        time.sleep(2)  # Explicit post-load membership bracket, outside timers.
        stop.touch(mode=0o600)
        process.wait(timeout=10)
        memory = getattr(self, 'memory_sampler', None)
        if memory is not None:
            memory.wait(timeout=10)
            self.memory_sampler = None
        log.close()
        self.collector = None
        require(process.returncode == 0 and (memory is None or memory.returncode == 0), 'collector exit')
        return {'stopped': True, 'output': str(output)}

    def cleanup(self):
        if self.closed:
            return {'already_closed': True}
        self.stop_collector()
        try:
            self.finish(strict=False)
        finally:
            self.restore_route()
        if self.notifications.state.exists():
            save_json(self.args.output / 'notification-restoration.json', self.notifications.restore())
            glance_switch.wait_auth(self.args.url)
        # Session checks the original catalog/sites/Placement, closes owned
        # runtime and identity, restores apt/log controls, trims guest and exits.
        if hasattr(self, 'original_apparmor_sha'):
            require(hashlib.sha256(Path('/etc/apparmor.d/usr.sbin.mysqld').read_bytes()).hexdigest() == self.original_apparmor_sha,
                    'original MySQL AppArmor profile unchanged')
        result = super().cleanup()
        self.closed = True
        save_json(self.args.output / 'cleanup.json', result)
        return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['work-root', 'output', 'binary']:
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--url', default='http://192.168.122.10/image')
    parser.add_argument('--go-route', choices=['direct', 'public'], default='direct')
    parser.add_argument('--go-catalog', choices=['official'], default='official',
                        help='kept for older drivers; Glance Go only serves the original catalog')
    parser.add_argument('--allow-mutations', action='store_true')
    parser.add_argument('--notification-policy', choices=['original', 'noop'], default='original')
    parser.add_argument('--go-notifications', choices=['original', 'noop'], default='noop',
                        help="'original': Glance Go sends notifications with the original's settings")
    args = parser.parse_args()
    require(args.allow_mutations and args.work_root.is_absolute() and args.work_root == args.work_root.resolve()
            and re.fullmatch(r'tt-placement-[a-z0-9-]+', args.work_root.name), 'owned canonical private root and opt-in')
    logging.disable(logging.CRITICAL)
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    with exclusive_run(Path('/run/lock/tt-placement-integration.lock')):
        args.work_root.mkdir(mode=0o700, exist_ok=False)
        lab = Lab(args)
        try:
            for line in sys.stdin:
                try:
                    request = json.loads(line)
                    action = request['action']
                    methods = {'prepare': lab.prepare, 'token': lab.refresh, 'finish': lab.finish,
                               'stop-collector': lab.stop_collector, 'cleanup': lab.cleanup}
                    if action == 'ping':
                        value = {'utc_ns': time.time_ns()}
                    elif action == 'seed':
                        value = lab.seed(request['values'])
                    elif action == 'route':
                        value = lab.route(request['implementation'])
                    elif action == 'start-collector':
                        value = lab.start_collector(request['run_id'])
                    else:
                        value = methods[action]()
                    print(json.dumps({'ok': True, 'value': value}), flush=True)
                except Exception as error:
                    print(json.dumps({'ok': False, 'error': type(error).__name__,
                                      'reason': str(error) if isinstance(error, AssertionError) else None}), flush=True)
                if lab.closed:
                    break
        finally:
            try:
                lab.cleanup()
            except Exception as error:
                save_json(args.output / 'cleanup-error.json', {'error': type(error).__name__, 'private_state_retained': True})


if __name__ == '__main__':
    main()
