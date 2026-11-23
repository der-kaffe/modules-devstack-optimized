#!/usr/bin/env python3
"""Reversible /image substitution; no Nova or original database changes."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import time
from urllib.error import HTTPError
from urllib.request import urlopen

from placement_switch import active, command, digest, replace_site, stop_recovery_timer

SITE_FILE = Path('/etc/apache2/sites-enabled/glance-api.conf')
ORIGINAL_UNIT = 'devstack@g-api.service'


def replacement(port):
    if type(port) is not int or not 1024 <= port <= 65535:
        raise ValueError('controlled loopback backend port required')
    # The same Apache settings as the original's site (KeepAlive Off, chunked
    # requests to the backend): without them Go behind Apache was the only
    # route that reused client connections, and a 16-client burst could hit a
    # connection Apache then closed (RemoteDisconnected on the first POST).
    return ('KeepAlive Off\n'
            'SetEnv proxy-sendchunked 1\n'
            f'ProxyPass "/image" "http://127.0.0.1:{port}" retry=0 acquire=1\n'
            f'ProxyPassReverse "/image" "http://127.0.0.1:{port}"\n').encode()


def wait_auth(url):
    for _ in range(50):
        try:
            with urlopen(url.rstrip('/') + '/v2/images', timeout=2):
                pass
        except HTTPError as error:
            try:
                if error.code == 401:
                    return
            finally:
                error.close()
        except OSError:
            pass
        time.sleep(.2)
    raise RuntimeError('Glance protected endpoint not ready')


def run(args):
    if not args.allow_mutations:
        raise ValueError('explicit mutation opt-in required')
    root = args.root
    if (not root.is_absolute() or root != root.resolve() or root.stat().st_uid != os.getuid()
            or root.stat().st_mode & 0o077 or not re.fullmatch(r'tt-placement-[a-z0-9-]+', root.name)):
        raise ValueError('canonical private owned runtime required')
    runtime = json.loads((root / 'state.json').read_text())
    if runtime.get('domain') != 'glance' or runtime.get('owner_uid') != os.getuid() or not runtime.get('prepared'):
        raise ValueError('prepared Glance runtime required')
    site = SITE_FILE.resolve()
    state_path = root / 'glance-switch-state.json'
    def save(state):
        state_path.write_text(json.dumps(state))
        state_path.chmod(0o600)
    def restore(state):
        original = (root / 'original-glance-apache.conf').read_bytes()
        current = site.read_bytes()
        if digest(original) != state['original_sha'] or digest(current) not in (state['original_sha'], state['go_sha']):
            raise ValueError('Glance backup/site changed outside lease')
        command(['sudo', 'systemctl', 'start', ORIGINAL_UNIT])
        if digest(current) != state['original_sha']:
            replace_site(site, original, digest(current), state['mode'], state['uid'], state['gid'])
        command(['sudo', 'apache2ctl', 'configtest'])
        command(['sudo', 'systemctl', 'reload', 'apache2'])
        if not state['proxy_http_was_enabled']:
            command(['sudo', 'a2dismod', 'proxy_http'])
            command(['sudo', 'apache2ctl', 'configtest'])
            command(['sudo', 'systemctl', 'reload', 'apache2'])
        wait_auth(args.public_url)
        if not active(ORIGINAL_UNIT) or digest(site.read_bytes()) != state['original_sha']:
            raise RuntimeError('original Glance restoration failed')
        state['active'] = False
        save(state)
        if state.get('rollback_unit'):
            stop_recovery_timer(state['rollback_unit'])
        return {'original_restored': True, 'original_site_sha256': state['original_sha'], 'original_unit_active': True}
    if args.action == 'restore':
        print(json.dumps(restore(json.loads(state_path.read_text()))))
        return 0
    current = site.read_bytes()
    if digest(current) != args.expected_original_sha or not 5 <= args.lease_seconds <= 1800:
        raise ValueError('original digest and bounded recovery lease required')
    if not active(ORIGINAL_UNIT) or not active(runtime['go_unit']):
        raise ValueError('original and own Go service must be ready before substitution')
    wait_auth(args.public_url)
    wait_auth('http://127.0.0.1:' + str(runtime['listen_port']))
    target = replacement(runtime['listen_port'])
    attrs = site.stat()
    modules = subprocess.check_output(['sudo', 'apache2ctl', '-M'], stderr=subprocess.DEVNULL, text=True)
    state = {'original_sha': digest(current), 'go_sha': digest(target), 'mode': attrs.st_mode & 0o777,
             'uid': attrs.st_uid, 'gid': attrs.st_gid, 'proxy_http_was_enabled': 'proxy_http_module' in modules,
             'active': False}
    backup = root / 'original-glance-apache.conf'
    if backup.exists() and digest(backup.read_bytes()) != state['original_sha']:
        raise ValueError('existing Glance backup digest mismatch')
    if not backup.exists():
        backup.write_bytes(current); backup.chmod(0o600)
    state['rollback_unit'] = 'tt-placement-b13-rollback-' + secrets.token_hex(5)
    save(state)
    try:
        command(['sudo', 'systemd-run', '--unit=' + state['rollback_unit'], '--uid=' + str(os.getuid()),
                 '--collect', '--on-active=' + str(args.lease_seconds) + 's', '--timer-property=AccuracySec=1s',
                 sys.executable, str(Path(__file__).resolve()), 'restore', '--root', str(root),
                 '--public-url', args.public_url, '--allow-mutations'])
        if not state['proxy_http_was_enabled']:
            command(['sudo', 'a2enmod', 'proxy_http'])
        replace_site(site, target, state['original_sha'], state['mode'], state['uid'], state['gid'])
        command(['sudo', 'apache2ctl', 'configtest'])
        command(['sudo', 'systemctl', 'reload', 'apache2'])
        command(['sudo', 'systemctl', 'stop', ORIGINAL_UNIT])
        wait_auth(args.public_url)
        if active(ORIGINAL_UNIT):
            raise RuntimeError('original Glance still active under Go load')
        state['active'] = True
        save(state)
    except Exception:
        restore(state)
        raise
    print(json.dumps({'go_route_active': True, 'original_unit_stopped': True, 'site_sha256': digest(site.read_bytes()), 'lease_s': args.lease_seconds}))
    return 0


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['activate', 'restore'])
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--public-url', default='http://192.168.122.10/image')
    parser.add_argument('--expected-original-sha')
    parser.add_argument('--lease-seconds', type=int, default=600)
    parser.add_argument('--allow-mutations', action='store_true')
    sys.exit(run(parser.parse_args()))
