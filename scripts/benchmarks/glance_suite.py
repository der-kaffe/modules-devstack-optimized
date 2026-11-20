#!/usr/bin/env python3
"""Finite sequential Glance cases; continue failures only after verified OFF.

Each case gets a complete laboratory session. Publication is local only.
The child owns the infrastructure lock and restoration; this parent never
changes routes, services, databases, VM power or Git.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

from glance_benchmark import exclusive_lock


def restoration_proof(folder):
    path = folder/'session-01/restoration/restoration-verified.json'
    if not path.exists():
        path = folder/'session-01/preflight-restored.json'
    proof = json.loads(path.read_text())
    if proof.get('restored') is not True or proof.get('vm_state') != 'shut off' or proof.get('errors', []):
        raise ValueError('restoration not verified; stop all subsequent mutations')
    if path.name == 'restoration-verified.json' and not proof.get('evidence'):
        raise ValueError('restoration evidence missing')
    if path.name == 'preflight-restored.json' and proof.get('guest_service_mutations_started') is not False:
        raise ValueError('preflight mutation status unknown')
    for item in proof.get('evidence', []):
        if hashlib.sha256(Path(item['path']).read_bytes()).hexdigest() != item['sha256']:
            raise ValueError('restoration evidence changed')
    return str(path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--allow-mutations', action='store_true')
    args = parser.parse_args()
    if not args.allow_mutations:
        parser.error('explicit opt-in required')
    config = json.loads(args.config.read_text())
    root = Path(config['output'])
    base = config['base_config']
    protocol = json.loads(Path(base['protocol']).read_text())
    cases = config['cases']
    if root.exists() or root != root.resolve() or not root.is_absolute():
        raise ValueError('fresh canonical output required')
    if not 600 <= config['maximum_seconds'] <= 36000 or not cases or len(set(map(tuple, cases))) != len(cases) or any(c not in protocol['cases'] for c in cases):
        raise ValueError('finite, unique, predeclared protocol cases required')
    root.mkdir(mode=0o700)
    deadline = time.monotonic()+config['maximum_seconds']
    stopping = [False]
    child = [None]
    def stop(*_):
        stopping[0] = True
        if child[0] is not None and child[0].poll() is None:
            child[0].send_signal(signal.SIGTERM)
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, stop)
    inhibitor = subprocess.Popen(['systemd-inhibit','--what=sleep:idle','--mode=block','--who=tt-glance-suite',
        '--why=Finite sequential Glance campaign','sleep',str(config['maximum_seconds']+900)],
        stdout=subprocess.DEVNULL,stderr=(root/'inhibitor.log').open('w'))
    results = []
    error = None
    def save(phase, **extra):
        row = {'utc_ns':time.time_ns(), 'phase':phase, 'results':results, **extra}
        temporary = root/'state.tmp'
        temporary.write_text(json.dumps(row, indent=2)+'\n')
        temporary.replace(root/'state.json')
        with (root/'events.jsonl').open('a') as stream:
            stream.write(json.dumps(row)+'\n')
    def immutable_sources():
        source = Path(base['source_root'])
        manifest = json.loads(Path(config['source_manifest']).read_text())
        for relative, digest in manifest.items():
            path = source/relative
            if path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != digest:
                raise ValueError('frozen source bundle changed')
    try:
        with exclusive_lock(Path('/tmp/tt-glance-suite.lock')):
            for index, case in enumerate(cases, 1):
                if stopping[0]:
                    break
                if inhibitor.poll() is not None:
                    raise RuntimeError('suite sleep inhibitor lost')
                immutable_sources()
                remaining = deadline-time.monotonic()
                required = protocol['repetitions']*2*(protocol['warmup_s']+protocol['measurement_s']+8)+900
                if remaining < required:
                    raise RuntimeError('finite suite time cannot fit a full case and restoration')
                folder = root/f'case-{index:02d}'
                run = dict(base, output=str(folder), cases=[case], cases_per_session=1,
                           maximum_seconds=min(3600, int(remaining)))
                run_file = root/f'case-{index:02d}-config.json'
                run_file.write_text(json.dumps(run, indent=2)+'\n')
                save('RUNNING', case=case, folder=str(folder))
                with (root/f'case-{index:02d}.log').open('x') as log:
                    child[0] = subprocess.Popen([sys.executable, '-u', str(Path(base['source_root'])/'scripts/benchmarks/glance_lab_campaign.py'),
                                                '--config', str(run_file), '--allow-mutations'], stdout=log, stderr=subprocess.STDOUT)
                    (root/'child.json').write_text(json.dumps({'pid':child[0].pid, 'case':case, 'folder':str(folder)})+'\n')
                    abort_at = None
                    while child[0].poll() is None:
                        if time.monotonic() >= deadline:
                            stop()
                        if stopping[0] and abort_at is None:
                            abort_at = time.monotonic()
                        if abort_at is not None and time.monotonic()-abort_at > 900:
                            raise RuntimeError('NEEDS_ATTENTION: child restoration exceeded grace; no next case')
                        time.sleep(3)
                    exit_code = child[0].returncode
                    child[0] = None
                report = json.loads((folder/'job-report.json').read_text())
                # OFF must be current, not merely asserted in an old report.
                status = subprocess.check_output(['virsh','-c','qemu:///system','domstate','tt-openstack-lab'], text=True, timeout=20, env=dict(os.environ,LC_ALL='C')).strip()
                if status != 'shut off' or report.get('vm_state') != 'shut off':
                    raise RuntimeError('NEEDS_ATTENTION: VM not verified OFF; no next case')
                proof = restoration_proof(folder)
                accepted = False
                if exit_code == 0 and report.get('complete'):
                    completed = report['cases_completed']
                    accepted = len(completed) == 1
                    if base['stage'] == 'official':
                        accepted = accepted and json.loads(Path(completed[0]['acceptance']).read_text()).get('selected_block_accepted') is True
                results.append({'case':case, 'accepted':accepted, 'stage':base['stage'], 'exit':exit_code,
                                'report':str(folder/'job-report.json'), 'restoration':proof, 'error':report.get('error')})
                save('CASE_FINISHED', case=case, accepted=accepted)
                # A failed case stays failed; unrelated cases run on a freshly
                # restored lab. No automatic removal/retry of failed evidence.
    except Exception as exc:
        error = type(exc).__name__+': '+str(exc)
        stop()
    finally:
        if child[0] is not None and child[0].poll() is None:
            stop()
            try:
                child[0].wait(timeout=900)
            except subprocess.TimeoutExpired:
                error = 'NEEDS_ATTENTION: child remains active after finite restoration grace'
        inhibitor.terminate()
        try:
            inhibitor.wait(timeout=10)
        except subprocess.TimeoutExpired:
            inhibitor.kill(); inhibitor.wait(timeout=5)
    complete = not error and not stopping[0] and len(results) == len(cases) and all(r['accepted'] for r in results)
    save('COMPLETE' if complete else 'INCOMPLETE', error=error, requested_cases=cases)
    print(json.dumps({'complete':complete, 'cases_finished':len(results), 'accepted':sum(r['accepted'] for r in results), 'error':error}), flush=True)
    return 0 if complete else 1


if __name__ == '__main__':
    sys.exit(main())
