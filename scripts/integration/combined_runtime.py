#!/usr/bin/env python3
"""Dedicated native MySQL with separate Go Glance/Placement schemas for lab only.

Reuses the inspected Placement runtime without its campaign/publication layer.
No original SQL writes, Keystone policy edits, data adoption or route switching.
"""
import argparse
from contextlib import redirect_stdout
import datetime
import hashlib
import json
import os
import re
from pathlib import Path
import secrets
import subprocess
import sys
import time
from types import SimpleNamespace
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parent))
import placement_runtime


def save(path, value):
    path.write_text(json.dumps(value, indent=2) if not isinstance(value, str) else value)
    path.chmod(0o600)



def validate_runtime_root(path):
    if (not path.is_absolute() or path != path.resolve()
            or not re.fullmatch(r'tt-placement-[a-z0-9-]+', path.name)):
        raise ValueError('canonical dedicated runtime root required')
    for candidate in (path, path.with_name(path.name + '-auth')):
        if candidate.exists() and (candidate.stat().st_uid != os.getuid() or candidate.stat().st_mode & 0o077):
            raise ValueError('private owned runtime directory required')


def admin_client():
    from keystoneauth1 import session
    from keystoneauth1.identity import v3
    from keystoneclient.v3 import client
    url = os.environ['OS_AUTH_URL'].rstrip('/')
    if url not in ('http://192.168.122.10/identity', 'http://192.168.122.10/identity/v3'):
        raise ValueError('identified local Keystone required')
    if not url.endswith('/v3'):
        url += '/v3'
    plugin = v3.Password(auth_url=url, username=os.environ['OS_USERNAME'], password=os.environ['OS_PASSWORD'],
                         project_name=os.environ['OS_PROJECT_NAME'], user_domain_name=os.environ.get('OS_USER_DOMAIN_NAME', 'Default'),
                         project_domain_name=os.environ.get('OS_PROJECT_DOMAIN_NAME', 'Default'))
    return client.Client(session=session.Session(auth=plugin, timeout=15)), url


def native_args(args, action):
    return SimpleNamespace(action=action, allow_mutations=True, root=args.root,
                           auth_root=args.root.with_name(args.root.name + '-auth'),
                           binary=args.placement_binary, mysql_port=13316, listen_port=18779,
                           public_url='http://192.168.122.10/placement', keystone_url='http://192.168.122.10/identity/v3')



def listener_owned_by(unit, port):
    try:
        pid = int(subprocess.check_output(['systemctl', 'show', unit, '-p', 'MainPID', '--value'], text=True).strip())
        if pid <= 0:
            return False
        listeners = subprocess.check_output(['sudo', 'ss', '-ltnp', 'sport = :' + str(port)], text=True)
        return re.search(r'pid=' + str(pid) + r'[,)]', listeners) is not None
    except (ValueError, subprocess.CalledProcessError):
        return False


