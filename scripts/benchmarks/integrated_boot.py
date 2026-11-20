#!/usr/bin/env python3
"""One sequential Nova boot observation with write-ahead ownership and recovery.

Does not switch services or certify A/B/C/D deployment identity. Run only inside
an already prepared, exclusively locked lab. Pilot only; official gates remain
closed until joint deployment, resource sampling and restoration are verified.
"""
import argparse
import hashlib
import json
import logging
import math
import os
import re
from pathlib import Path
import sys
import subprocess
import time
from urllib.parse import urlsplit
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parent))
from glance_benchmark import canonical_path, checksums, exclusive_lock, save, utc, source_revision


class SDKGateway:
    """Public APIs only; no credentials, server fault body or user data in reports."""
    def __init__(self, placement_url):
        e = urlsplit(placement_url)
        if (e.scheme not in ('http', 'https') or e.hostname != '192.168.122.10'
                or e.username or e.password or e.query or e.fragment or e.path.rstrip('/') != '/placement'):
            raise ValueError('identified lab Placement endpoint required')
        if os.environ.get('OS_AUTH_URL', '').rstrip('/') not in ('http://192.168.122.10/identity', 'http://192.168.122.10/identity/v3'):
            raise ValueError('identified lab Keystone environment required')
        logging.disable(logging.CRITICAL)
        from openstack.connection import Connection
        from keystoneauth1 import session
        from keystoneauth1.identity import v3
        auth = v3.Password(auth_url=os.environ['OS_AUTH_URL'].rstrip('/') + ('' if os.environ['OS_AUTH_URL'].rstrip('/').endswith('/v3') else '/v3'),
                           username=os.environ['OS_USERNAME'], password=os.environ['OS_PASSWORD'],
                           project_name=os.environ['OS_PROJECT_NAME'],
                           user_domain_name=os.environ.get('OS_USER_DOMAIN_NAME', 'Default'),
                           project_domain_name=os.environ.get('OS_PROJECT_DOMAIN_NAME', 'Default'))
        self.connection = Connection(session=session.Session(auth=auth, timeout=15),
                                     region_name=os.environ.get('OS_REGION_NAME', 'RegionOne'),
                                     image_api_version='2', image_endpoint_override='http://192.168.122.10/image/v2')
        self.placement_url = placement_url.rstrip('/')
        for service in ('compute', 'image', 'network'):
            effective = self.connection.session.get_endpoint(service_type=service, interface='public')
            parsed = urlsplit(effective)
            if parsed.hostname != '192.168.122.10' or parsed.username or parsed.password:
                raise ValueError('catalog routes outside local lab rejected')

    def servers(self):
        return [dict(id=s.id, name=s.name, metadata=s.metadata or {})
                for s in self.connection.compute.servers(details=True, all_projects=True)]

    def inputs(self, args):
        image = self.connection.image.get_image(args.image_id)
        flavor = self.connection.compute.get_flavor(args.flavor_id)
        network = self.connection.network.get_network(args.network_id)
        if image.status.lower() != 'active' or image.hash_algo != 'sha512' or image.hash_value != args.image_sha512:
            raise ValueError('boot image identity/hash/status mismatch')
        if network.id != args.network_id or int(flavor.vcpus) < 1:
            raise ValueError('flavor/network preflight failed')
        return {'image_id': image.id, 'image_sha512': image.hash_value,
                'flavor_id': flavor.id, 'network_id': network.id,
                'resources': {'VCPU': int(flavor.vcpus), 'MEMORY_MB': int(flavor.ram), 'DISK_GB': int(flavor.disk)}}

    def create(self, name, campaign, inputs):
        server = self.connection.compute.create_server(name=name, metadata={'tt_campaign': campaign},
                                                       image_id=inputs['image_id'], flavor_id=inputs['flavor_id'],
                                                       networks=[{'uuid': inputs['network_id']}])
        return server.id

    def get(self, ident):
        try:
            s = self.connection.compute.get_server(ident)
            return dict(id=s.id, name=s.name, metadata=s.metadata or {}, status=s.status)
        except Exception as error:
            from openstack.exceptions import ResourceNotFound
            if isinstance(error, ResourceNotFound):
                return None
            raise

    def delete(self, ident):
        self.connection.compute.delete_server(ident, ignore_missing=True)

    def allocations(self, ident):
        r = self.connection.session.get(self.placement_url + '/allocations/' + ident,
                                        headers={'OpenStack-API-Version': 'placement 1.28'}, timeout=15)
        raw = r.content
        if len(raw) > 2 * 1024 * 1024:
            raise ValueError('oversized Placement response')
        result = json.loads(raw)
        if not isinstance(result.get('allocations'), dict):
            raise ValueError('invalid allocations')
        return result['allocations']


