#!/usr/bin/env python3
"""Hash-leased A/B/C/D Apache routes with an independent finite restore guardian."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import uuid

from placement_switch import replace_site

SITES = {'glance': (Path('/etc/apache2/sites-enabled/glance-api.conf'), 'devstack@g-api.service', 9293),
         'placement': (Path('/etc/apache2/sites-enabled/placement-api.conf'), 'devstack@placement-api.service', 18779)}
CONFIGS = {'A': (), 'B': ('glance',), 'C': ('placement',), 'D': ('glance', 'placement')}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def go_route(service, original):
    if service == 'glance':
        old = b'http://127.0.0.1:60999'
        if original.count(old) != 1:
            raise ValueError('unexpected original Glance route')
        return original.replace(old, b'http://127.0.0.1:9293')
    if service != 'placement' or b'uwsgi-uds-placement-api' not in original:
        raise ValueError('unexpected original Placement route')
    return b'ProxyPass "/placement" "http://127.0.0.1:18779" retry=0 acquire=1\nProxyPassReverse "/placement" "http://127.0.0.1:18779"\n'


def command(args):
    subprocess.run(args, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=120)


def save(root, state):
    path = root / 'routes.json'
    tmp = root / 'routes.tmp'
    with tmp.open('w') as stream:
        json.dump(state, stream, indent=2)
        stream.flush()
        os.fsync(stream.fileno())
    tmp.chmod(0o600)
    os.replace(tmp, path)


def transition(root, state, config, original_only=False):
    selected = () if original_only else CONFIGS[config]
    sandbox = json.loads((root / 'combined.json').read_text()).get('python_sandbox')
    for service, (file, original_unit, _) in SITES.items():
        item = state['sites'][service]
        original = (root / item['backup']).read_bytes()
        if digest(original) != item['original_sha256']:
            raise ValueError('route backup mismatch')
        current = file.resolve().read_bytes()
        allowed = [item['original_sha256'], item['go_sha256']]
        if service == 'glance' and sandbox:
            allowed.append(digest(original.replace(b'127.0.0.1:60999', b'127.0.0.1:60998')))
        if digest(current) not in allowed:
            raise ValueError('route changed outside this experiment')
    for service, (_, original_unit, _) in SITES.items():
        if service not in selected:
            start = sandbox['unit'] if service == 'glance' and sandbox and not original_only else original_unit
            command(['sudo', 'systemctl', 'start', start])
    for service, (file, _, _) in SITES.items():
        item = state['sites'][service]
        original = (root / item['backup']).read_bytes()
        data = go_route(service, original) if service in selected else original
        if service == 'glance' and sandbox and not original_only and service not in selected:
            data = original.replace(b'127.0.0.1:60999', b'127.0.0.1:60998')
        current = file.resolve().read_bytes()
        if current != data:
            replace_site(file.resolve(), data, digest(current), item['mode'], item['uid'], item['gid'])
    command(['sudo', 'apache2ctl', 'configtest'])
    command(['sudo', 'systemctl', 'reload', 'apache2'])
    for service, (_, original_unit, _) in SITES.items():
        if service in selected or service == 'glance' and sandbox and not original_only:
            command(['sudo', 'systemctl', 'stop', original_unit])
    if sandbox and (original_only or 'glance' in selected):
        command(['sudo', 'systemctl', 'stop', sandbox['unit']])
    state['configuration'] = config
    state['restored'] = original_only or config == 'A' and not sandbox
    state['effective_sha256'] = {service: digest(file.resolve().read_bytes()) for service, (file, _, _) in SITES.items()}
    save(root, state)
    return state


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('action', choices=('switch', 'restore', 'pin-nova'))
    p.add_argument('--root', type=Path, required=True)
    p.add_argument('--configuration', choices=list(CONFIGS), default='A')
    p.add_argument('--allow-mutations', action='store_true')
    p.add_argument('--guardian-seconds', type=int, default=900)
    args = p.parse_args()
    if not 60 <= args.guardian_seconds <= 7200:
        p.error('guardian must be finite, between 60 and 7200 seconds')
    root = args.root
    if not args.allow_mutations or not root.is_absolute() or root != root.resolve() or not root.name.startswith('tt-placement-') or root.stat().st_uid != os.getuid() or root.stat().st_mode & 0o077:
        raise ValueError('owned prepared runtime and opt-in required')
    combined = json.loads((root / 'combined.json').read_text())
    if (root / 'routes.json').exists():
        state = json.loads((root / 'routes.json').read_text())
    else:
        state = {'sites': {}, 'configuration': 'A', 'restored': True}
        for service, (file, _, _) in SITES.items():
            path = file.resolve()
            original = path.read_bytes()
            meta = path.stat()
            backup = 'original-' + service + '-route.conf'
            (root / backup).write_bytes(original)
            (root / backup).chmod(0o600)
            state['sites'][service] = {'backup': backup, 'original_sha256': digest(original),
                                      'go_sha256': digest(go_route(service, original)),
                                      'mode': meta.st_mode & 0o777, 'uid': meta.st_uid, 'gid': meta.st_gid}
        save(root, state)
    if args.action == 'pin-nova':
        if state.get('nova_files'):
            for item in state['nova_files']:
                current = subprocess.check_output(['sudo', 'cat', item['path']])
                if digest(current) not in (item['original_sha256'], item['pinned_sha256']):
                    raise ValueError('Nova endpoint lease changed')
        else:
            state['nova_files'] = []
            for index, name in enumerate(('nova.conf', 'nova-cpu.conf', 'nova_cell1.conf')):
                path = Path('/etc/nova') / name
                # Only the two endpoint options; keep user/service credentials exact.
                original = subprocess.check_output(['sudo', 'cat', str(path)])
                if re.search(rb'(?m)^\[glance\]', original):
                    raise ValueError('existing glance section requires explicit reviewed patch')
                data = original + b'\n[glance]\napi_servers = http://192.168.122.10/image\nendpoint_override = http://192.168.122.10/image\n'
                meta = path.stat()
                backup = 'original-nova-' + str(index) + '.bin'
                (root / backup).write_bytes(original)
                (root / backup).chmod(0o600)
                state['nova_files'].append({'path': str(path), 'backup': backup, 'original_sha256': digest(original),
                                            'pinned_sha256': digest(data), 'uid': meta.st_uid, 'gid': meta.st_gid, 'mode': meta.st_mode & 0o777})
        save(root, state)
        for item in state['nova_files']:
            original = (root / item['backup']).read_bytes()
            data = original + b'\n[glance]\napi_servers = http://192.168.122.10/image\nendpoint_override = http://192.168.122.10/image\n'
            current = subprocess.check_output(['sudo', 'cat', item['path']])
            replace_site(Path(item['path']), data, digest(current), item['mode'], item['uid'], item['gid'])
        command(['sudo', 'systemctl', 'restart', 'devstack@n-api.service', 'devstack@n-cpu.service', 'devstack@n-super-cond.service', 'devstack@n-cond-cell1.service'])
        state['nova_endpoint_pinned'] = True
        save(root, state)
        print(json.dumps({'nova_endpoint_pinned': True, 'same_explicit_glance_endpoint_all_configurations': 'http://192.168.122.10/image'}))
        return
    if args.action == 'restore':
        result = transition(root, state, 'A', original_only=True)
        if state.get('nova_files'):
            for item in state['nova_files']:
                original = (root / item['backup']).read_bytes()
                if digest(original) != item['original_sha256']:
                    raise ValueError('Nova backup checksum mismatch')
                current = subprocess.check_output(['sudo', 'cat', item['path']])
                if digest(current) not in (item['original_sha256'], item['pinned_sha256']):
                    raise ValueError('Nova configuration changed outside experiment')
                replace_site(Path(item['path']), original, digest(current), item['mode'], item['uid'], item['gid'])
            command(['sudo', 'systemctl', 'restart', 'devstack@n-api.service', 'devstack@n-cpu.service', 'devstack@n-super-cond.service', 'devstack@n-cond-cell1.service'])
            result['nova_endpoint_pinned'] = False
            result['nova_restored'] = True
            save(root, result)
    else:
        command(['systemctl', 'is-active', '--quiet', combined['profile_unit']])
        if args.configuration != 'A':
            for service in CONFIGS[args.configuration]:
                unit = combined['glance_unit'] if service == 'glance' else json.loads((root / 'state.json').read_text())['go_unit']
                command(['systemctl', 'is-active', '--quiet', unit])
            guardian_active = state.get('guardian') and subprocess.run(['systemctl', 'is-active', '--quiet', state['guardian'] + '.service'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
            if not guardian_active:
                guardian = 'tt-api-restore-' + uuid.uuid4().hex[:10]
                state['guardian'] = guardian
                save(root, state)
                restore = sys.executable + ' ' + str(Path(__file__).resolve()) + ' restore --root ' + str(root) + ' --allow-mutations'
                command(['sudo', 'systemd-run', '--unit=' + guardian, '--uid=' + str(os.getuid()),
                         '--property=RuntimeMaxSec=' + str(args.guardian_seconds), '--property=TimeoutStopSec=180',
                         '--property=PartOf=' + combined['profile_unit'], '--property=After=' + combined['profile_unit'],
                         '--property=ExecStopPost=' + restore, '/usr/bin/sleep', str(args.guardian_seconds)])
        try:
            result = transition(root, state, args.configuration)
        except Exception:
            transition(root, state, 'A', original_only=True)
            raise
    print(json.dumps({'configuration': result['configuration'], 'restored': result['restored'],
                      'effective_sha256': result['effective_sha256']}))


if __name__ == '__main__':
    main()
