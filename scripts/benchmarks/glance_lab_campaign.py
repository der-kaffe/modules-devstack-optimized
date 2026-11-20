#!/usr/bin/env python3
"""Finite multi-session lab campaign with private Python/Go catalogs.

No publication, Git writes, host configuration changes, original SQL writes or
forced shutdowns. Failed restoration keeps the VM available for repair.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import threading
import time
import uuid

sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'integration'))
from restore_benchmark_lab import restore
from glance_benchmark import exclusive_lock

VM='tt-openstack-lab'
VM_UUID='385591e9-02ea-4209-b09e-49f3f882a8e6'
REMOTE_BENCHMARK_MODULES = ['placement_resources.py', 'sample_processes.py', 'placement_http.py', 'glance_benchmark.py']
BUDGET_UNITS=['devstack@n-api.service','devstack@n-api-meta.service','devstack@n-cond-cell1.service',
 'devstack@n-super-cond.service','devstack@n-cpu.service','devstack@n-sch.service','devstack@neutron-api.service',
 'devstack@neutron-ovn-maintenance-worker.service','devstack@neutron-periodic-workers.service',
 'devstack@neutron-rpc-server.service','devstack@q-ovn-agent.service']


def sha(path):
    h=hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda:stream.read(256*1024),b''):h.update(block)
    return h.hexdigest()


def available():
    return next(int(s.split()[1])*1024 for s in Path('/proc/meminfo').read_text().splitlines() if s.startswith('MemAvailable:'))


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config',type=Path,required=True)
    parser.add_argument('--allow-mutations',action='store_true')
    args=parser.parse_args()
    if not args.allow_mutations:parser.error('explicit opt-in required')
    config=json.loads(args.config.read_text())
    root=Path(config['output'])
    if not root.is_absolute() or root!=root.resolve() or root.exists():
        raise ValueError('fresh canonical output required')
    if not 600<=config['maximum_seconds']<=36000 or not 1<=config.get('cases_per_session',4)<=4:
        raise ValueError('bounded job and at most four cases per session required')
    source=Path(config['source_root'])
    protocol_path=Path(config['protocol'])
    protocol=json.loads(protocol_path.read_text())
    cases=config['cases']
    if not cases or len(set(map(tuple,cases)))!=len(cases) or any(c not in protocol['cases'] for c in cases):
        raise ValueError('declared unique protocol cases required')
    stage=config['stage']
    if stage not in ('pilot','official'):raise ValueError('pilot or official required')
    if stage=='official' and (protocol['status']!='frozen' or not protocol['official_execution_enabled']):
        raise ValueError('frozen protocol required')
    root.mkdir(mode=0o700)
    private=root/'private';private.mkdir(mode=0o700)
    stop=threading.Event()
    child=[None]
    def interrupted(*_):
        stop.set()
        if child[0] is not None and child[0].poll() is None:
            child[0].send_signal(signal.SIGTERM)
    for sig in (signal.SIGINT,signal.SIGTERM,signal.SIGHUP):signal.signal(sig,interrupted)
    deadline=time.time()+config['maximum_seconds']
    job_id=uuid.uuid4().hex
    events=root/'events.jsonl'
    def event(phase,**values):
        row={'utc_ns':time.time_ns(),'phase':phase,**values}
        with events.open('a') as stream:stream.write(json.dumps(row)+'\n');stream.flush();os.fsync(stream.fileno())
        (root/'state.json').write_text(json.dumps(row,indent=2)+'\n')
    inhibit=subprocess.Popen(['systemd-inhibit','--what=sleep:idle','--mode=block','--who=tt-glance-'+job_id[:8],
                             '--why=Finite reproducible thesis campaign','sleep',str(config['maximum_seconds']+300)],
                             stdout=subprocess.DEVNULL,stderr=(private/'inhibitor.stderr').open('w'))
    ssh=config['ssh']
    virsh=['virsh','-c','qemu:///system']
    sequence=[0]
    def host(*argv):
        return subprocess.check_output(virsh+list(argv),text=True,stderr=subprocess.STDOUT,timeout=30,env=dict(os.environ,LC_ALL='C'))
    def remote(folder,label,command,timeout=180):
        sequence[0]+=1
        logs=folder/'private';logs.mkdir(mode=0o700,exist_ok=True)
        p=subprocess.run(ssh+[command],capture_output=True,text=True,timeout=timeout)
        name=f'{sequence[0]:04d}-{label}'
        (logs/(name+'.stdout')).write_text(p.stdout)
        (logs/(name+'.stderr')).write_text(p.stderr)
        if p.returncode:raise RuntimeError(label+' failed; private diagnostics retained')
        return p.stdout
    def guest(folder,label,argv,admin=False,timeout=180):
        command=shlex.join(argv)
        if admin:command='source /opt/stack/devstack/openrc admin admin >/dev/null 2>&1; '+command
        return remote(folder,label,command,timeout)
    remote_code='/home/stack/tt-bench-v4-'+job_id
    event('INITIALIZED',job_id=job_id,remote_code=remote_code)
    results=[]
    failed=None
    def ensure_safety():
        if stop.is_set() or time.time()>=deadline:raise RuntimeError('finite deadline or supervised abort')
        if inhibit.poll() is not None:raise RuntimeError('sleep inhibitor lost')
        if available()<768*1024**2:raise RuntimeError('host memory reserve')
        battery=Path('/sys/class/power_supply/BAT0/capacity')
        online=Path('/sys/class/power_supply/AC0/online')
        if battery.exists() and online.exists() and online.read_text().strip()=='0':
            raise RuntimeError('battery reserve; restore before critical power')
        import shutil
        for path in (root,Path(config['vm_disk'])):
            if shutil.disk_usage(path).free<2*1024**3:raise RuntimeError('host disk reserve')
    def upload(folder,path,destination):
        with path.open('rb') as stream:
            p=subprocess.run(ssh+['cat > '+shlex.quote(destination)],stdin=stream,stdout=subprocess.DEVNULL,stderr=subprocess.PIPE,timeout=120)
        if p.returncode:raise RuntimeError('public artifact upload failed')
    def backend_cleanup(folder,runtime):
        result=json.loads(guest(folder,'restore-routes',['/opt/stack/data/venv/bin/python',remote_code+'/api_routes.py','restore','--root',runtime,'--allow-mutations']))
        if not result['restored']:raise RuntimeError('original routes not restored')
        routes=json.loads(guest(folder,'route-metadata',['cat',runtime+'/routes.json']))
        if routes.get('guardian'):
            guest(folder,'stop-guardian',['sudo','systemctl','stop',routes['guardian']+'.service'])
        text=guest(folder,'cleanup-backend',['/opt/stack/data/venv/bin/python',remote_code+'/combined_runtime.py','cleanup','--root',runtime,'--allow-mutations'],True)
        if not json.loads(text.strip().splitlines()[-1]).get('cleanup_verified'):
            raise RuntimeError('backend cleanup incomplete')
        (folder/'backend-cleanup.json').write_text(json.dumps({'restored_routes':result,'cleanup_verified':True},indent=2)+'\n')
    try:
        with exclusive_lock(Path('/tmp/tt-openstack-benchmarks-20261031.lock')):
            chunk_size=config.get('cases_per_session',4)
            for chunk_index,offset in enumerate(range(0,len(cases),chunk_size),1):
                ensure_safety()
                subset=cases[offset:offset+chunk_size]
                estimated=len(subset)*protocol['repetitions']*2*(protocol['warmup_s']+protocol['measurement_s']+8)+600
                if time.time()+estimated>deadline:raise RuntimeError('remaining finite job time cannot fit complete session')
                folder=root/f'session-{chunk_index:02d}';folder.mkdir(mode=0o700)
                runtime=None;profile=None;budget=None;session_restored=False;session_results=[]
                guest_mutations=False;original_route_hashes=None;profile_baseline=None;baseline=folder/'resources-before.json'
                original=config['xml_sha256']
                if host('domuuid',VM).strip()!=VM_UUID or host('domstate',VM).strip()!='shut off':
                    raise RuntimeError('identified VM must be off; do not compete with another campaign')
                if hashlib.sha256(host('dumpxml',VM,'--inactive').encode()).hexdigest()!=original:
                    raise RuntimeError('original XML changed')
                if available()<5*1024**3:raise RuntimeError('5 GiB host available required before 4 GiB guest boot')
                event('BOOTING',session=chunk_index)
                try:
                    try:
                        host('setmem',VM,'4GiB','--config');host('start',VM)
                    finally:
                        host('setmem',VM,'6GiB','--config')
                    if hashlib.sha256(host('dumpxml',VM,'--inactive').encode()).hexdigest()!=original:
                        raise RuntimeError('persistent XML did not restore immediately')
                    for _ in range(90):
                        ensure_safety()
                        p=subprocess.run(ssh+['true'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=8)
                        if p.returncode==0:break
                        time.sleep(2)
                    else:raise RuntimeError('guest SSH readiness')
                    if chunk_index==1:
                        remote(folder,'code-root','umask 077; mkdir -- '+shlex.quote(remote_code))
                        for name in ['lab_capacity_profile.py','lab_restoration_lease.py']:
                            upload(folder,source/'scripts/integration'/name,remote_code+'/'+name)
                    profile_root='/var/tmp/tt-capacity-'+str(uuid.uuid4())
                    profile_unit='tt-capacity-'+uuid.uuid4().hex[:12]
                    profile={'root':profile_root,'unit':profile_unit}
                    (folder/'profile-launch.json').write_text(json.dumps(profile,indent=2)+'\n')
                    guest_mutations=True
                    guest(folder,'profile-start',['sudo','systemd-run','--unit='+profile_unit,'--property=RuntimeMaxSec=7250',
                        '--property=TimeoutStopSec=240','--property=ExecStopPost=/usr/bin/python3 '+remote_code+'/lab_capacity_profile.py restore --root '+profile_root+' --allow-mutations',
                        '/usr/bin/python3',remote_code+'/lab_capacity_profile.py','session','--root',profile_root,'--maximum-seconds','7200','--allow-mutations','--await-preflight'])
                    for _ in range(100):
                        ensure_safety()
                        p=subprocess.run(ssh+['sudo cat '+shlex.quote(profile_root+'/state.json')],capture_output=True,text=True,timeout=15)
                        if p.returncode==0 and json.loads(p.stdout)['phase']=='WAITING_PREFLIGHT':
                            profile_state=json.loads(p.stdout);break
                        time.sleep(2)
                    else:raise RuntimeError('worker profile readiness failed')
                    profile_deadline=profile_state['started_utc_ns']/1e9+7200
                    baseline=folder/'resources-before.json'
                    profile_baseline=folder/'profile-baseline.json';profile_baseline.write_text(json.dumps(profile_state,indent=2)+'\n')
                    fingerprints="import hashlib,json,pathlib; paths={'glance':'/etc/apache2/sites-enabled/glance-api.conf','placement':'/etc/apache2/sites-enabled/placement-api.conf'}; print(json.dumps({k:hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest() for k,p in paths.items()}))"
                    original_route_hashes=json.loads(guest(folder,'route-baseline',['python3','-c',fingerprints]))
                    inventory=Path(config['inventory_script']).read_text()
                    before=json.loads(remote(folder,'resources-before','source /opt/stack/devstack/openrc admin admin >/dev/null 2>&1; /opt/stack/data/venv/bin/python -c '+shlex.quote(inventory),90))
                    baseline=folder/'resources-before.json';baseline.write_text(json.dumps(before,indent=2)+'\n')
                    unit_probe="import json,subprocess; print(json.dumps({u:subprocess.run(['systemctl','is-active','--quiet',u]).returncode==0 for u in "+repr(BUDGET_UNITS)+"}))"
                    original_units=json.loads(guest(folder,'original-budget-units',['python3','-c',unit_probe]))
                    (folder/'original-budget-units.json').write_text(json.dumps(original_units,indent=2)+'\n')
                    if not all(original_units.values()):
                        raise RuntimeError('required original Nova/Neutron services not ready before profile application')
                    guest(folder,'preflight-released',['sudo','touch',profile_root+'/apply-approved'])
                    for _ in range(100):
                        ensure_safety()
                        p=subprocess.run(ssh+['sudo cat '+shlex.quote(profile_root+'/state.json')],capture_output=True,text=True,timeout=15)
                        if p.returncode==0 and json.loads(p.stdout)['phase']=='ACTIVE':break
                        time.sleep(2)
                    else:raise RuntimeError('worker profile application failed')
                    if chunk_index==1:
                        for name in ['lab_capacity_profile.py','lab_restoration_lease.py','combined_runtime.py','api_routes.py','placement_runtime.py','placement_switch.py','glance_python_sandbox.py']:
                            upload(folder,source/'scripts/integration'/name,remote_code+'/'+name)
                        guest(folder,'collector-directory',['mkdir','-p',remote_code+'/scripts/benchmarks'])
                        for name in REMOTE_BENCHMARK_MODULES:
                            upload(folder,source/'scripts/benchmarks'/name,remote_code+'/scripts/benchmarks/'+name)
                        guest(folder,'collector-import-check',['python3','-c',"import sys; sys.path.insert(0,"+repr(remote_code+'/scripts/benchmarks')+"); from placement_resources import collect"])
                        for name,path in config['binaries'].items():
                            upload(folder,Path(path),remote_code+'/'+name)
                            guest(folder,'binary-mode',['chmod','700',remote_code+'/'+name])
                            observed=guest(folder,'binary-sha',['sha256sum',remote_code+'/'+name]).split()[0]
                            if observed!=sha(Path(path)):raise ValueError('binary transport checksum mismatch')
                    budget='tt-http-budget-'+uuid.uuid4().hex[:10]
                    (folder/'budget-launch.json').write_text(json.dumps({'unit':budget,'original_units':BUDGET_UNITS},indent=2)+'\n')
                    guest(folder,'http-budget',['sudo','systemd-run','--unit='+budget,'--property=RuntimeMaxSec=7000',
                        '--property=TimeoutStopSec=240','--property=After='+profile_unit+'.service','--property=PartOf='+profile_unit+'.service',
                        '--property=ExecStartPre=/usr/bin/systemctl stop '+' '.join(BUDGET_UNITS),
                        '--property=ExecStopPost=/usr/bin/systemctl start '+' '.join(BUDGET_UNITS),'/usr/bin/sleep','7000'])
                    for case_index,case in enumerate(subset,1):
                        ensure_safety()
                        if runtime is not None:
                            backend_cleanup(folder,runtime)
                            runtime=None
                        if time.time()+protocol['repetitions']*2*(protocol['warmup_s']+protocol['measurement_s']+8)+900>profile_deadline:
                            raise RuntimeError('profile time cannot fit next complete case and restoration')
                        runtime='/home/stack/tt-placement-integrated-'+uuid.uuid4().hex
                        case_folder=folder/f'case-{case_index:02d}';case_folder.mkdir(mode=0o700)
                        (case_folder/'runtime-launch.json').write_text(json.dumps({'root':runtime,'remote_code':remote_code},indent=2)+'\n')
                        guest(case_folder,'prepare',['/opt/stack/data/venv/bin/python',remote_code+'/combined_runtime.py','prepare','--root',runtime,
                            '--placement-binary',remote_code+'/placement','--glance-binary',remote_code+'/glance','--profile-unit',profile_unit+'.service','--allow-mutations'],True)
                        controls=json.loads(guest(case_folder,'match-db',['/opt/stack/data/venv/bin/python',remote_code+'/combined_runtime.py','match-db-baseline','--root',runtime,'--allow-mutations']))
                        (case_folder/'database-controls.json').write_text(json.dumps(controls,indent=2)+'\n')
                        sandbox=json.loads(guest(case_folder,'python-sandbox',['/opt/stack/data/venv/bin/python',remote_code+'/glance_python_sandbox.py','--root',runtime,'--allow-mutations']))
                        state=json.loads(guest(case_folder,'runtime-state',['cat',runtime+'/state.json']))
                        combined=json.loads(guest(case_folder,'combined-state',['cat',runtime+'/combined.json']))
                        for name in ('glance','placement'):
                            if combined['binary_sha256'][name]!=sha(Path(config['binaries'][name])):
                                raise ValueError('production binary changed')
                        environment={'original_python':sandbox['source'],'go_binary_sha256':combined['binary_sha256'],
                            'go_repository_sha':config['code_sha'],'database_controls':controls,'vm_memory_mib':4096,'vcpus':4,
                            'python_workers':1,'go_glance_gomaxprocs':1,'go_max_upload_bytes':67108864,'go_transfer_limit':2,
                            'python_schema':sandbox['schema'],'go_schema':combined['glance_schema'],
                            'go_sql_pool_source_defaults':{'max_open_connections':4,'max_idle_connections':2,'max_lifetime_s':180,'source':'internal/glance/repository/mysql/database.go at '+config['code_sha']},
                            'go_store_source_calls':'file and directory fsync; not a physical power-loss guarantee',
                            'authentication':'original Python middleware; Go per-user-token Keystone validation; no auth bypass'}
                        environment_file=case_folder/'environment.json';environment_file.write_text(json.dumps(environment,indent=2)+'\n')
                        token=guest(case_folder,'token',['/opt/stack/data/venv/bin/openstack','token','issue','-f','value','-c','id'],True).strip()
                        for secret_log in (case_folder/'private').glob('*-token.stdout'):
                            secret_log.unlink()
                        token_file=case_folder/'private/admin.token';token_file.write_text(token);token_file.chmod(0o600)
                        # No token value is copied to public state, events or manifests.
                        receipt={'schema':1,'protocol_sha256':sha(protocol_path),'instrument_sha256':sha(source/'scripts/benchmarks/glance_benchmark.py'),
                            'environment_sha256':sha(environment_file),'code_sha':config['code_sha'],'valid_until_utc_s':min(profile_deadline-600,deadline-600),'gates':config.get('entry_gates',{})}
                        receipt_file=case_folder/'entry-evidence.json';receipt_file.write_text(json.dumps(receipt,indent=2)+'\n')
                        groups={'python':'/system.slice/'+sandbox['unit']+'.service','go':'/system.slice/'+combined['glance_unit']+'.service',
                            'original-mysql':'/system.slice/mysql.service','experimental-mysql':'/system.slice/'+state['db_unit']+'.service',
                            'keystone':'/system.slice/system-devstack.slice/devstack@keystone.service','apache':'/system.slice/apache2.service'}
                        run_config={'output':str(case_folder/'raw'),'protocol':str(protocol_path),'entry_evidence':str(receipt_file),
                            'cases':[case],'stage':stage,'deadline_utc_s':min(profile_deadline-600,deadline-600),'ssh':ssh,'remote_code':remote_code,
                            'runtime':runtime,'resource_groups':groups,'url':'http://192.168.122.10/image','token_file':str(token_file),
                            'environment':str(environment_file),'code_sha':config['code_sha'],'operation_lock':'/tmp/tt-glance-operation-v4.lock','campaign_lock':'/tmp/tt-glance-campaign-v4.lock',
                            'payloads':config['payloads'],'max_address_space_mib':768,'guardian_seconds':3300}
                        run_file=case_folder/'run-config.json';run_file.write_text(json.dumps(run_config,indent=2)+'\n')
                        event('MEASURING',session=chunk_index,case=case,folder=str(case_folder))
                        with (case_folder/'coordinator.log').open('x') as log:
                            child[0]=subprocess.Popen([sys.executable,'-u',str(source/'scripts/benchmarks/glance_campaign.py'),'--config',str(run_file)],stdout=log,stderr=subprocess.STDOUT)
                            end=min(time.time()+2900,profile_deadline-480,deadline-480)
                            while child[0].poll() is None:
                                try:ensure_safety()
                                except Exception:
                                    interrupted()
                                if time.time()>=end:interrupted()
                                if stop.is_set():
                                    try:child[0].wait(timeout=180)
                                    except subprocess.TimeoutExpired:child[0].kill();child[0].wait(timeout=15)
                                    break
                                time.sleep(3)
                            code=child[0].returncode;child[0]=None
                        token_file.unlink(missing_ok=True)
                        if code:raise RuntimeError('case incomplete; preserve and restore')
                        after_source=json.loads(guest(case_folder,'source-after',['/opt/stack/data/venv/bin/python',remote_code+'/glance_python_sandbox.py','--inspect-source']))
                        if after_source!=sandbox['source']:
                            raise ValueError('original Python code, configuration or dependency identity changed during case')
                        report=json.loads((case_folder/'raw/campaign.json').read_text())
                        if report['failures'] or report['completed_windows']!=2*protocol['repetitions']:
                            raise RuntimeError('case did not finish declared repetitions')
                        session_results.append({'case':case,'folder':str(case_folder),'run_config':str(run_file)})
                        event('CASE_MEASURED',session=chunk_index,case=case)
                finally:
                    event('RESTORING',session=chunk_index)
                    if not guest_mutations:
                        if hashlib.sha256(host('dumpxml',VM,'--inactive').encode()).hexdigest()!=original:
                            raise RuntimeError('preflight XML not restored; VM kept available')
                        host('shutdown',VM)
                        for _ in range(90):
                            if host('domstate',VM).strip()=='shut off':break
                            time.sleep(2)
                        else:raise RuntimeError('preflight orderly shutdown did not finish')
                        session_restored=True
                        (folder/'preflight-restored.json').write_text(json.dumps({'restored':True,'vm_state':'shut off','guest_service_mutations_started':False})+'\n')
                    else:
                        restore_config={'ssh':ssh,'runtime':runtime,'remote_code':remote_code,'profile_unit':profile['unit'],
                            'profile_root':profile['root'],'budget_units':[budget] if budget else [],'xml_sha256':original,
                            'inventory_script':config['inventory_script'],'baseline_resources':str(baseline),
                            'original_route_hashes':original_route_hashes,'profile_baseline':str(profile_baseline),
                            'extra_original_units':BUDGET_UNITS}
                        (folder/'restoration-config.json').write_text(json.dumps(restore_config,indent=2)+'\n')
                        restoration=restore(restore_config,folder/'restoration')
                        session_restored=restoration['restored']
                        if not session_restored:raise RuntimeError('NEEDS_ATTENTION: restoration failed; VM not forced off')
                    event('RESTORED_OFF',session=chunk_index)
                    if stage=='official':
                        for item in session_results:
                            validation=Path(item['folder'])/'accepted'
                            p=subprocess.run([sys.executable,str(source/'scripts/benchmarks/validate_glance_campaign.py'),
                                '--config',item['run_config'],'--restoration',str(folder/'restoration/restoration-verified.json'),'--output',str(validation)],
                                capture_output=True,text=True,timeout=180)
                            if p.returncode:raise RuntimeError('raw acceptance failed after restoration')
                            item['acceptance']=str(validation/'acceptance.json')
                    results.extend(session_results)
                    (root/'completed-cases.json').write_text(json.dumps(results,indent=2)+'\n')
                if stop.is_set():raise RuntimeError('supervised abort restored')
    except Exception as error:
        failed=type(error).__name__+': '+str(error)
    finally:
        if child[0] is not None and child[0].poll() is None:
            child[0].terminate()
            try:child[0].wait(timeout=180)
            except subprocess.TimeoutExpired:child[0].kill();child[0].wait(timeout=15)
        inhibit.terminate()
        try:inhibit.wait(timeout=10)
        except subprocess.TimeoutExpired:inhibit.kill();inhibit.wait()
        try:
            vm_state=host('domstate',VM).strip()
        except Exception:
            vm_state='UNKNOWN';failed=(failed or '')+' hypervisor final state unavailable'
        complete=not failed and len(results)==len(cases) and vm_state=='shut off'
        report={'schema':1,'stage':stage,'complete':complete,'full_protocol_complete':complete and set(map(tuple,cases))==set(map(tuple,protocol['cases'])),
            'cases_completed':results,'requested_cases':cases,'error':failed,'vm_state':vm_state,'protocol_sha256':sha(protocol_path),
            'production_sha':config['code_sha'],'job_id':job_id,'remote_code':remote_code}
        (root/'job-report.json').write_text(json.dumps(report,indent=2)+'\n')
        event('COMPLETE' if complete else 'INCOMPLETE',error=failed,vm_state=vm_state)
        print(json.dumps({'complete':complete,'cases_completed':len(results),'vm_state':vm_state,'error':failed}),flush=True)
    return 0 if complete else 1


if __name__=='__main__':
    sys.exit(main())