def owned(server, report):
    return (server is not None and server['id'] not in report['baseline_ids'] and
            server['name'] == report['name'] and server['metadata'].get('tt_campaign') == report['campaign_id'])


def cleanup(gateway, root, report, timeout, poll):
    """Recover ambiguous create only by exact identity+metadata, never by prefix."""
    if not all(math.isfinite(x) for x in (timeout, poll)) or not 1 <= timeout <= 180 or not .01 <= poll <= 5:
        raise ValueError('bounded recovery window required')
    candidates = [s for s in gateway.servers() if s['name'] == report['name']]
    if report['server_id']:
        current = gateway.get(report['server_id'])
        if current is not None and all(s['id'] != current['id'] for s in candidates):
            candidates.append(current)
    if len(candidates) > 1 or any(not owned(s, report) for s in candidates):
        raise ValueError('ambiguous or foreign server; retain recovery ledger')
    ident = report['server_id']
    if candidates:
        ident = candidates[0]['id']
        report['server_id'] = ident
        save(root / 'report.json', report)
        # Recheck the fetched object immediately before delete.
        current = gateway.get(ident)
        if current is not None:
            if not owned(current, report):
                raise ValueError('server ownership changed')
            gateway.delete(ident)
    end = time.monotonic() + timeout
    while ident and time.monotonic() < end:
        if gateway.get(ident) is None and not gateway.allocations(ident):
            break
        time.sleep(poll)
    else:
        if ident:
            raise TimeoutError('server deletion or allocation release unconfirmed')
    remaining = gateway.servers()
    ids = {s['id'] for s in remaining}
    if not set(report['baseline_ids']).issubset(ids) or any(s['name'] == report['name'] for s in remaining):
        raise ValueError('baseline server inventory or owned cleanup changed')
    report['cleanup_verified'] = True
    report['allocations_released'] = True if ident else None
    save(root / 'report.json', report)


