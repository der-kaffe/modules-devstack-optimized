import os,sys,json,subprocess,shlex,fcntl
from pathlib import Path
cfg=json.loads(Path('/tmp/glance-fd-calibration-config-20261031-01.json').read_text());os.chdir(cfg['worktree']);sys.path.insert(0,'scripts/benchmarks')
from complete_glance_campaign import start_vm,deploy,freeze
from complete_placement_campaign import safely_shutdown
from glance_campaign import save
root=Path('/tmp/glance-fd-calibration-20261031-01');root.mkdir(mode=0o700,exist_ok=False)
status={'stage':'running','accepted':False,'source':cfg['frozen_head']};path=Path('/tmp/glance-fd-status-20261031-01.json');save(path,status)
with Path('/tmp/tt-glance-compact-supervisor.lock').open('a') as lock:
 fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
 try:
  freeze(cfg);start_vm(cfg);deploy(cfg)
  command='set -e; set +u; source /opt/stack/devstack/openrc admin admin >/dev/null 2>&1; exec '+shlex.join(['/opt/stack/data/venv/bin/python',cfg['remote_code']+'/benchmarks/glance_fd_calibration.py','--work-root','/home/stack/tt-placement-glance-fd-calibration-20261031-01','--output','/home/stack/glance-fd-result-20261031-01','--binary',cfg['remote_code']+'/glance','--allow-mutations'])
  log=root/'private-execution.log'
  with log.open('x') as f:
   log.chmod(0o600);run=subprocess.run([*cfg['ssh'],'bash','-lc',shlex.quote(command)],stdout=f,stderr=f)
  subprocess.run(['scp',*cfg['ssh'][1:-1],'-r',cfg['ssh'][-1]+':/home/stack/glance-fd-result-20261031-01',str(root/'lab')],check=True,stdout=subprocess.DEVNULL)
  report=json.loads((root/'lab/report.json').read_text());safely_shutdown(report,root/'vm-final-state.json')
  if run.returncode or not report['valid']:raise ValueError('FD calibration invalid; see preserved report')
  status.update(stage='complete',valid=True,cleanup=True,vm='shut off',original_fd_growth=report['original_fd_growth'],noop_fd_growth=report['noop_fd_growth']);save(path,status)
 except Exception as error:
  status.update(stage='needs_attention',error=type(error).__name__,reason=str(error) if isinstance(error,ValueError) else 'private log retained');save(path,status);raise
