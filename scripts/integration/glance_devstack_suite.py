#!/usr/bin/env python3
"""Glance Go integration suite on a DevStack installed with the glance-go plugin.

It replaces the B26 suite (which started a temporary Glance Go) and the shared
catalog E2E: everything goes through the public /image endpoint to the
permanent DevStack service devstack@g-go, as Nova and any client use it. It
creates no services, units, accounts or routes.

Cases, in order (fail-fast):
  devstack_integration   devstack@g-go active and enabled, g-api stopped and
                         disabled, Apache /image -> g-go, Keystone image endpoint
  auth_fail_closed       401 without or with an invalid token; discovery 300
  not_found              404 for an unknown image
  catalog_and_bytes      create, PATCH, upload, download (hashes), list, delete;
                         the file <id> appears and disappears in the store
  notifications          the cycle's messagingv2 notifications equal the
                         original's (recorded baseline)
  devstack_cirros_served the CirrOS DevStack uploaded through g-api is served
                         by Glance Go with its hashes
  nova_boot_from_devstack_cirros   Nova boots from that image
  image_for_nova         CirrOS uploaded through Glance Go (bytes as <id>)
  nova_boot_from_go      Nova boots from the image Glance Go stored
  nova_glance_flow       Nova read the metadata and every byte from Glance Go
  server_and_image_removed

Run inside the VM with admin openrc loaded:
  python3 scripts/integration/glance_devstack_suite.py --flavor-id 42 \\
      --network-id <private> --output <NEW dir> --allow-mutations
"""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys
import time
import uuid

HERE = Path(__file__).resolve().parent
sys.path[:0] = [str(HERE), str(HERE.parent / 'benchmarks')]
import glance_flow  # noqa: E402
from glance_suite import boot_fault, cirros_image, compare_notifications, file_digest, server_image_id, unauthenticated_status  # noqa: E402
from placement_runtime import glance_notification_settings  # noqa: E402
from placement_suite import require, save_json, sdk_connection  # noqa: E402

UNIT, ORIGINAL_UNIT = 'devstack@g-go.service', 'devstack@g-api.service'
GLANCE_CONF = Path('/etc/glance/glance-api.conf')
APACHE_SITE = Path('/etc/apache2/sites-available/glance-api.conf')
BASELINE = HERE / 'fixtures' / 'glance-notifications-original.json'
MIB = 1024**2
CASES = ('devstack_integration', 'auth_fail_closed', 'not_found', 'catalog_and_bytes', 'notifications',
         'devstack_cirros_served', 'nova_boot_from_devstack_cirros', 'image_for_nova', 'nova_boot_from_go',
         'nova_glance_flow', 'server_and_image_removed')


def unit_listen(environment):
    """GLANCE_GO_LISTEN_ADDR from `systemctl show -p Environment`."""
    match = re.search(r'(?:^|\s)GLANCE_GO_LISTEN_ADDR=(\S+)', environment)
    require(match is not None, 'g-go listen address in its unit')
    return match.group(1)


def proxied_backend(site_text):
    match = re.search(r'^ProxyPass\s+"/image"\s+"(http://[^"]+)"', site_text, re.M)
    require(match is not None, 'Apache /image proxy')
    return match.group(1).rstrip('/')


def render_baseline(messages, values):
    """The original's recorded notifications with this installation's values."""
    def fill(v):
        if isinstance(v, dict):
            return {k: fill(x) for k, x in v.items()}
        if isinstance(v, list):
            return [fill(x) for x in v]
        return values.get(v, v) if isinstance(v, str) else v
    return [{**m, 'publisher_id': values.get(m['publisher_id'], m['publisher_id']), 'payload': fill(m['payload'])} for m in messages]


def stored(image_id, store=Path('/opt/stack/data/glance/images')):
    return store / image_id