def prepare(args):
    from keystoneauth1 import session
    from keystoneauth1.identity import v3
    from keystoneclient.v3 import client
    import pymysql
    placement_runtime.probe_available_port(9293)
    # placement_runtime enforces the private tt-placement root and loopback ports.
    if not args.root.is_absolute() or args.root != args.root.resolve() or not re.fullmatch(r'tt-placement-[a-z0-9-]+', args.root.name):
        raise ValueError('canonical dedicated runtime root required')
    if not args.glance_binary or not args.placement_binary or not args.glance_binary.is_file() or not args.placement_binary.is_file():
        raise ValueError('existing binaries required')
    if args.root.exists() or args.root.with_name(args.root.name + '-auth').exists():
        raise ValueError('fresh runtime and identity roots required')
    if not args.profile_unit.startswith('tt-capacity-'):
        raise ValueError('bounded capacity supervisor required')
    placement_runtime.command(['systemctl', 'is-active', '--quiet', args.profile_unit])
    auth_root = args.root.with_name(args.root.name + '-auth')
    auth_root.mkdir(mode=0o700)
    admin, url = admin_client()
    service_projects = admin.projects.list(name='service')
    if len(service_projects) != 1:
        raise ValueError('exact existing service project required for Nova composite tokens')
    project = service_projects[0]
    name = 'tt-benchmark-validator-' + uuid.uuid4().hex
    identity = {'name': name, 'project_id': project.id, 'user_id': None, 'application_id': None}
    save(auth_root / 'identity.json', identity)
    domain = next(d.id for d in admin.domains.list(name='Default'))
    password = secrets.token_urlsafe(32)
    user = admin.users.create(name, domain=domain, password=password, default_project=project.id)
    identity['user_id'] = user.id
    save(auth_root / 'identity.json', identity)
    role = next(r for r in admin.roles.list() if r.name == 'service')
    admin.roles.grant(role.id, user=user.id, project=project.id)
    scoped = client.Client(session=session.Session(auth=v3.Password(auth_url=url, user_id=user.id, password=password, project_id=project.id), timeout=15))
    app = scoped.application_credentials.create(name=name, user=user.id, roles=[{'name': 'service'}],
                                                expires_at=datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=8))
    identity['application_id'] = app.id
    save(auth_root / 'identity.json', identity)
    save(auth_root / 'validator.json', {'application_credential_id': app.id, 'application_credential_secret': app.secret})
    with (auth_root / 'native-prepare.log').open('w') as log, redirect_stdout(log):
        placement_runtime.run(native_args(args, 'prepare'))
    state = json.loads((args.root / 'state.json').read_text())
    # Attach only our transient units to the finite capacity session. They stop
    # before original workers are restored, including on supervisor failure.
    def budget_unit(unit):
        directory = Path('/run/systemd/system') / (unit + '.service.d')
        placement_runtime.command(['sudo', 'mkdir', '-p', str(directory)])
        text = '[Unit]\nAfter=' + args.profile_unit + '\nPartOf=' + args.profile_unit + '\n'
        placement_runtime.command(['sudo', 'tee', str(directory / 'budget.conf')], input=text, text=True)
    for unit in state['units']:
        budget_unit(unit)
    placement_runtime.command(['sudo', 'systemctl', 'daemon-reload'])
    root_config = args.root / 'root.cnf'
    import configparser
    cfg = configparser.ConfigParser(interpolation=None)
    cfg.read(root_config)
    schema = 'glance_go_test_bench_' + state['id']
    account_files = {}
    with pymysql.connect(unix_socket=cfg['client']['socket'], user='root', password=cfg['client']['password'], connect_timeout=5) as connection:
        with connection.cursor() as cursor:
            cursor.execute('SELECT @@server_uuid, DATABASE()')
            server_uuid, _ = cursor.fetchone()
            cursor.execute('CREATE DATABASE `' + schema + '`')
            for mode, rights in [('migrate', 'SELECT,INSERT,UPDATE,DELETE,CREATE,ALTER,INDEX,REFERENCES'), ('app', 'SELECT,INSERT,UPDATE,DELETE')]:
                username, secret = 'gg_' + mode + '_' + state['id'], secrets.token_hex(24)
                cursor.execute("CREATE USER %s@'127.0.0.1' IDENTIFIED BY %s", (username, secret))
                cursor.execute('GRANT ' + rights + ' ON `' + schema.replace('_', r'\_') + "`.* TO %s@'127.0.0.1'", (username,))
                path = args.root / ('glance-' + mode + '.dsn')
                save(path, username + ':' + secret + '@tcp(127.0.0.1:13316)/' + schema + '?parseTime=true\n')
                account_files[mode] = path
        connection.commit()
    combined = {'auth_root': str(auth_root), 'glance_schema': schema, 'server_uuid': server_uuid,
                'glance_unit': 'tt-glance-bench-' + state['id'], 'imported_images': [],
                'profile_unit': args.profile_unit, 'prepared': False}
    save(args.root / 'combined.json', combined)
    env = os.environ.copy()
    env.update(GLANCE_GO_DATABASE_DSN_FILE=str(account_files['migrate']), GOMAXPROCS='1')
    with (args.root / 'glance-migrate.log').open('w') as log:
        subprocess.run([str(args.glance_binary), 'migrate'], env=env, check=True, stdout=log, stderr=subprocess.STDOUT)
    env['GLANCE_GO_DATABASE_DSN_FILE'] = str(account_files['app'])
    scope = subprocess.check_output([str(args.glance_binary), 'store-scope'], env=env, text=True).strip()
    uuid.UUID(scope)
    store = args.root / 'glance-store'
    store.mkdir(mode=0o700)
    env.update(GLANCE_GO_STORE_ROOT=str(store), GLANCE_GO_STORE_SCOPE=scope)
    with (args.root / 'glance-store-init.log').open('w') as log:
        subprocess.run([str(args.glance_binary), 'store-init'], env=env, check=True, stdout=log, stderr=subprocess.STDOUT)
    settings = {'GLANCE_GO_DATABASE_DSN_FILE': str(account_files['app']), 'GLANCE_GO_STORE_ROOT': str(store),
                'GLANCE_GO_STORE_SCOPE': scope, 'GLANCE_GO_BINARY_ENABLED': 'true',
                'GLANCE_GO_KEYSTONE_AUTH_URL': url, 'GLANCE_GO_KEYSTONE_CREDENTIAL_FILE': str(auth_root / 'validator.json'),
                'GLANCE_GO_LISTEN_ADDR': '127.0.0.1:9293', 'GLANCE_GO_PUBLIC_BASE_URL': 'http://192.168.122.10/image',
                'GLANCE_GO_MAX_UPLOAD_BYTES': str(64 * 1024**2), 'GLANCE_GO_MAX_CONCURRENT_TRANSFERS': '2', 'GOMAXPROCS': '1'}
    save(args.root / 'glance.env', ''.join(k + '=' + v + '\n' for k, v in settings.items()))
    state['units'].append(combined['glance_unit'])
    save(args.root / 'state.json', state)
    placement_runtime.command(['sudo', 'systemd-run', '--unit=' + combined['glance_unit'], '--uid=' + str(os.getuid()),
                               '--collect', '--property=EnvironmentFile=' + str(args.root / 'glance.env'),
                               '--property=After=' + args.profile_unit, '--property=PartOf=' + args.profile_unit,
                               str(args.glance_binary), 'serve'])
    for _ in range(100):
        import socket
        try:
            with socket.create_connection(('127.0.0.1', 9293), timeout=1):
                break
        except OSError:
            time.sleep(.2)
    else:
        raise RuntimeError('Glance Go readiness failed; preserve private diagnostic')
    if not listener_owned_by(combined['glance_unit'], 9293) or not listener_owned_by(state['go_unit'], 18779):
        raise ValueError('API listener does not belong to the dedicated unit')
    combined['prepared'] = True
    combined['binary_sha256'] = {'glance': hashlib.sha256(args.glance_binary.read_bytes()).hexdigest(),
                                  'placement': hashlib.sha256(args.placement_binary.read_bytes()).hexdigest()}
    save(args.root / 'combined.json', combined)
    print(json.dumps({k: combined[k] for k in ('glance_unit', 'glance_schema', 'server_uuid', 'prepared', 'binary_sha256')}))



