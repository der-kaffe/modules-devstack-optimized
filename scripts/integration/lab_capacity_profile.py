#!/usr/bin/env python3
"""Reversible single-worker profile for the identified thesis VM only.

Run as root in a finite systemd unit with ExecStopPost=... restore. Keeps the B15
lock for the whole session. Never changes DB contents, credentials or routes.
"""
import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import signal
import stat
import subprocess
import time

VM_UUID = '385591e9-02ea-4209-b09e-49f3f882a8e6'
FILES = {
    '/etc/glance/glance-uwsgi.ini': 'devstack@g-api.service',
    '/etc/placement/placement-uwsgi.ini': 'devstack@placement-api.service',
    '/etc/keystone/keystone-uwsgi-public.ini': 'devstack@keystone.service',
    '/etc/nova/nova-api-uwsgi.ini': 'devstack@n-api.service',
    '/etc/nova/nova-metadata-uwsgi.ini': 'devstack@n-api-meta.service',
    '/etc/neutron/neutron-api-uwsgi.ini': 'devstack@neutron-api.service',
    '/etc/cinder/cinder-api-uwsgi.ini': 'devstack@c-api.service',
    '/etc/neutron/plugins/ml2/ovn_agent.ini': 'devstack@q-ovn-agent.service',
}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def single_worker(data, key):
    """Change one existing numeric key; preserve every other byte and section."""
    regex = re.compile(rb'(?m)^(' + key.encode() + rb'\s*=\s*)([0-9]+)([^\r\n]*)$')
    matches = list(regex.finditer(data))
    if len(matches) != 1 or int(matches[0][2]) not in (1, 2):
        raise ValueError('expected one existing worker count of 1 or 2')
    return regex.sub(lambda m: m[1] + b'1' + m[3], data)


def save(root, state):
    target = root / 'state.json'
    tmp = root / 'state.tmp'
    with tmp.open('w') as stream:
        json.dump(state, stream, indent=2)
        stream.write('\n')
        stream.flush()
        os.fsync(stream.fileno())
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


def validate_root(path):
    if not path.is_absolute() or path != path.resolve() or not re.fullmatch(r'tt-capacity-[0-9a-f-]{36}', path.name):
        raise ValueError('new canonical owned capacity root required')
    if os.geteuid() != 0 or Path('/sys/class/dmi/id/product_uuid').read_text().strip().lower() != VM_UUID:
        raise ValueError('root on identified lab VM required')


@contextmanager
def lease():
    stack_uid = pwd.getpwnam('stack').pw_uid
    path = '/run/lock/tt-placement-integration.lock'
    try:
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        os.fchown(fd, stack_uid, pwd.getpwnam('stack').pw_gid)
    except FileExistsError:
        fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != stack_uid or info.st_mode & 0o077:
            raise ValueError('B15 lock ownership/mode mismatch')
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        os.close(fd)


def systemctl(*args):
    subprocess.run(['systemctl', *args], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=120)


def active(unit):
    return subprocess.run(['systemctl', 'is-active', '--quiet', unit], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0


def replace(path, data, expected_hashes, metadata):
    path = Path(path)
    if digest(path.read_bytes()) not in expected_hashes or path.is_symlink():
        raise ValueError('configuration lease changed; do not overwrite')
    tmp = path.with_name('.tt-capacity-' + path.name)
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
            os.fchmod(stream.fileno(), metadata['mode'])
            os.fchown(stream.fileno(), metadata['uid'], metadata['gid'])
        os.replace(tmp, path)
        fd_dir = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd_dir)
        finally:
            os.close(fd_dir)
    finally:
        if tmp.exists():
            tmp.unlink()  # only our O_EXCL temporary file


