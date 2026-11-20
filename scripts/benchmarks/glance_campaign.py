#!/usr/bin/env python3
"""Finite local Glance coordinator; never publishes or mutates Git.

Requires an already identified, leased laboratory and a frozen entry receipt.
Routes restore in finally and independently through the guest systemd guardian.
The external lab supervisor must restore workers and shut down only after proof.
"""
import argparse
import hashlib
import json
import os
import resource
from pathlib import Path
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import uuid

import glance_benchmark as g
from experiment_analysis import glance_run, host_capacity_summary


def run(config):
    if config.get('max_address_space_mib'):
        cap = int(config['max_address_space_mib']) * 1024**2
        resource.setrlimit(resource.RLIMIT_AS, (cap, cap))
    out = Path(config['output'])
    out.mkdir(mode=0o700)
    private = out / 'private'
    private.mkdir(mode=0o700)
    protocol = json.loads(Path(config['protocol']).read_text())
    campaign = uuid.uuid4()
    stop = threading.Event()
    results, failures = [], []
    deadline = min(config['deadline_utc_s'], time.time() + 2800)
    expected = len(config['cases']) * protocol['repetitions'] * 2
    if time.time() + expected * (protocol['warmup_s'] + protocol['measurement_s'] + 4) + 30 > deadline:
        raise ValueError('insufficient finite supervisor time for this complete block')
    control_root = Path(tempfile.mkdtemp(prefix='tt-bench-ssh-'))
    control = control_root / 'ctl'
    base = config['ssh']
    subprocess.run(base[:-1] + ['-M', '-o', 'ControlPersist=180', '-S', str(control), '-fnNT', base[-1]], check=True, timeout=20)
    ssh = base[:-1] + ['-S', str(control), base[-1]]

    def call(args):
        p = subprocess.run(ssh + [shlex.join(args)], capture_output=True, text=True, timeout=120)
        if p.returncode:
            raise RuntimeError('remote operation failed; preserve supervisor evidence')
        return p.stdout

    def route(implementation, final=False):
        return json.loads(call(['/opt/stack/data/venv/bin/python', config['remote_code'] + '/api_routes.py',
                                'restore' if final else 'switch', '--root', config['runtime'],
                                '--configuration', 'A' if implementation == 'python' else 'B',
                                '--guardian-seconds', str(config.get('guardian_seconds', 2700)), '--allow-mutations']))

    def clock():
        probes = []
        for _ in range(5):
            before = time.time_ns()
            remote = int(call(['date', '+%s%N']).strip())
            after = time.time_ns()
            probes.append({'rtt_ns': after-before, 'remote_minus_generator_ns': remote-(before+after)//2})
        return min(probes, key=lambda p: p['rtt_ns'])

    remote_stop = config['remote_code'] + '/collector-stop-' + campaign.hex
    collector = """import json,time,pathlib,sys,os
sys.path.insert(0,REMOTE)
from placement_resources import collect
for index in range(2800):
 r=collect(GROUPS);d=os.statvfs(STORE);r['store_available_bytes']=d.f_bavail*d.f_frsize;v={x.split()[0]:int(x.split()[1]) for x in pathlib.Path('/proc/vmstat').read_text().splitlines()};r['swap_counters']={k:v[k] for k in ['pswpin','pswpout','pgmajfault','oom_kill']};r['pressure']=pathlib.Path('/proc/pressure/memory').read_text();print(json.dumps(r),flush=True)
 if pathlib.Path(STOP).exists():break
 time.sleep(1)
""".replace('REMOTE', repr(config['remote_code'] + '/scripts/benchmarks')).replace('GROUPS', repr(config['resource_groups'])).replace('STOP', repr(remote_stop)).replace('STORE', repr(config['runtime'] + '/glance-store'))
    resource_path = out / 'resources-all.jsonl'
    resources = resource_path.open('x')
    stderr = (private / 'collector-stderr.log').open('x')
    process = subprocess.Popen(ssh + ['sudo -n python3 -'], stdin=subprocess.PIPE, stdout=resources, stderr=stderr, text=True)
    process.stdin.write(collector)
    process.stdin.close()
    finished = threading.Event()
    watchdog_failures = []

    def monitor():
        previous = None
        host_recent = []
        while not finished.wait(3):
            try:
                available = next(int(s.split()[1])*1024 for s in Path('/proc/meminfo').read_text().splitlines() if s.startswith('MemAvailable:'))
                host_vm = {s.split()[0]: int(s.split()[1]) for s in Path('/proc/vmstat').read_text().splitlines()}
                host_record = {'utc_ns':time.time_ns(), 'mem_available_bytes':available,
                    'pressure_memory':Path('/proc/pressure/memory').read_text(),
                    'pswpin':host_vm['pswpin'], 'pswpout':host_vm['pswpout'], 'oom_kill':host_vm['oom_kill'],
                    'page_size_bytes':os.sysconf('SC_PAGE_SIZE'),
                    'host_cpu_ticks':[int(value) for value in Path('/proc/stat').read_text().splitlines()[0].split()[1:]],
                    'host_loadavg':Path('/proc/loadavg').read_text().strip(),
                    'pressure_cpu':Path('/proc/pressure/cpu').read_text(),
                    'pressure_io':Path('/proc/pressure/io').read_text(),
                    'generator_and_orchestrator_cpu_s':resource.getrusage(resource.RUSAGE_SELF).ru_utime + resource.getrusage(resource.RUSAGE_SELF).ru_stime}
                with (out / 'host-controls.jsonl').open('a') as host_log:
                    host_log.write(json.dumps(host_record)+'\n')
                host_recent.append(host_record)
                host_recent[:] = host_recent[-4:]
                if protocol.get('host_capacity_policy') and len(host_recent) >= 4:
                    host_capacity_summary(host_recent,host_recent[0]['utc_ns'],host_recent[-1]['utc_ns'],protocol['host_capacity_policy'])
                if process.poll() is not None:
                    raise RuntimeError('resource collector exited; private diagnostics retained')
                lines = resource_path.read_text().splitlines()
                row = json.loads(lines[-1]) if lines else None
                if shutil.disk_usage(out).free < 1024**3:
                    raise RuntimeError('host evidence disk reserve')
                if time.time() >= deadline or available < 768 * 1024**2:
                    raise RuntimeError('host capacity or finite deadline')
                if row:
                    if time.time_ns() - row['utc_ns'] > 15_000_000_000:
                        raise RuntimeError('resource collector stale')
                    if row['store_available_bytes'] < 512*1024**2:
                        raise RuntimeError('guest store disk reserve')
                    # collect() exposes MemAvailable in KiB; do not infer RAM from swap.
                    if row['vm_mem_available_kib'] < 512*1024:
                        raise RuntimeError('guest memory reserve')
                    if previous and row['swap_counters']['oom_kill'] > previous['swap_counters']['oom_kill']:
                        raise RuntimeError('guest OOM')
                    if previous and row['swap_counters']['pswpout'] - previous['swap_counters']['pswpout'] > 256:
                        raise RuntimeError('guest swap-out during load')
                    previous = row
            except (json.JSONDecodeError, IndexError):
                continue  # incomplete final append; next tick must be fresh
            except Exception as exc:
                watchdog_failures.append(str(exc))
                stop.set()
                return
    watcher = threading.Thread(target=monitor, daemon=True)
    watcher.start()
    old_handlers = {s: signal.getsignal(s) for s in (signal.SIGINT, signal.SIGTERM)}
    for sig in old_handlers:
        signal.signal(sig, lambda *_: stop.set())
    restored = None
    try:
        for _ in range(15):
            if process.poll() is not None:
                raise RuntimeError('resource collector startup failed; private diagnostics retained')
            if len(resource_path.read_text().splitlines()) >= 3:
                break
            time.sleep(1)
        else:
            raise RuntimeError('resource collector readiness timeout')
        for repetition in range(1, protocol['repetitions'] + 1):
            for scenario, concurrency, size in config['cases']:
                key = f'{scenario}-c{concurrency}-b{size or 0}-r{repetition}'
                pair = str(uuid.uuid5(campaign, key))
                order = ['python', 'go'] if repetition % 2 else ['go', 'python']
                for implementation in order:
                    if stop.is_set():
                        raise RuntimeError('supervised abort')
                    proof = route(implementation)
                    time.sleep(3)
                    before = clock()
                    target = out / (key + '-' + implementation)
                    args = argparse.Namespace(url=config['url'], implementation=implementation,
                        token_file=Path(config['token_file']), output=target, lock=Path(config['operation_lock']),
                        campaign_id=pair, scenario=scenario, stage=config.get('stage', 'official'), concurrency=concurrency,
                        duration=protocol['measurement_s'], warmup=protocol['warmup_s'], timeout=30., max_requests=100000,
                        max_payload_bytes=64*1024**2, payload=Path(config['payloads'][str(size)]) if size else None,
                        protocol=Path(config['protocol']), entry_evidence=Path(config['entry_evidence']),
                        environment=Path(config['environment']) if config.get('environment') else None,
                        code_sha=config['code_sha'], allow_mutations=True, stop_event=stop)
                    code = g.run(args)
                    after = clock()
                    time.sleep(2)
                    rows = [json.loads(l) for l in resource_path.read_text().splitlines()]
                    m = json.loads((target / 'manifest.json').read_text())
                    offset = (before['remote_minus_generator_ns'] + after['remote_minus_generator_ns'])//2
                    uncertainty = max(before['rtt_ns'], after['rtt_ns'])//2 + abs(before['remote_minus_generator_ns']-after['remote_minus_generator_ns'])
                    start, end = m.get('measurement_start_utc_ns'), m.get('measurement_end_utc_ns')
                    if not start or not end:
                        raise RuntimeError('measurement interval missing')
                    selected = [r for r in rows if start+offset-7_000_000_000 <= r['utc_ns'] <= end+offset+2_000_000_000]
                    (target / 'resources.jsonl').write_text(''.join(json.dumps(r, separators=(',', ':'))+'\n' for r in selected))
                    (target / 'clock-alignment.json').write_text(json.dumps({'verified': uncertainty<100_000_000,
                        'uncertainty_ns': uncertainty, 'remote_minus_generator_ns': offset, 'service_vcpus':4,
                        'measurement_probes': {'before': before, 'after': after}}, indent=2)+'\n')
                    g.checksums(target)
                    analysis = glance_run(target)
                    item = {'path':str(target), 'repetition':repetition, 'scenario':scenario, 'implementation':implementation,
                            'exit_code':code, 'route_proof':proof, 'validation':analysis}
                    results.append(item)
                    (out / 'progress.json').write_text(json.dumps({'expected_windows':expected,'completed':len(results), 'runs':results}, indent=2))
                    print(json.dumps({'completed':len(results),'expected':expected,'path':str(target),'valid':analysis['valid']}), flush=True)
                    if code or not analysis['valid'] or not analysis['resources']:
                        raise RuntimeError('window validation rejected; retain entire block')
    except Exception as exc:
        failures.append(type(exc).__name__ + ': ' + str(exc))
    finally:
        try:
            restored = route('python', True)
        except Exception as exc:
            failures.append('route restoration: ' + type(exc).__name__)
        try:
            call(['touch', remote_stop])
            process.wait(timeout=10)
        except Exception:
            process.terminate()
            process.wait(timeout=10)
        finished.set()
        watcher.join(timeout=5)
        resources.close()
        stderr.close()
        subprocess.run(ssh[:-1] + ['-O','exit',ssh[-1]], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
        control.unlink(missing_ok=True)
        control_root.rmdir()
        for sig, handler in old_handlers.items():
            signal.signal(sig, handler)
        failures.extend(watchdog_failures)
        report = {'schema':1,'stage':config.get('stage', 'official'),'protocol_sha256':hashlib.sha256(Path(config['protocol']).read_bytes()).hexdigest(),
                  'campaign_id':str(campaign),'expected_windows':expected,'completed_windows':len(results),
                  'state':'BLOCK_COMPLETE_PENDING_FULL_RESTORATION' if len(results)==expected and not failures else 'PARTIAL_OR_FAILED',
                  'official_accepted':False,'scope':'only selected cases; full protocol may remain partial',
                  'route_restoration':restored,'failures':failures,'runs':results}
        (out / 'campaign.json').write_text(json.dumps(report,indent=2)+'\n')
    return 0 if not failures and len(results)==expected else 1


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--config', type=Path, required=True)
    args=p.parse_args()
    config=json.loads(args.config.read_text())
    with g.exclusive_lock(Path(config['campaign_lock'])):
        return run(config)


if __name__ == '__main__':
    sys.exit(main())