def match_database_baseline(args):
    """Match two measured controls; write only the dedicated experimental server."""
    import configparser
    import pymysql
    from sqlalchemy.engine import make_url
    baseline = configparser.ConfigParser(interpolation=None)
    baseline.read_string(subprocess.check_output(['sudo', 'cat', '/etc/placement/placement.conf'], text=True))
    url = make_url(baseline['placement_database']['connection'])
    query = 'SELECT @@server_uuid,@@port,@@innodb_buffer_pool_size,@@max_connections,@@innodb_flush_log_at_trx_commit,@@sync_binlog'
    with pymysql.connect(host=url.host, port=url.port or 3306, user=url.username, password=url.password, database=url.database, connect_timeout=5) as original:
        with original.cursor() as cursor:
            cursor.execute(query)
            observed = cursor.fetchone()
    if observed[1] != 3306 or not 8*1024**2 <= observed[2] <= 256*1024**2 or not 1 <= observed[3] <= 2048:
        raise ValueError('unexpected original DB controls')
    own = configparser.ConfigParser(interpolation=None)
    own.read(args.root / 'root.cnf')
    expected = json.loads((args.root / 'combined.json').read_text())
    config = args.root / 'mysql.cnf'
    before = config.read_bytes()
    marker = b'innodb-buffer-pool-size=134217728'
    if before.count(marker) != 1 or b'max-connections=' in before:
        raise ValueError('dedicated fresh DB config required')
    after = before.replace(marker, b'innodb-buffer-pool-size=' + str(observed[2]).encode() + b'\ninnodb-buffer-pool-chunk-size=8388608\nmax-connections=' + str(observed[3]).encode())
    save(args.root / 'mysql-before-matching.cnf', before.decode())
    with pymysql.connect(unix_socket=own['client']['socket'], user='root', password=own['client']['password'], connect_timeout=5) as db:
        with db.cursor() as cursor:
            cursor.execute(query)
            previous = cursor.fetchone()
            if previous[0] != expected['server_uuid'] or previous[0] == observed[0] or previous[1] != 13316:
                raise ValueError('independent owned server identity required')
    # Buffer chunk sizing is read at startup; dynamic shrinking may round up.
    # Restart only the UUID-verified dedicated native server, with durable config.
    save(config, after.decode())
    state = json.loads((args.root / 'state.json').read_text())
    placement_runtime.command(['sudo', 'systemctl', 'restart', state['db_unit']])
    current = None
    for _ in range(100):
        try:
            with pymysql.connect(unix_socket=own['client']['socket'], user='root', password=own['client']['password'], connect_timeout=2) as db:
                with db.cursor() as cursor:
                    cursor.execute(query)
                    current = cursor.fetchone()
            break
        except pymysql.Error:
            time.sleep(.2)
    if current is None or current[0] != expected['server_uuid'] or current[2:] != observed[2:]:
        raise ValueError('restarted DB controls not matched; preserve config and diagnostics')
    record = {'variables': ['server_uuid','port','innodb_buffer_pool_size','max_connections','innodb_flush_log_at_trx_commit','sync_binlog'],
              'original_read_only': list(observed), 'own_before': list(previous), 'own_after': list(current),
              'original_server_written': False, 'own_config_before_sha256': hashlib.sha256(before).hexdigest(),
              'own_config_after_sha256': hashlib.sha256(after).hexdigest()}
    save(args.root / 'database-matching.json', record)
    print(json.dumps(record))