class Suite:
    def __init__(self, args):
        self.args = args
        self.report = {'schema': 1, 'suite': 'glance-devstack', 'run_id': 'tt-glance-ds-' + uuid.uuid4().hex[:12],
                       'started_utc': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()), 'cases': {}}
        self.tag = self.report['run_id']
        self.pending, self.servers = set(), set()
        self.connection, self.project, self.user = sdk_connection()
        self.token = self.connection.session.get_token()
        self.url = self.connection.session.get_endpoint(service_type='image', interface='public').rstrip('/')

    def request(self, method, path, expected, **kwargs):
        from glance_http import Client
        c = Client(self.url, self.token, binary_timeout=600)
        try:
            sample, body = c.request(method, path, **kwargs)
        finally:
            c.close()
        require(sample['status'] == expected, '%s %s -> %s, expected %s' % (method, path, sample['status'], expected))
        return body

    def case(self, name):
        started = time.monotonic()
        try:
            details = getattr(self, 'case_' + name)() or {}
            self.report['cases'][name] = {'passed': True, 'seconds': round(time.monotonic() - started, 1), **details}
        except Exception as error:
            self.report['cases'][name] = {'passed': False, 'seconds': round(time.monotonic() - started, 1),
                                          'error': type(error).__name__, 'reason': str(error)[:300]}
            raise

    # -- DevStack integration --------------------------------------------------
    def case_devstack_integration(self):
        def show(unit, prop):
            return subprocess.run(['systemctl', 'show', '-p', prop, '--value', unit], capture_output=True, text=True).stdout.strip()
        require(show(UNIT, 'ActiveState') == 'active' and show(UNIT, 'UnitFileState') == 'enabled', 'devstack@g-go active and enabled')
        # Go only (default): the original is not installed at all; with
        # GLANCE_GO_INSTALL_ORIGINAL it is installed, stopped and disabled.
        original = show(ORIGINAL_UNIT, 'LoadState')
        require(original == 'not-found' or show(ORIGINAL_UNIT, 'ActiveState') != 'active' and show(ORIGINAL_UNIT, 'UnitFileState') == 'disabled',
                'devstack@g-api absent, or stopped and disabled')
        listen = unit_listen(show(UNIT, 'Environment'))
        require(proxied_backend(APACHE_SITE.read_text()) == 'http://' + listen, 'Apache /image proxies to g-go')
        require(self.url.endswith('/image'), 'Keystone image endpoint is /image')
        return {'listen': listen, 'endpoint': self.url, 'original_installed': original != 'not-found'}

    def case_auth_fail_closed(self):
        for token in (None, 'tt-invalid-token'):
            require(unauthenticated_status(self.url, token) == 401, '401 with token %r' % token)
        out = subprocess.run(['curl', '-s', '-o', '/dev/null', '-w', '%{http_code}', self.url + '/'], capture_output=True, text=True).stdout
        require(out == '300', 'version discovery 300')
        return {}

    def case_not_found(self):
        self.request('GET', '/v2/images/' + str(uuid.uuid4()), 404)
        return {}

    def case_catalog_and_bytes(self):
        from glance_http import payload
        sizes = []
        for mib in self.args.payload_mib:
            data = Path(self.args.output) / ('payload-%dm.bin' % mib)
            expected = payload(data, mib * MIB)
            ident = str(uuid.uuid4())
            self.pending.add(ident)
            self.request('POST', '/v2/images', 201, body={'id': ident, 'name': 'tt-ds-%dm' % mib, 'tags': [self.tag],
                                                          'disk_format': 'raw', 'container_format': 'bare', 'visibility': 'private'})
            self.request('PATCH', '/v2/images/' + ident, 200, body=[{'op': 'add', 'path': '/tt_case', 'value': 'catalog'}],
                         content_type='application/openstack-images-v2.1-json-patch')
            self.request('PUT', '/v2/images/' + ident + '/file', 204, upload=data, content_type='application/octet-stream')
            meta = self.request('GET', '/v2/images/' + ident, 200)
            require(meta['status'] == 'active' and meta['size'] == mib * MIB and meta.get('tt_case') == 'catalog', 'active with metadata')
            require(stored(ident).is_file() and stored(ident).stat().st_size == mib * MIB, 'file <id> in the store')
            self.request('GET', '/v2/images/' + ident + '/file', 200, expected_payload=expected)
            listed = self.request('GET', '/v2/images?limit=1000&tag=' + self.tag, 200)
            require(ident in [i['id'] for i in listed['images']], 'listed by tag')
            self.request('DELETE', '/v2/images/' + ident, 204)
            self.request('GET', '/v2/images/' + ident, 404)
            require(not stored(ident).exists(), 'file removed from the store')
            self.pending.discard(ident)
            sizes.append(mib)
        return {'sizes_mib': sizes}

    def case_notifications(self):
        from kombu import Connection, Exchange, Queue
        from glance_http import payload
        settings = glance_notification_settings(GLANCE_CONF.read_text())
        require(settings['driver'] == 'messagingv2', 'notifications enabled (messagingv2) in glance-api.conf')
        data = Path(self.args.output) / 'notif-1m.bin'
        expected = payload(data, MIB, seed=b'glance-notifications')
        ident, messages = str(uuid.uuid4()), []
        with Connection(settings['transport_url'].replace('rabbit://', 'amqp://', 1)) as conn:
            queue = Queue('tt-notif-' + uuid.uuid4().hex[:8], exchange=Exchange('glance', type='topic', durable=False, auto_delete=False),
                          routing_key='#', exclusive=True, auto_delete=True, durable=False)
            queue(conn.default_channel).declare()
            self.pending.add(ident)
            body = {'id': ident, 'name': 'tt-glance-preflight-notif', 'tags': [self.tag], 'disk_format': 'raw',
                    'container_format': 'bare', 'visibility': 'private', 'tt_notif': 'v1'}
            for method, path, status, kw in (
                    ('POST', '/v2/images', 201, {'body': body}),
                    ('PATCH', '/v2/images/' + ident, 200, {'body': [{'op': 'replace', 'path': '/name', 'value': 'tt-glance-preflight-notif-p'}],
                                                          'content_type': 'application/openstack-images-v2.1-json-patch'}),
                    ('PUT', '/v2/images/' + ident + '/file', 204, {'upload': data, 'content_type': 'application/octet-stream'}),
                    ('GET', '/v2/images/' + ident + '/file', 200, {'expected_payload': expected}),
                    ('DELETE', '/v2/images/' + ident, 204, {})):
                self.request(method, path, status, **kw)
            self.pending.discard(ident)
            time.sleep(3)  # the asynchronous publisher drains its queue
            with conn.SimpleQueue(queue) as q:
                while True:
                    try:
                        m = q.get(timeout=2)
                    except q.Empty:
                        break
                    outer = m.payload if isinstance(m.payload, dict) else json.loads(m.body)
                    inner = json.loads(outer['oslo.message'])
                    p = inner.get('payload')
                    if isinstance(p, dict) and ident in (p.get('id'), p.get('image_id')) or isinstance(p, str) and ident in p:
                        messages.append({'event_type': inner['event_type'], 'priority': inner['priority'],
                                         'publisher_id': inner['publisher_id'], 'routing_key': m.delivery_info.get('routing_key'),
                                         'properties': {k: m.properties.get(k) for k in ('content_type', 'content_encoding', 'delivery_mode')},
                                         'oslo_version': outer.get('oslo.version'), 'payload': p})
                    m.ack()
        baseline = render_baseline(json.loads(BASELINE.read_text())['messages'],
                                   {'{project}': self.project, '{user}': self.user, '{tag}': self.tag, '{publisher}': settings['publisher_id']})
        diffs = compare_notifications(baseline, messages)
        save_json(Path(self.args.output) / 'notifications.json', {'events': [m['event_type'] for m in messages], 'differences': diffs})
        require(not diffs, 'notifications equal the original: ' + '; '.join(diffs[:3]))
        return {'events': [m['event_type'] for m in messages], 'differences': 0}

    # -- Nova ------------------------------------------------------------------
    def devstack_cirros(self):
        images = self.request('GET', '/v2/images?limit=1000', 200)['images']
        found = [i for i in images if re.fullmatch(r'cirros-[0-9.]+-x86_64-disk', i['name']) and i['status'] == 'active']
        require(len(found) == 1, 'one DevStack CirrOS image')
        return found[0]

    def case_devstack_cirros_served(self):
        cirros = self.devstack_cirros()
        out = Path(self.args.output) / 'devstack-cirros.img'
        subprocess.run(['curl', '-sf', '-H', 'X-Auth-Token: ' + self.token, '-o', str(out),
                        self.url + '/v2/images/' + cirros['id'] + '/file'], check=True)
        got = file_digest(out)
        out.unlink()
        require((got['size'], got['md5'], got['sha512']) == (cirros['size'], cirros['checksum'], cirros['os_hash_value']),
                'Glance Go serves the CirrOS DevStack uploaded with its hashes')
        return {'image': cirros['id'], 'size': cirros['size']}

    def boot(self, image_id):
        compute = self.connection.compute
        name = '%s-%s' % (self.report['run_id'], uuid.uuid4().hex[:6])
        since = time.time_ns() // 1000
        server = compute.create_server(name=name, image_id=image_id, flavor_id=self.args.flavor_id,
                                       networks=[{'uuid': self.args.network_id}])
        self.servers.add(server.id)
        states, started, current = [], time.monotonic(), server
        while time.monotonic() - started < self.args.wait_seconds:
            current = compute.get_server(server.id)
            if not states or states[-1]['status'] != current.status:
                states.append({'t_s': round(time.monotonic() - started, 1), 'status': current.status})
            if current.status in ('ACTIVE', 'ERROR'):
                break
            time.sleep(2)
        if states and states[-1]['status'] == 'ERROR':
            self.report['boot_fault'] = boot_fault(current)
        require(states and states[-1]['status'] == 'ACTIVE', 'server ACTIVE from image ' + image_id)
        require(server_image_id(current) == image_id, 'server image')
        time.sleep(3)  # journald and Apache flush the window
        return server.id, (since, time.time_ns() // 1000), [s['status'] for s in states]

    def remove_server(self, server_id):
        compute = self.connection.compute
        server = compute.get_server(server_id)
        compute.delete_server(server_id, ignore_missing=False)
        compute.wait_for_delete(server, interval=2, wait=self.args.wait_seconds)
        self.servers.discard(server_id)

    def case_nova_boot_from_devstack_cirros(self):
        server, _, states = self.boot(self.devstack_cirros()['id'])
        self.remove_server(server)
        return {'states': states}

    def case_image_for_nova(self):
        path = cirros_image(self.args.cirros_image)
        expected = file_digest(path)
        ident = str(uuid.uuid4())
        self.pending.add(ident)
        self.request('POST', '/v2/images', 201, body={'id': ident, 'name': 'tt-glance-preflight-cirros', 'tags': [self.tag],
                                                      'disk_format': 'qcow2', 'container_format': 'bare', 'visibility': 'private'})
        self.request('PUT', '/v2/images/' + ident + '/file', 204, upload=path, content_type='application/octet-stream')
        meta = self.request('GET', '/v2/images/' + ident, 200)
        require(meta['status'] == 'active' and (meta['size'], meta['checksum'], meta['os_hash_value']) ==
                (expected['size'], expected['md5'], expected['sha512']), 'CirrOS active with its hashes')
        require(stored(ident).is_file() and file_digest(stored(ident)) == expected, 'bytes as <id> in the store')
        self.nova_image = {'id': ident, 'size': expected['size']}
        return {'image_size': expected['size']}

    def case_nova_boot_from_go(self):
        self.server, self.window, states = self.boot(self.nova_image['id'])
        return {'states': states}

    def case_nova_glance_flow(self):
        record = glance_flow.collect(UNIT, *self.window, self.nova_image['size'])
        save_json(Path(self.args.output) / 'flow.json', record)
        (Path(self.args.output) / 'flow.mmd').write_text(glance_flow.mermaid(record['timeline']))
        require(record['accepted'], 'Nova read metadata and downloaded every byte from Glance Go')
        return record['summary']

    def case_server_and_image_removed(self):
        self.remove_server(self.server)
        ident = self.nova_image['id']
        self.request('DELETE', '/v2/images/' + ident, 204)
        self.request('GET', '/v2/images/' + ident, 404)
        require(not stored(ident).exists(), 'file removed')
        self.pending.discard(ident)
        return {}

    def cleanup(self):
        result = {}
        for server in list(self.servers):
            try:
                self.remove_server(server)
            except Exception as error:
                result['server_error'] = type(error).__name__
        for ident in list(self.pending):
            try:
                self.request('DELETE', '/v2/images/' + ident, 204)
                self.pending.discard(ident)
            except Exception:
                pass
        left = self.request('GET', '/v2/images?limit=1000&tag=' + self.tag, 200)['images']
        result.update(servers_removed=not self.servers, images_removed=not self.pending and not left)
        return result

    def run(self):
        try:
            for name in CASES:
                self.case(name)
        except Exception as error:
            self.report['execution_error'] = type(error).__name__
        finally:
            self.report['cleanup'] = self.cleanup()
            self.report['finished_utc'] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
            cases = self.report['cases']
            self.report['passed'] = (all(cases.get(n, {}).get('passed') for n in CASES)
                                     and all(self.report['cleanup'].get(k) for k in ('servers_removed', 'images_removed')))
            save_json(Path(self.args.output) / 'report.json', self.report)
        print(json.dumps({'run_id': self.report['run_id'], 'passed': self.report['passed'],
                          'cases_passed': sum(1 for c in self.report['cases'].values() if c['passed']), 'cases_required': len(CASES)}))
        return 0 if self.report['passed'] else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--flavor-id', required=True)
    parser.add_argument('--network-id', required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--payload-mib', type=int, nargs='+', default=[1])
    parser.add_argument('--cirros-image', type=Path)
    parser.add_argument('--wait-seconds', type=int, default=300)
    parser.add_argument('--allow-mutations', action='store_true')
    args = parser.parse_args()
    if not args.allow_mutations:
        print('explicit --allow-mutations required', file=sys.stderr)
        return 2
    if any(m not in (1, 16, 64) for m in args.payload_mib) or not 10 <= args.wait_seconds <= 900:
        print('payload sizes 1/16/64 MiB and 10..900 s waits only', file=sys.stderr)
        return 2
    args.output.mkdir(parents=True, exist_ok=False)
    return Suite(args).run()


if __name__ == '__main__':
    sys.exit(main())
