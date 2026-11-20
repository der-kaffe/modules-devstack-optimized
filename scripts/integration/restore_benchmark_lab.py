#!/usr/bin/env python3
"""Restore one identified local session; shutdown is gated by guest evidence."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import time
import uuid

VM_UUID='385591e9-02ea-4209-b09e-49f3f882a8e6'


def restore(config, output):
    output.mkdir(mode=0o700,exist_ok=False)
    private=output/'private'
    private.mkdir(mode=0o700)
    ssh=config['ssh']
    reports=[]
    errors=[]
    def save(name,value):
        path=output/(name+'.json')
        path.write_text(json.dumps(value,indent=2)+'\n')
        reports.append(path)
        return value
    def remote(name,command,timeout=180):
        p=subprocess.run(ssh+[command],capture_output=True,text=True,timeout=timeout)
        (private/(name+'.stdout')).write_text(p.stdout)
        (private/(name+'.stderr')).write_text(p.stderr)
        if p.returncode:
            raise RuntimeError(name+' failed; private diagnostics retained')
        return p.stdout
    def guest(name,args,admin=False):
        cmd=shlex.join(args)
        if admin:
            cmd='source /opt/stack/devstack/openrc admin admin >/dev/null 2>&1; '+cmd
        return remote(name,cmd)
    def stop_if_active(name,unit):
        # Exit 3/4 means inactive/not found. Never act on any other unit.
        p=subprocess.run(ssh+['systemctl is-active --quiet '+shlex.quote(unit)],capture_output=True,timeout=15)
        if p.returncode==0:
            guest(name,['sudo','systemctl','stop',unit])
    virsh=['virsh','-c','qemu:///system']
    vm=config.get('vm','tt-openstack-lab')
    runtime=config['runtime']
    code=config['remote_code']
    try:
        if subprocess.check_output(virsh+['domuuid',vm],text=True,env=dict(os.environ,LC_ALL='C')).strip()!=VM_UUID:
            raise ValueError('wrong VM identity')
        own={'units':[]}
        present=False
        if runtime:
            p=subprocess.run(ssh+['test -d '+shlex.quote(runtime)],capture_output=True,timeout=15)
            if p.returncode not in (0,1):raise RuntimeError('backend presence unknown')
            present=p.returncode==0
        if present:
            route=json.loads(guest('routes-restored',['/opt/stack/data/venv/bin/python',code+'/api_routes.py','restore','--root',runtime,'--allow-mutations']))
            if route.get('restored') is not True:
                raise ValueError('original routes not restored')
            save('routes-restored',route)
            state=json.loads(guest('route-metadata',['cat',runtime+'/routes.json']))
            save('route-metadata',state)
            if state.get('guardian'):
                stop_if_active('guardian-stopped',state['guardian']+'.service')
            own=json.loads(guest('runtime-metadata',['cat',runtime+'/state.json']))
            save('runtime-metadata',own)
            combined=json.loads(guest('combined-metadata',['cat',runtime+'/combined.json']))
            if combined.get('imported_images'):
                save('own-images-deleted',json.loads(guest('own-images-deleted',['/opt/stack/data/venv/bin/python',code+'/combined_runtime.py','delete-images','--root',runtime,'--allow-mutations'],True)))
            result=guest('runtime-cleanup',['/opt/stack/data/venv/bin/python',code+'/combined_runtime.py','cleanup','--root',runtime,'--allow-mutations'],True)
            final_line=json.loads(result.strip().splitlines()[-1])
            if not final_line.get('cleanup_verified'):
                raise ValueError('dedicated backend cleanup not verified')
            save('runtime-cleanup',final_line)
        else:
            hashes=config.get('original_route_hashes')
            if not hashes:
                raise ValueError('original route fingerprint required when no backend was prepared')
            probe="import hashlib,json,pathlib; paths={'glance':'/etc/apache2/sites-enabled/glance-api.conf','placement':'/etc/apache2/sites-enabled/placement-api.conf'}; print(json.dumps({k:hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest() for k,p in paths.items()}))"
            observed=json.loads(guest('unmodified-routes',['python3','-c',probe]))
            if observed!=hashes:raise ValueError('routes changed without a prepared backend')
            save('unmodified-routes',observed)
        for index,unit in enumerate(config['budget_units']):
            stop_if_active('budget-stopped-'+str(index),unit+'.service')
        existing_lease=config.get('existing_restoration_lease')
        if existing_lease:
            lease_root=existing_lease['root'];lease_unit=existing_lease['unit']
        else:
            lease_root='/var/tmp/tt-restoration-lease-'+str(uuid.uuid4())
            lease_unit='tt-restoration-lease-'+uuid.uuid4().hex[:12]
            guest('lease-handover',['sudo','systemd-run','--unit='+lease_unit,'--property=RuntimeMaxSec=620',
                '/usr/bin/python3',code+'/lab_restoration_lease.py','--root',lease_root,'--maximum-seconds','600'])
        stop_if_active('profile-stopped',config['profile_unit']+'.service')
        lease_state=None
        for _ in range(120):
            p=subprocess.run(ssh+['sudo cat '+shlex.quote(lease_root+'/state.json')],capture_output=True,text=True,timeout=15)
            if p.returncode==0:
                lease_state=json.loads(p.stdout)
                if lease_state['phase']=='HELD':break
                if lease_state['phase'] in ('NOT_ACQUIRED','RELEASED'):break
            time.sleep(1)
        if not lease_state or lease_state['phase']!='HELD':
            raise ValueError('exclusive restoration lease unavailable; do not shut down another campaign')
        guest('lease-kernel-ownership', ['sudo','python3',code+'/lab_restoration_lease.py','--root',lease_root,'--unit',lease_unit,'--verify-held'])
        save('exclusive-restoration-lease',{'unit':lease_unit,'root':lease_root,'state':lease_state})
        exists=subprocess.run(ssh+['sudo test -f '+shlex.quote(config['profile_root']+'/state.json')],capture_output=True,timeout=15)
        if exists.returncode==0:
            profile=json.loads(guest('profile-state',['sudo','cat',config['profile_root']+'/state.json']))
            if profile.get('phase')!='RESTORED' or profile.get('restored') is not True or profile.get('restore_errors'):
                raise ValueError('worker profile not restored')
        elif exists.returncode==1 and config.get('profile_baseline'):
            profile=json.loads(Path(config['profile_baseline']).read_text())
            profile['phase']='NOT_APPLIED_VERIFY_UNCHANGED'
        else:
            raise ValueError('profile restoration state unavailable')
        save('profile-state',profile)
        extra=config.get('extra_original_units',[])
        if extra:
            guest('extra-services-after-profile',['sudo','systemctl','start',*extra])
            time.sleep(3)
        probe='''import pathlib,hashlib,json,subprocess
state=STATE; checks=[]
for item in state['files']:
 p=pathlib.Path(item['path']);st=p.stat();checks.append({'path':str(p),'sha256_matches':hashlib.sha256(p.read_bytes()).hexdigest()==item['original_sha256'],'metadata_matches':(st.st_uid,st.st_gid,st.st_mode&0o777)==(item['uid'],item['gid'],item['mode'])})
units={u:subprocess.run(['systemctl','is-active','--quiet',u]).returncode==0 for u in set(state['units']+EXTRA)}
print(json.dumps({'configurations':checks,'original_units_active':units,'verified':all(x['sha256_matches'] and x['metadata_matches'] for x in checks) and all(units.values())}))
'''.replace('STATE',repr(profile)).replace('EXTRA',repr(config.get('extra_original_units',[])))
        check=json.loads(guest('profile-independent-check',['sudo','python3','-c',probe]))
        if not check['verified']:
            raise ValueError('configuration bytes, ownership or services differ')
        save('profile-independent-check',check)
        # Original endpoints restored: SDK census must see exactly the original
        # IDs/status, never delete new or unrecognized resources to pass a check.
        inventory=Path(config['inventory_script']).read_text()
        command='source /opt/stack/devstack/openrc admin admin >/dev/null 2>&1; /opt/stack/data/venv/bin/python -c '+shlex.quote(inventory)
        current=json.loads(remote('resources-final',command,90))
        before=json.loads(Path(config['baseline_resources']).read_text())
        for key in ('servers','images','flavors','networks','volumes'):
            def identities(rows):
                return sorted((r['id'],r.get('status')) for r in rows)
            if identities(current[key])!=identities(before[key]):
                raise ValueError('resource inventory differs: '+key)
        if 'allocations' in before:
            def quantities(value):
                return {sid:{rp:entry['resources'] for rp,entry in data['allocations'].items()} for sid,data in value.items()}
            if quantities(current.get('allocations',{}))!=quantities(before['allocations']):
                raise ValueError('original allocation quantities changed')
        for key in ('servers','images','flavors','networks','volumes'):
            if sorted(current[key],key=lambda row:row['id'])!=sorted(before[key],key=lambda row:row['id']):
                raise ValueError('original resource metadata changed: '+key)
        save('resources-final',current)
        save('resources-match',{'verified':True,'baseline_sha256':hashlib.sha256(Path(config['baseline_resources']).read_bytes()).hexdigest()})
        statuses={}
        for unit in own['units']:
            p=subprocess.run(ssh+['systemctl is-active --quiet '+shlex.quote(unit)],capture_output=True,timeout=15)
            statuses[unit]=p.returncode!=0
        if not all(statuses.values()):
            raise ValueError('own backend unit remains active')
        save('own-units-stopped',statuses)
        xml=subprocess.check_output(virsh+['dumpxml',vm,'--inactive'],env=dict(os.environ,LC_ALL='C'))
        if hashlib.sha256(xml).hexdigest()!=config['xml_sha256']:
            raise ValueError('persistent VM XML changed')
        save('persistent-xml',{'sha256':hashlib.sha256(xml).hexdigest(),'matches_original':True})
        lease_confirm=json.loads(guest('lease-confirmed',['sudo','cat',lease_root+'/state.json']))
        if lease_confirm.get('phase')!='HELD':
            raise ValueError('exclusive restoration lease expired; no shutdown')
        guest('lease-kernel-reconfirmed', ['sudo','python3',code+'/lab_restoration_lease.py','--root',lease_root,'--unit',lease_unit,'--verify-held'])
        # No shutdown occurs before all preceding guest gates pass.
        subprocess.run(virsh+['shutdown',vm],check=True,stdout=subprocess.DEVNULL,timeout=30)
        deadline=time.monotonic()+180
        while time.monotonic()<deadline:
            status=subprocess.check_output(virsh+['domstate',vm],text=True,timeout=20,env=dict(os.environ,LC_ALL='C')).strip()
            if status=='shut off':
                break
            time.sleep(2)
        if status!='shut off':
            raise RuntimeError('orderly shutdown did not finish; do not force poweroff')
        save('vm-final',{'state':status,'uuid':VM_UUID})
    except Exception as error:
        errors.append(type(error).__name__+': '+str(error))
    report={'schema':1,'restored':not errors,'vm_state':'shut off' if not errors else 'NEEDS_ATTENTION',
            'errors':errors,'evidence':[{'path':str(p.resolve()),'sha256':hashlib.sha256(p.read_bytes()).hexdigest()} for p in reports]}
    (output/'restoration-verified.json').write_text(json.dumps(report,indent=2)+'\n')
    return report


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--config',type=Path,required=True)
    p.add_argument('--output',type=Path,required=True)
    p.add_argument('--allow-mutations',action='store_true')
    args=p.parse_args()
    if not args.allow_mutations:
        p.error('explicit opt-in required')
    result=restore(json.loads(args.config.read_text()),args.output)
    print(json.dumps({'restored':result['restored'],'vm_state':result['vm_state'],'errors':result['errors']}))
    return 0 if result['restored'] else 1


if __name__=='__main__':
    sys.exit(main())