def cleanup(args):
    auth_root = args.root.with_name(args.root.name + '-auth')
    # Routes must already be original; never dismantle a backend they still use.
    if any(address in Path('/etc/apache2/sites-enabled/glance-api.conf').read_text() for address in ('127.0.0.1:9293', '127.0.0.1:60998')):
        raise ValueError('restore original Glance route first')
    sandbox = None
    if args.root.exists():
        combined_file = args.root / 'combined.json'
        if combined_file.exists() and json.loads(combined_file.read_text()).get('imported_images'):
            raise ValueError('delete owned boot images through API before backend cleanup')
        if combined_file.exists():
            sandbox = json.loads(combined_file.read_text()).get('python_sandbox')
            if sandbox:
                state = json.loads((args.root / 'state.json').read_text())
                expected = '/run/systemd/system/tt-glance-python-bench-' + state['id'] + '.service'
                if sandbox['unit_file'] != expected or hashlib.sha256(Path(expected).read_bytes()).hexdigest() != sandbox['unit_sha256']:
                    raise ValueError('own Python unit changed outside experiment')
        placement_runtime.run(native_args(args, 'cleanup'))
        if sandbox:
            placement_runtime.command(['sudo', 'unlink', sandbox['unit_file']])
            placement_runtime.command(['sudo', 'systemctl', 'daemon-reload'])
    if auth_root.exists():
        identity = json.loads((auth_root / 'identity.json').read_text())
        admin, _ = admin_client()
        if identity.get('user_id'):
            from keystoneclient import exceptions
            try:
                user = admin.users.get(identity['user_id'])
                if user.name != identity['name']:
                    raise ValueError('validator ownership mismatch')
                admin.users.delete(user.id)
            except exceptions.NotFound:
                pass
        # Keep public identity metadata as evidence; remove only credential file.
        (auth_root / 'validator.json').unlink(missing_ok=True)
        identity['cleanup_verified'] = True
        save(auth_root / 'identity.json', identity)
    print(json.dumps({'cleanup_verified': True}))



