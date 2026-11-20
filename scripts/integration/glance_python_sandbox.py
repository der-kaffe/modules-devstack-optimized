#!/usr/bin/env python3
"""Original Python Glance with dedicated experimental schema/store/config.

Does not edit original service files, grants, SQL tables or image stores.
Uses the installed original uWSGI entry point and real Keystone middleware.
"""
import argparse
import configparser
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys

import combined_runtime as runtime


def private_configuration(original, directory, connection):
    parser = configparser.ConfigParser(interpolation=None, strict=False)
    parser.read_string(original)
    if not parser.has_section('database'):
        raise ValueError('original database section required')
    parser['database']['connection'] = connection
    parser['DEFAULT']['log_dir'] = str(directory / 'logs')
    parser['DEFAULT']['image_cache_dir'] = str(directory / 'cache')
    parser['DEFAULT']['node_staging_uri'] = 'file://' + str(directory / 'staging')
    parser['DEFAULT']['worker_self_reference_url'] = 'http://127.0.0.1:60998'
    backends = parser['DEFAULT'].get('enabled_backends', '')
    names = []
    if backends:
        for item in backends.split(','):
            name, kind = item.strip().split(':')
            if kind != 'file' or not re.fullmatch(r'[A-Za-z0-9_-]+', name):
                raise ValueError('only identified local filesystem backends supported')
            names.append(name)
    else:
        names = ['glance_store']
    names += ['os_glance_staging_store', 'os_glance_tasks_store']
    for name in names:
        if not parser.has_section(name):
            parser.add_section(name)
        parser[name]['filesystem_store_datadir'] = str(directory / ('store-' + name))
    if not parser.has_section('paste_deploy'):
        parser.add_section('paste_deploy')
    parser['paste_deploy']['config_file'] = str(directory / 'glance-api-paste.ini')
    stream = io.StringIO()
    parser.write(stream)
    return stream.getvalue(), names




def configured_controls(data):
    """Only numeric/bool allowlisted controls; never export connection or secrets."""
    parser = configparser.ConfigParser(interpolation=None, strict=False)
    parser.read_string(data.decode() if isinstance(data, bytes) else data)
    selected = {'database':['max_pool_size','max_overflow','connection_recycle_time'],
                'keystone_authtoken':['token_cache_time','memcache_pool_maxsize','memcache_use_advanced_pool'],
                'DEFAULT':['workers','image_size_cap','debug','use_syslog','use_stderr'],
                'glance_store':['filesystem_store_file_perm','filesystem_store_chunk_size']}
    result = {}
    for section, names in selected.items():
        result[section] = {}
        for name in names:
            value = parser.get(section,name,fallback=None)
            if value is None:
                parsed = None  # library defaults were not resolved; do not invent them
            elif value.lower() in ('true','false'):
                parsed = value.lower() == 'true'
            else:
                try:
                    parsed = int(value)
                except ValueError:
                    parsed = 'configured-value-not-exported'
            result[section][name] = parsed
    return result


def source_fingerprint():
    import importlib.metadata
    import platform
    package=Path('/opt/stack/glance/glance')
    digest=hashlib.sha256()
    for path in sorted(package.rglob('*.py')):
        digest.update(str(path.relative_to(package)).encode()+b'\0'+path.read_bytes())
    dependencies={}
    for name in ['glance','glance_store','keystonemiddleware','keystoneauth1','oslo.db','oslo.config','SQLAlchemy','PyMySQL']:
        try:dependencies[name]=importlib.metadata.version(name)
        except importlib.metadata.PackageNotFoundError:dependencies[name]=None
    head=subprocess.check_output(['git','-C','/opt/stack/glance','rev-parse','HEAD'],text=True).strip()
    config=subprocess.check_output(['sudo','cat','/etc/glance/glance-api.conf'])
    return {'python':platform.python_version(),'kernel':platform.release(),'glance_git_head':head,
            'glance_package_sha256':digest.hexdigest(),'api_config_sha256':hashlib.sha256(config).hexdigest(),
            'dependencies':dependencies,'configured_controls':configured_controls(config),
            'unconfigured_controls':'null means library default not resolved, not zero or disabled'}


