#!/usr/bin/env python3
"""B26 opt-in Glance Go integration suite inside the lab VM.

One command runs, in order and fail-fast, the functional contract of the Go
service, the reversible substitution behind Apache /image, a real Nova boot from
an image that only Glance Go stores, and the capture of the Nova → Glance flow.
It always restores the original route, removes its own server, image, runtime
and identities, and refuses to report a pass if any cleanup check fails.

Requires: the VM with DevStack, admin openrc in the environment, deployed
openstacksdk, a built cmd/glance binary, the CirrOS file DevStack downloaded and
explicit flavor/network IDs. Never starts the VM, never writes SQL to the
original Glance database and never touches the original store.
"""
import argparse
import datetime
import hashlib
import json
import logging
from pathlib import Path
import re
import shutil
import sys
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen
import uuid

HERE = Path(__file__).resolve().parent
sys.path[:0] = [str(HERE), str(HERE.parent / 'benchmarks'), str(HERE.parent)]
import glance_flow  # noqa: E402
import glance_switch  # noqa: E402
from glance_preflight import Session  # noqa: E402
from placement_suite import exclusive_run, require, save_json, sdk_connection  # noqa: E402

GO_URL = 'http://127.0.0.1:19293'  # fixed by glance_preflight.Session.prepare
MIB = 1024**2
CASES = ('preflight_and_isolation', 'auth_fail_closed', 'not_found', 'catalog_and_bytes_direct',
         'route_go_public', 'catalog_and_bytes_public', 'image_for_nova', 'nova_boot_from_go',
         'nova_glance_flow', 'server_and_image_removed')
CLEANUP = ('original_unchanged', 'runtime_removed', 'identity_removed', 'private_root_removed',
           'controls_restored')


def suite_passed(report):
    cleanup = report.get('cleanup', {})
    return (report.get('execution_error') is None
            and all(report.get('cases', {}).get(name, {}).get('passed') is True for name in CASES)
            and all(cleanup.get(name) is True for name in CLEANUP)
            and cleanup.get('pending_images') == 0)


def unauthenticated_status(url, token=None):
    headers = {'User-Agent': glance_flow.SUITE_AGENT}
    if token is not None:
        headers['X-Auth-Token'] = token
    try:
        with urlopen(Request(url.rstrip('/') + '/v2/images', headers=headers), timeout=10) as response:
            return response.status
    except HTTPError as error:
        try:
            return error.code
        finally:
            error.close()


def file_digest(path):
    hashes = {name: hashlib.new(name) for name in ('md5', 'sha256', 'sha512')}
    size = 0
    with Path(path).open('rb') as stream:
        while chunk := stream.read(1024 * 1024):
            size += len(chunk)
            for digest in hashes.values():
                digest.update(chunk)
    return {'size': size, **{name: digest.hexdigest() for name, digest in hashes.items()}}


def server_image_id(server):
    image = server.image
    if isinstance(image, dict):
        return image.get('id')
    return getattr(image, 'id', None)


def cirros_image(explicit):
    if explicit:
        return explicit
    found = sorted(Path('/opt/stack/devstack/files').glob('cirros-*-x86_64-disk.img'))
    require(len(found) == 1, 'exactly one DevStack CirrOS image (or --cirros-image)')
    return found[0]