def restore(root, state):
    errors = []
    for item in state['files']:
        try:
            original = (root / item['backup']).read_bytes()
            if digest(original) != item['original_sha256']:
                raise ValueError('backup hash mismatch')
            replace(item['path'], original, {item['original_sha256'], item['profile_sha256']}, item)
        except Exception as error:
            errors.append({'file': item['path'], 'error': type(error).__name__})
    if not errors:
        for unit in state['units']:
            try:
                systemctl('restart', unit)
            except Exception as error:
                errors.append({'unit': unit, 'error': type(error).__name__})
    state['restored'] = not errors and all(digest(Path(r['path']).read_bytes()) == r['original_sha256'] for r in state['files']) and all(active(u) for u in state['units'])
    state['restore_errors'] = errors
    state['phase'] = 'RESTORED' if state['restored'] else 'NEEDS_ATTENTION'
    state['finished_utc_ns'] = time.time_ns()
    save(root, state)
    return state['restored']


def session(root, maximum, await_preflight=False):
    validate_root(root)
    if not 60 <= maximum <= 7200 or root.exists():
        raise ValueError('finite session and fresh directory required')
    with lease():
        # Validate all inputs before creating backups or editing files.
        plan = []
        for filename, unit in FILES.items():
            path = Path(filename)
            if path.is_symlink() or not path.is_file() or not active(unit):
                raise ValueError('expected original file/service absent')
            original = path.read_bytes()
            new = single_worker(original, 'metadata_workers' if 'ovn_agent' in filename else 'processes')
            meta = path.stat()
            plan.append((path, unit, original, new, meta))
        root.mkdir(mode=0o700)
        state = {'phase': 'PREPARING', 'started_utc_ns': time.time_ns(), 'maximum_s': maximum,
                 'files': [], 'units': list(dict.fromkeys(FILES.values())), 'restored': False}
        for index, (path, unit, original, new, meta) in enumerate(plan):
            backup = f'original-{index:02d}.bin'
            (root / backup).write_bytes(original)
            (root / backup).chmod(0o600)
            state['files'].append({'path': str(path), 'backup': backup, 'unit': unit,
                                   'original_sha256': digest(original), 'profile_sha256': digest(new),
                                   'mode': stat.S_IMODE(meta.st_mode), 'uid': meta.st_uid, 'gid': meta.st_gid})
        save(root, state)
        stopped = [False]
        for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            signal.signal(sig, lambda signum, frame: stopped.__setitem__(0, True))
        try:
            if await_preflight:
                state['phase'] = 'WAITING_PREFLIGHT'
                save(root, state)
                until = time.monotonic() + 120
                while not stopped[0] and not (root / 'apply-approved').exists() and time.monotonic() < until:
                    time.sleep(.2)
                if stopped[0] or not (root / 'apply-approved').exists():
                    raise RuntimeError('read-only preflight not released')
            for item, (_, _, _, new, _) in zip(state['files'], plan):
                replace(item['path'], new, {item['original_sha256']}, item)
            state['phase'] = 'APPLIED_RESTARTING'
            save(root, state)
            for unit in state['units']:
                systemctl('restart', unit)
            if not all(active(u) for u in state['units']):
                raise RuntimeError('profile service not active')
            state['phase'] = 'ACTIVE'
            save(root, state)
            deadline = time.monotonic() + maximum
            while not stopped[0] and not (root / 'stop').exists() and time.monotonic() < deadline:
                time.sleep(1)
        except Exception as error:
            state['error'] = type(error).__name__
        finally:
            restore(root, state)
        return 0 if state['restored'] else 1


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('action', choices=('session', 'restore'))
    p.add_argument('--root', type=Path, required=True)
    p.add_argument('--maximum-seconds', type=int, default=3300)
    p.add_argument('--allow-mutations', action='store_true')
    p.add_argument('--await-preflight', action='store_true')
    args = p.parse_args()
    if not args.allow_mutations:
        p.error('explicit mutation opt-in required')
    if args.action == 'session':
        return session(args.root, args.maximum_seconds, args.await_preflight)
    validate_root(args.root)
    with lease():
        state = json.loads((args.root / 'state.json').read_text())
        if state.get('restored') and all(digest(Path(r['path']).read_bytes()) == r['original_sha256'] for r in state['files']):
            return 0
        return 0 if restore(args.root, state) else 1


if __name__ == '__main__':
    raise SystemExit(main())