def prepare(root):
    import pymysql
    runtime.validate_runtime_root(root)
    runtime.placement_runtime.probe_available_port(60998)
    combined = json.loads((root / 'combined.json').read_text())
    state = json.loads((root / 'state.json').read_text())
    if not combined['prepared'] or combined.get('python_sandbox'):
        raise ValueError('fresh dedicated prepared runtime required')
    runtime.placement_runtime.command(['systemctl','is-active','--quiet',combined['profile_unit']])
    directory = root / ('python-sandbox-' + secrets.token_hex(4))
    directory.mkdir(mode=0o700)
    original = subprocess.check_output(['sudo','cat','/etc/glance/glance-api.conf'],text=True)
    uwsgi = Path('/etc/glance/glance-uwsgi.ini').read_bytes()
    if uwsgi.count(b'127.0.0.1:60999') != 1 or not re.search(rb'(?m)^processes\s*=\s*1\s*$', uwsgi):
        raise ValueError('single-worker original uWSGI profile required')
    executable = Path('/bin/uwsgi')
    manage = Path('/opt/stack/data/venv/bin/glance-manage')
    if not executable.is_file() or not manage.is_file():
        raise ValueError('original installed Python tools required')
    schema = 'glance_python_test_' + state['id']
    credentials = {}
    cfg = configparser.ConfigParser(interpolation=None)
    cfg.read(root / 'root.cnf')
    with pymysql.connect(unix_socket=cfg['client']['socket'],user='root',password=cfg['client']['password'],connect_timeout=5) as db:
        with db.cursor() as cursor:
            cursor.execute('SELECT @@server_uuid,@@port')
            observed = cursor.fetchone()
            if observed != (combined['server_uuid'],13316):
                raise ValueError('independent native server identity mismatch')
            cursor.execute('CREATE DATABASE `' + schema + '`')
            for role, rights in [('migrate','ALL PRIVILEGES'),('app','SELECT,INSERT,UPDATE,DELETE')]:
                user, password = 'py_' + role + '_' + state['id'], secrets.token_hex(24)
                cursor.execute("CREATE USER %s@'127.0.0.1' IDENTIFIED BY %s",(user,password))
                cursor.execute('GRANT ' + rights + ' ON `' + schema.replace('_',r'\_') + "`.* TO %s@'127.0.0.1'",(user,))
                credentials[role] = 'mysql+pymysql://' + user + ':' + password + '@127.0.0.1:13316/' + schema + '?charset=utf8'
        db.commit()
    migration, stores = private_configuration(original,directory,credentials['migrate'])
    application, _ = private_configuration(original,directory,credentials['app'])
    for name in ['logs','cache','staging',*['store-'+n for n in stores]]:
        (directory/name).mkdir(mode=0o700)
    paste = Path('/etc/glance/glance-api-paste.ini')
    runtime.save(directory/'glance-api-paste.ini',subprocess.check_output(['sudo','cat',str(paste)],text=True))
    runtime.save(directory/'migration.conf',migration)
    runtime.save(directory/'glance-api.conf',application)
    runtime.save(directory/'uwsgi.ini',uwsgi.replace(b'127.0.0.1:60999',b'127.0.0.1:60998').decode())
    with (directory/'migration.log').open('x') as log:
        subprocess.run([str(manage),'--config-file',str(directory/'migration.conf'),'db_sync'],check=True,stdout=log,stderr=subprocess.STDOUT,timeout=180)
    unit = 'tt-glance-python-bench-' + state['id']
    unit_file = Path('/run/systemd/system')/(unit+'.service')
    if unit_file.exists():
        raise ValueError('fresh own unit file required')
    content = ('[Unit]\nAfter='+combined['profile_unit']+'\nPartOf='+combined['profile_unit']+'\n[Service]\n'
               'User=stack\nGroup=stack\nKillMode=process\nKillSignal=SIGTERM\nEnvironment=OS_GLANCE_CONFIG_DIR='+str(directory)+'\n'
               'ExecStart='+str(executable)+' --procname-prefix tt-glance-python --ini '+str(directory/'uwsgi.ini')+' --venv /opt/stack/data/venv\nTimeoutStopSec=90\n')
    sandbox={'unit':unit,'unit_file':str(unit_file),'unit_sha256':hashlib.sha256(content.encode()).hexdigest(),
             'schema':schema,'port':60998,'directory':str(directory),
             'original_api_config_sha256':hashlib.sha256(original.encode()).hexdigest(),
             'original_uwsgi_sha256':hashlib.sha256(uwsgi).hexdigest(),'original_source_unmodified':True,'source':source_fingerprint()}
    combined['python_sandbox']=sandbox
    state['units'].append(unit)
    runtime.save(root/'combined.json',combined)
    runtime.save(root/'state.json',state)
    subprocess.run(['sudo','tee',str(unit_file)],input=content,text=True,check=True,stdout=subprocess.DEVNULL)
    runtime.placement_runtime.command(['sudo','systemctl','daemon-reload'])
    runtime.placement_runtime.command(['sudo','systemctl','start',unit])
    import time
    for _ in range(100):
        if runtime.listener_owned_by(unit, 60998):
            break
        time.sleep(.2)
    else:
        raise ValueError('private Python listener ownership not verified')
    print(json.dumps(sandbox))


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--root',type=Path)
    p.add_argument('--inspect-source',action='store_true')
    p.add_argument('--allow-mutations',action='store_true')
    args=p.parse_args()
    if args.inspect_source:
        print(json.dumps(source_fingerprint()))
        return 0
    if not args.allow_mutations or args.root is None:
        p.error('explicit opt-in and dedicated root required')
    try:
        prepare(args.root)
        return 0
    except Exception as error:
        print('Python sandbox preparation failed: '+type(error).__name__,file=sys.stderr)
        return 2


if __name__=='__main__':
    sys.exit(main())