class Suite(Session):
    def __init__(self, args):
        super().__init__(args)
        self.report = {'schema': 1, 'suite': 'glance-b26', 'run_id': 'tt-glance-b26-' + uuid.uuid4().hex[:12],
                       'started_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                       'cases': {}, 'cleanup': {}, 'execution_error': None, 'passed': False}
        self.connection = None
        self.server = None
        self.server_name = None
        self.nova_image = None
        self.window = None

    def checkpoint(self):
        save_json(self.args.output / 'report.json', self.report)

    def case(self, name):
        started = time.monotonic()
        try:
            detail = getattr(self, 'case_' + name)() or {}
        except Exception as error:
            self.report['cases'][name] = {'passed': False, 'error': type(error).__name__,
                                          'reason': str(error) if isinstance(error, AssertionError) else None,
                                          'seconds': round(time.monotonic() - started, 1)}
            self.checkpoint()
            raise
        self.report['cases'][name] = {'passed': True, 'seconds': round(time.monotonic() - started, 1), **detail}
        self.checkpoint()

    # -- cases, executed in CASES order ------------------------------------
    def case_preflight_and_isolation(self):
        self.prepare()
        return {'original_images': len(self.original['images'])}

    def case_auth_fail_closed(self):
        bogus = 'tt-invalid-' + uuid.uuid4().hex
        observed = {'go_missing_token': unauthenticated_status(GO_URL),
                    'go_invalid_token': unauthenticated_status(GO_URL, bogus),
                    'python_missing_token': unauthenticated_status(self.args.url)}
        require(observed['go_missing_token'] == observed['go_invalid_token'] == 401, 'Go rejects missing/invalid token')
        require(observed['python_missing_token'] == 401, 'original reference also answers 401')
        return observed

    def case_not_found(self):
        self.request(GO_URL, 'GET', '/v2/images/' + str(uuid.uuid4()), 404)
        return {}

    def case_catalog_and_bytes_direct(self):
        for size in self.args.payload_mib:
            self.cycle(f'go-direct-{size}m', GO_URL, size * MIB)
        return {'sizes_mib': self.args.payload_mib}

    def case_route_go_public(self):
        self.action('route-go', glance_switch, action='activate', root=self.runtime,
                    expected_original_sha=self.original['apache_site_sha256'], public_url=self.args.url,
                    lease_seconds=self.args.lease_seconds)
        require(not glance_switch.active(glance_switch.ORIGINAL_UNIT), 'original Glance stopped behind the route')
        require(unauthenticated_status(self.args.url) == 401, 'public route protected after switch')
        return {'lease_seconds': self.args.lease_seconds}

    def case_catalog_and_bytes_public(self):
        self.cycle('go-public-1m', self.args.url, MIB)
        return {}

    def case_image_for_nova(self):
        path = cirros_image(self.args.cirros_image)
        expected = file_digest(path)
        ident = str(uuid.uuid4())
        base = '/v2/images/' + ident
        name = 'tt-glance-preflight-cirros'
        image = {'id': ident, 'name': name, 'tags': [self.tag], 'disk_format': 'qcow2',
                 'container_format': 'bare', 'visibility': 'private'}
        self.request(self.args.url, 'GET', base, 404)
        self.pending[('go-public-cirros', ident)] = {'url': self.args.url, 'name': name}
        self.request(self.args.url, 'POST', '/v2/images', 201, body=image)
        self.request(self.args.url, 'PUT', base + '/file', 204, upload=path, content_type='application/octet-stream')
        active = self.request(self.args.url, 'GET', base, 200)
        require(active.get('status') == 'active' and active.get('size') == expected['size']
                and active.get('checksum') == expected['md5'] and active.get('os_hash_value') == expected['sha512'],
                'CirrOS active in Go with matching size/MD5/SHA-512')
        require(not (Path('/opt/stack/data/glance/images') / ident).exists(), 'bytes not in the original store')
        self.nova_image = {'id': ident, 'size': expected['size']}
        return {'image_size': expected['size'], 'source': path.name}

    def case_nova_boot_from_go(self):
        self.connection, project, _ = sdk_connection()
        flavor = self.connection.compute.get_flavor(self.args.flavor_id)
        network = self.connection.network.get_network(self.args.network_id)
        require(network.id == self.args.network_id and int(flavor.vcpus) >= 1, 'flavor/network preflight')
        self.server_name = self.report['run_id']
        require(not list(self.connection.compute.servers(name=self.server_name, all_projects=True)), 'fresh server name')
        since = time.time_ns() // 1000 - 1_000_000
        server = self.connection.compute.create_server(name=self.server_name, image_id=self.nova_image['id'],
                                                      flavor_id=self.args.flavor_id,
                                                      networks=[{'uuid': self.args.network_id}])
        self.server = server.id
        states, started = [], time.monotonic()
        while time.monotonic() - started < self.args.wait_seconds:
            current = self.connection.compute.get_server(server.id)
            if not states or states[-1]['status'] != current.status:
                states.append({'t_s': round(time.monotonic() - started, 1), 'status': current.status})
            if current.status in ('ACTIVE', 'ERROR'):
                break
            time.sleep(2)
        self.report['boot_states'] = states
        require(states and states[-1]['status'] == 'ACTIVE', 'server reached ACTIVE from the Go image')
        require(current.project_id == project and server_image_id(current) == self.nova_image['id'], 'server owner/image')
        time.sleep(3)  # let journald and Apache flush the last records of the window
        self.window = (since, time.time_ns() // 1000)
        return {'seconds_to_active': states[-1]['t_s'], 'states': [s['status'] for s in states]}

    def case_nova_glance_flow(self):
        state = json.loads((self.runtime / 'state.json').read_text())
        record = glance_flow.collect(state['go_unit'], *self.window, self.nova_image['size'])
        save_json(self.args.output / 'flow.json', record)
        (self.args.output / 'flow.mmd').write_text(glance_flow.mermaid(record['timeline']))
        require(record['accepted'], 'Nova read metadata and downloaded every byte from Glance Go')
        return record['summary']

    def case_server_and_image_removed(self):
        self.delete_server()
        ident = self.nova_image['id']
        self.request(self.args.url, 'DELETE', '/v2/images/' + ident, 204)
        self.request(self.args.url, 'GET', '/v2/images/' + ident, 404)
        self.pending.pop(('go-public-cirros', ident))
        return {}

    # -- cleanup -----------------------------------------------------------
    def delete_server(self):
        if self.connection is None:
            return
        if self.server is None and self.server_name:
            matches = [s for s in self.connection.compute.servers(name=self.server_name, all_projects=True)
                       if s.name == self.server_name]
            require(len(matches) <= 1, 'unambiguous owned server')
            self.server = matches[0].id if matches else None
        if self.server is None:
            return
        actual = self.connection.compute.get_server(self.server)
        require(actual.name == self.server_name, 'server ownership')
        self.connection.compute.delete_server(self.server, ignore_missing=False)
        self.connection.compute.wait_for_delete(actual, interval=2, wait=self.args.wait_seconds)
        self.server = None

    def cleanup(self):
        server_error = None
        try:
            self.delete_server()
        except Exception as error:  # keep going: route/runtime must still be restored
            server_error = type(error).__name__
        result = super().cleanup()
        result['server_removed'] = server_error is None and self.server is None
        if server_error:
            result['server_error'] = server_error
        return result

    def execute(self):
        try:
            for name in CASES:
                self.case(name)
        except Exception as error:
            self.report['execution_error'] = type(error).__name__
            self.keep_diagnostics()
        finally:
            try:
                self.report['cleanup'] = self.cleanup()
            except Exception as error:
                self.report['cleanup'] = {'error': type(error).__name__,
                                          'reason': str(error) if isinstance(error, AssertionError) else 'private state retained for repair'}
            finally:
                if self.connection is not None:
                    self.connection.close()
            self.report['request_cases'] = self.cases
            self.report['passed'] = suite_passed(self.report) and self.report['cleanup'].get('server_removed') is True
            self.report['finished_utc'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
            self.checkpoint()
        print(json.dumps({'run_id': self.report['run_id'], 'passed': self.report['passed'],
                          'cases_passed': sum(c['passed'] for c in self.report['cases'].values()),
                          'cases_required': len(CASES)}), flush=True)
        return 0 if self.report['passed'] else 1

    def keep_diagnostics(self):
        target = self.root.with_name(self.root.name + '-diagnostics')
        target.mkdir(mode=0o700, exist_ok=True)
        for log in self.root.rglob('*.log'):
            name = hashlib.sha256(str(log.relative_to(self.root)).encode()).hexdigest()[:10] + '-' + log.name
            shutil.copyfile(log, target / name)
        self.report['private_diagnostics'] = str(target)


def run(args):
    if not args.allow_mutations:
        raise ValueError('explicit mutation opt-in required')
    args.output.mkdir(parents=True, exist_ok=False)
    if (not args.work_root.is_absolute() or args.work_root != args.work_root.resolve()
            or not re.fullmatch(r'tt-placement-[a-z0-9-]+', args.work_root.name)):
        raise ValueError('canonical owned private work root required')
    if not args.binary.is_file() or not 10 <= args.wait_seconds <= 300 or not 300 <= args.lease_seconds <= 1800:
        raise ValueError('binary, bounded wait and bounded route lease required')
    if not args.payload_mib or any(size not in (1, 16, 64) for size in args.payload_mib):
        raise ValueError('payload sizes limited to 1, 16 and 64 MiB')
    with exclusive_run(Path('/run/lock/tt-placement-integration.lock')):
        args.work_root.mkdir(mode=0o700, exist_ok=False)
        logging.disable(logging.CRITICAL)
        return Suite(args).execute()


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--flavor-id', required=True)
    parser.add_argument('--network-id', required=True)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--work-root', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--url', default='http://192.168.122.10/image')
    parser.add_argument('--cirros-image', type=Path)
    parser.add_argument('--payload-mib', type=int, nargs='+', default=[1],
                        help='direct round-trip sizes; 1 keeps the run light, 1 16 64 matches the campaign')
    parser.add_argument('--wait-seconds', type=int, default=180)
    parser.add_argument('--lease-seconds', type=int, default=900)
    parser.add_argument('--allow-mutations', action='store_true')
    args = parser.parse_args()
    try:
        return run(args)
    except Exception as error:
        print('glance integration suite failed: ' + type(error).__name__, file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