def boot_image(args, action):
    from urllib.request import Request, urlopen
    # This module works both in repo scripts/integration and as an independently
    # deployed public artifact beside its scripts/ tree.
    for candidate in (Path(__file__).resolve().parents[1] / 'benchmarks', Path(__file__).resolve().parent / 'scripts/benchmarks'):
        if candidate.is_dir():
            sys.path.insert(0, str(candidate))
    from glance_benchmark import Client
    state_file = args.root / 'combined.json'
    state = json.loads(state_file.read_text())
    admin, _ = admin_client()
    token = admin.session.get_token()
    source = args.root / 'boot-source.bin'
    if action == 'cache-image':
        uuid.UUID(args.original_image_id)
        url = 'http://127.0.0.1:60999/v2/images/' + args.original_image_id
        with urlopen(Request(url, headers={'X-Auth-Token': token}), timeout=15) as response:
            image = json.load(response)
        if image['id'] != args.original_image_id or image['status'] != 'active' or image.get('os_hash_algo') != 'sha512' or not 1 <= image['size'] <= 64*1024**2:
            raise ValueError('small active original boot image required')
        expected = image['os_hash_value']
        h = hashlib.sha512()
        size = 0
        with source.open('xb') as output, urlopen(Request(url + '/file', headers={'X-Auth-Token': token}), timeout=30) as response:
            while True:
                block = response.read(256*1024)
                if not block:
                    break
                size += len(block)
                if size > image['size']:
                    raise ValueError('original image length exceeded')
                h.update(block)
                output.write(block)
        if size != image['size'] or h.hexdigest() != expected:
            raise ValueError('original image integrity failed')
        state['boot_source'] = {'original_id': image['id'], 'sha512': expected, 'bytes': size,
                                'disk_format': image['disk_format'], 'container_format': image['container_format'],
                                'min_disk': image.get('min_disk', 0), 'min_ram': image.get('min_ram', 0)}
        save(state_file, state)
        print(json.dumps(state['boot_source']))
        return
    client = Client('http://127.0.0.1:9293', token, 30, 64*1024**2)
    try:
        if action == 'import-image':
            original = state['boot_source']
            ident = str(uuid.uuid4())
            tag = 'tt-runtime-' + args.root.name
            body = {'id': ident, 'name': 'tt-boot-' + ident, 'visibility': 'private', 'tags': [tag],
                    **{k: original[k] for k in ('disk_format', 'container_format', 'min_disk', 'min_ram')}}
            state['imported_images'].append({'id': ident, 'name': body['name'], 'tag': tag})
            save(state_file, state)  # durable intent before mutation
            reply, value = client.request('POST', '/v2/images', body=body)
            if reply['status'] != 201 or reply['error_kind'] or value.get('id') != ident:
                raise ValueError('boot image create failed')
            reply, _ = client.request('PUT', '/v2/images/' + ident + '/file', upload=source)
            if reply['status'] != 204 or reply['error_kind']:
                raise ValueError('boot image upload failed')
            reply, value = client.request('GET', '/v2/images/' + ident)
            if reply['status'] != 200 or value.get('status') != 'active' or value.get('os_hash_value') != original['sha512'] or value.get('size') != original['bytes']:
                raise ValueError('boot image metadata integrity failed')
            reply, _ = client.request('GET', '/v2/images/' + ident + '/file', binary=True)
            if reply['status'] != 200 or reply['response_bytes'] != original['bytes'] or reply['digests']['sha512'] != original['sha512']:
                raise ValueError('boot image download integrity failed')
            state['boot_image_id'] = ident
            save(state_file, state)
            print(json.dumps({'image_id': ident, 'sha512': original['sha512'], 'bytes': original['bytes'], 'integrity_verified': True}))
        else:
            for item in list(state['imported_images']):
                reply, image = client.request('GET', '/v2/images/' + item['id'])
                if reply['status'] != 404:
                    if reply['status'] != 200 or image.get('name') != item['name'] or item['tag'] not in image.get('tags', []):
                        raise ValueError('owned boot image mismatch')
                    deleted, _ = client.request('DELETE', '/v2/images/' + item['id'])
                    if deleted['status'] != 204:
                        raise ValueError('boot image delete failed')
                absent, _ = client.request('GET', '/v2/images/' + item['id'])
                if absent['status'] != 404:
                    raise ValueError('boot image absence unconfirmed')
                state['imported_images'].remove(item)
                save(state_file, state)
            print(json.dumps({'owned_boot_images_absent': True}))
    finally:
        client.close()


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('action', choices=('prepare', 'cleanup', 'cache-image', 'import-image', 'delete-images', 'match-db-baseline'))
    p.add_argument('--root', type=Path, required=True)
    p.add_argument('--placement-binary', type=Path)
    p.add_argument('--glance-binary', type=Path)
    p.add_argument('--profile-unit')
    p.add_argument('--original-image-id')
    p.add_argument('--allow-mutations', action='store_true')
    args = p.parse_args()
    if not args.allow_mutations:
        p.error('explicit opt-in required')
    try:
        validate_runtime_root(args.root)
        if args.action == 'prepare':
            prepare(args)
        elif args.action == 'match-db-baseline':
            match_database_baseline(args)
        elif args.action == 'cleanup':
            cleanup(args)
        else:
            boot_image(args, args.action)
        return 0
    except Exception as error:
        print('combined runtime failed: ' + type(error).__name__, file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
