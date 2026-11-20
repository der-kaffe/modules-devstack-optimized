#!/usr/bin/env python3
"""Finite B15 lease handover for verification and orderly shutdown only."""
import argparse
import json
import os
import subprocess
from pathlib import Path
import signal
import threading
import time
import uuid

from lab_capacity_profile import lease, VM_UUID

LOCK_PATH = Path('/run/lock/tt-placement-integration.lock')


def verify_held(root, unit):
    """A stale HELD marker is insufficient: verify the kernel flock holder."""
    if not unit.startswith('tt-restoration-lease-') or '/' in unit:
        raise ValueError('own restoration unit required')
    pid = int(subprocess.check_output(['systemctl', 'show', unit+'.service', '--property=MainPID', '--value'], text=True).strip())
    if pid <= 1:
        raise ValueError('restoration lease process absent')
    argv = Path('/proc') / str(pid) / 'cmdline'
    if str(root) not in argv.read_bytes().decode().split('\0'):
        raise ValueError('restoration process root mismatch')
    info = LOCK_PATH.stat()
    expected = (os.major(info.st_dev), os.minor(info.st_dev), info.st_ino)
    held = False
    for row in Path('/proc/locks').read_text().splitlines():
        fields = row.split()
        if len(fields) < 8 or fields[1:4] != ['FLOCK', 'ADVISORY', 'WRITE']:
            continue
        major, minor, inode = fields[5].split(':')
        if int(fields[4]) == pid and (int(major, 16), int(minor, 16), int(inode)) == expected:
            held = True
    if not held:
        raise ValueError('restoration process does not hold the current B15 inode')
    state = json.loads((root/'state.json').read_text())
    if state.get('phase') != 'HELD':
        raise ValueError('restoration lease expired')
    return {'verified': True, 'pid': pid, 'inode': info.st_ino, 'state': state}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',type=Path,required=True)
    parser.add_argument('--maximum-seconds',type=int,default=600)
    parser.add_argument('--verify-held', action='store_true')
    parser.add_argument('--unit')
    args=parser.parse_args()
    root=args.root
    if (Path('/sys/class/dmi/id/product_uuid').read_text().strip().lower()!=VM_UUID
            or root.parent!=Path('/var/tmp') or not root.name.startswith('tt-restoration-lease-')
            or os.geteuid()!=0 or root!=root.resolve()
            or not 60<=args.maximum_seconds<=600 or (root.exists() and not args.verify_held)):
        raise ValueError('identified lab and fresh finite restoration lease required')
    uuid.UUID(root.name.removeprefix('tt-restoration-lease-'))
    if args.verify_held:
        print(json.dumps(verify_held(root, args.unit or '')))
        return 0
    root.mkdir(mode=0o700)
    stop=threading.Event()
    for sig in (signal.SIGTERM,signal.SIGINT,signal.SIGHUP):signal.signal(sig,lambda *_:stop.set())
    def save(phase):
        p=root/'state.json';p.write_text(json.dumps({'phase':phase,'utc_ns':time.time_ns()})+'\n');p.chmod(0o600)
    deadline=time.monotonic()+args.maximum_seconds
    save('WAITING')
    while not stop.is_set() and time.monotonic()<deadline:
        try:
            with lease():
                save('HELD')
                while not stop.wait(1) and time.monotonic()<deadline:pass
                save('RELEASED')
                return 0
        except BlockingIOError:
            stop.wait(.5)
    save('NOT_ACQUIRED')
    return 1


if __name__=='__main__':
    raise SystemExit(main())