def observe(args, gateway):
    root = canonical_path(args.output)
    if root.exists() or args.configuration not in 'ABCD' or len(args.configuration) != 1:
        raise ValueError('fresh output and A/B/C/D label required')
    if not args.allow_mutations or not 1 <= args.boot_timeout <= 600 or not 1 <= args.delete_timeout <= 180 or not .01 <= args.poll <= 5:
        raise ValueError('mutation opt-in and bounded pilot parameters required')
    if any(not math.isfinite(v) for v in (args.boot_timeout, args.delete_timeout, args.poll)):
        raise ValueError('finite bounds required')
    baseline = gateway.servers()
    inputs = gateway.inputs(args)
    campaign = str(uuid.uuid4())
    name = 'tt-integrated-' + campaign
    if any(s['name'] == name for s in baseline):
        raise ValueError('name collision')
    root.mkdir(mode=0o700, parents=False)
    report = {'schema': 1, 'stage': 'pilot', 'configuration_claimed': args.configuration,
              'deployment_identity_verified': False, 'gate_accepted': False, 'official_accepted': False,
              'name': name, 'campaign_id': campaign, 'baseline_ids': [s['id'] for s in baseline],
              'server_id': None, 'inputs': inputs, 'started_utc': utc(), 'states': [],
              'boot_active': False, 'allocation_verified': False, 'cleanup_verified': False,
              'restoration_verified': False, 'service_configuration_changed': False,
              'state': 'CREATE_INTENT', 'execution_error': None, 'cleanup_error': None,
              'instrument_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
              'code_sha': source_revision(Path(__file__).resolve().parents[2], getattr(args, 'code_sha', None)),
              'protocol_sha256': hashlib.sha256((Path(__file__).resolve().parents[2] / 'docs/benchmarks/integrated/protocol-v1.json').read_bytes()).hexdigest(),
              'latency_note': 'POST to first observed ACTIVE; includes Nova/Neutron/hypervisor; polling uncertainty explicit',
              'poll_interval_s': args.poll}
    save(root / 'report.json', report)  # durable intent BEFORE Nova POST
    begin = time.monotonic_ns()
    try:
        report['server_id'] = gateway.create(name, campaign, inputs)
        report['create_response_ms'] = (time.monotonic_ns() - begin) / 1e6
        report['state'] = 'BOOTING'
        save(root / 'report.json', report)
        end = time.monotonic() + args.boot_timeout
        while time.monotonic() < end:
            current = gateway.get(report['server_id'])
            if not owned(current, report):
                raise ValueError('server identity mismatch')
            report['states'].append({'elapsed_ns': time.monotonic_ns() - begin, 'utc': utc(), 'status': current['status']})
            save(root / 'report.json', report)
            if current['status'] == 'ACTIVE':
                report['boot_active'] = True
                report['boot_to_active_ms'] = (time.monotonic_ns() - begin) / 1e6
                break
            if current['status'] == 'ERROR':
                raise RuntimeError('Nova ERROR; private diagnostics required')
            time.sleep(args.poll)
        if not report['boot_active']:
            raise TimeoutError('boot not ACTIVE')
        allocations = gateway.allocations(report['server_id'])
        resources = {}
        for allocation in allocations.values():
            for key, value in allocation['resources'].items():
                if type(value) is not int or value < 0:
                    raise ValueError('invalid allocation quantity')
                resources[key] = resources.get(key, 0) + value
        expected = {k: v for k, v in inputs['resources'].items() if v > 0}
        if not allocations or resources != expected:
            raise ValueError('Nova allocations do not match flavor')
        report['allocation_verified'] = True
        report['allocations'] = allocations
    except Exception as error:
        report['execution_error'] = type(error).__name__
    finally:
        try:
            cleanup(gateway, root, report, args.delete_timeout, args.poll)
        except Exception as error:
            report['cleanup_error'] = type(error).__name__
        report['finished_utc'] = utc()
        report['state'] = 'OBSERVATION_COMPLETE' if report['boot_active'] and report['allocation_verified'] and report['cleanup_verified'] and not report['execution_error'] else 'NEEDS_ATTENTION'
        save(root / 'report.json', report)
        checksums(root)
    return report


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('action', choices=('observe', 'cleanup'))
    p.add_argument('--output', type=Path, required=True)
    p.add_argument('--lock', type=Path, default=Path('/run/lock/tt-placement-integration.lock'))
    p.add_argument('--placement-url', default='http://192.168.122.10/placement')
    p.add_argument('--code-sha')
    p.add_argument('--configuration', choices=list('ABCD'))
    p.add_argument('--image-id')
    p.add_argument('--image-sha512')
    p.add_argument('--flavor-id')
    p.add_argument('--network-id')
    p.add_argument('--boot-timeout', type=float, default=600)
    p.add_argument('--delete-timeout', type=float, default=180)
    p.add_argument('--poll', type=float, default=1)
    p.add_argument('--allow-mutations', action='store_true')
    args = p.parse_args()
    try:
        if not args.allow_mutations:
            raise ValueError('explicit mutation opt-in required')
        # This same VM-local lock is used by B15/B17. Do not replace it with a
        # different host lock and assume exclusion across machines.
        with exclusive_lock(args.lock):
            gateway = SDKGateway(args.placement_url)
            if args.action == 'observe':
                if any(not getattr(args, k) for k in ('configuration', 'image_id', 'image_sha512', 'flavor_id', 'network_id')):
                    raise ValueError('explicit immutable boot inputs required')
                for k in ('image_id', 'network_id'):
                    uuid.UUID(getattr(args, k))
                if not re.fullmatch(r'[A-Za-z0-9_.-]{1,64}', args.flavor_id):
                    raise ValueError('opaque flavor ID required')
                if len(args.image_sha512) != 128 or any(c not in '0123456789abcdef' for c in args.image_sha512):
                    raise ValueError('SHA512 required')
                report = observe(args, gateway)
                return 0 if report['state'] == 'OBSERVATION_COMPLETE' else 1
            root = canonical_path(args.output)
            report = json.loads((root / 'report.json').read_text())
            cleanup(gateway, root, report, args.delete_timeout, args.poll)
            checksums(root)
            return 0
    except Exception as error:
        print('Nova observation failed: ' + type(error).__name__, file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
