#!/usr/bin/env python3
"""Nova → Glance data-flow capture for one bounded boot window.

Correlates three sources without changing any service:
- Glance Go journald: one JSON "request completed" record per request, with the
  X-Openstack-Request-Id it returned (route already anonymised by the service).
- Nova journald (n-api, n-cpu, conductors): keystoneauth DEBUG lines
  "<METHOD> call to image for <url> used request id <req-id>" and the REQ lines,
  from which only header *presence* is kept (X-Service-Token = composite auth).
- Apache access log: method, path, status, bytes and a coarse client class.

Tokens, token hashes, image IDs and hostnames never reach the output: IDs are
replaced by route templates and lines are reduced to whitelisted fields.
"""
import argparse
import datetime
import json
from pathlib import Path
import re
import subprocess
import sys

UUID = r'[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}'
REQUEST_ID = re.compile(r'\breq-' + UUID + r'\b')
NOVA_UNITS = ('devstack@n-api.service', 'devstack@n-cpu.service',
              'devstack@n-cond-cell1.service', 'devstack@n-super-cond.service')
CALL = re.compile(r'\b(GET|HEAD|POST|PUT|PATCH|DELETE) call to image for (\S+) used request id (req-' + UUID + r')')
REQ = re.compile(r'REQ: curl .*?-X (GET|HEAD|POST|PUT|PATCH|DELETE) (\S+)(.*)$')
APACHE = re.compile(r'^\S+ \S+ \S+ \[([^\]]+)\] "(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS) (\S+) HTTP/[0-9.]+" (\d{3}) (\d+|-)'
                    r'(?: "[^"]*" "([^"]*)")?')
SUITE_AGENT = 'tt-glance-suite'


def route(path):
    """Template a Glance path; anything outside the image API is 'other'."""
    path = path.split('?', 1)[0]
    for prefix in ('/image', ''):
        if path.startswith(prefix + '/v2/') or path in (prefix + '/', prefix):
            path = path[len(prefix):] or '/'
            break
    else:
        return 'other'
    if path == '/':
        return '/'
    if path == '/v2/images':
        return '/v2/images'
    match = re.fullmatch(r'/v2/images/' + UUID + r'(/file)?', path)
    if match:
        return '/v2/images/{id}' + (match.group(1) or '')
    return 'other'


def url_path(url):
    return re.sub(r'^[a-z]+://[^/]+', '', url)


def go_events(journal_lines):
    """journalctl -o json lines of the Glance Go unit → request events."""
    events = []
    for line in journal_lines:
        try:
            entry = json.loads(line)
            record = json.loads(entry['MESSAGE'])
        except (ValueError, KeyError, TypeError):
            continue
        if record.get('msg') != 'request completed':
            continue
        rid = record.get('request_id')
        if not isinstance(rid, str) or not REQUEST_ID.fullmatch(rid):
            continue
        events.append({'ts_us': int(entry['__REALTIME_TIMESTAMP']), 'source': 'glance-go',
                       'request_id': rid, 'method': record.get('method'), 'route': record.get('route'),
                       'status': record.get('status'), 'duration_ms': record.get('duration_ms'),
                       'response_bytes': record.get('response_bytes'), 'error_kind': record.get('error_kind') or None})
    return events


def nova_events(journal_lines):
    """journalctl -o json lines of Nova units → image calls and header presence."""
    calls, requests = [], []
    for line in journal_lines:
        try:
            entry = json.loads(line)
            message = entry['MESSAGE']
            ts = int(entry['__REALTIME_TIMESTAMP'])
        except (ValueError, KeyError, TypeError):
            continue
        if not isinstance(message, str):
            continue
        unit = str(entry.get('_SYSTEMD_UNIT', ''))
        caller = unit.removeprefix('devstack@').removesuffix('.service') or 'nova'
        found = CALL.search(message)
        if found:
            calls.append({'ts_us': ts, 'source': caller, 'method': found.group(1),
                          'route': route(url_path(found.group(2))), 'request_id': found.group(3)})
            continue
        found = REQ.search(message)
        if found and route(url_path(found.group(2))) != 'other':
            headers = found.group(3)
            requests.append({'ts_us': ts, 'source': caller, 'method': found.group(1),
                             'route': route(url_path(found.group(2))),
                             'x_auth_token': 'X-Auth-Token:' in headers,
                             'x_service_token': 'X-Service-Token:' in headers,
                             'user_agent_class': agent_class(_header(headers, 'User-Agent'))})
    return calls, requests


def _header(headers, name):
    found = re.search(r'-H "' + re.escape(name) + r': ([^"]*)"', headers)
    return found.group(1) if found else ''


def agent_class(agent):
    agent = agent or ''
    if SUITE_AGENT in agent:
        return 'suite'
    if 'nova' in agent.lower():
        return 'nova'
    if 'glanceclient' in agent or 'keystoneauth' in agent:
        return 'openstack-client'
    if 'openstacksdk' in agent:
        return 'openstacksdk'
    return 'other' if agent else 'unknown'


def apache_events(lines, since_us, until_us):
    events = []
    for line in lines:
        found = APACHE.match(line)
        if not found:
            continue
        stamp = datetime.datetime.strptime(found.group(1), '%d/%b/%Y:%H:%M:%S %z')
        ts = int(stamp.timestamp() * 1_000_000)
        # Apache has second resolution: keep the whole boundary second.
        if not since_us - 1_000_000 <= ts <= until_us + 1_000_000:
            continue
        path_route = route(found.group(3))
        if path_route == 'other' or not found.group(3).startswith('/image'):
            continue
        events.append({'ts_us': ts, 'source': 'apache', 'method': found.group(2), 'route': path_route,
                       'status': int(found.group(4)),
                       'response_bytes': None if found.group(5) == '-' else int(found.group(5)),
                       'user_agent_class': agent_class(found.group(6))})
    return events


def correlate(go, calls, requests, apache=()):
    """One timeline entry per Glance Go response (authoritative status/bytes/time).

    Attribution, strongest first: Nova keystoneauth "used request id" line
    (exact; also yields header presence) → Apache line with the same method,
    route and status within 2 s (client class only) → 'unattributed'.
    """
    by_id = {c['request_id']: c for c in calls}
    pool = sorted(apache, key=lambda e: e['ts_us'])
    used = set()
    timeline = []
    for event in sorted(go, key=lambda e: e['ts_us']):
        call = by_id.get(event['request_id'])
        caller, how, service_token = 'unattributed', None, None
        if call:
            caller, how = call['source'], 'request-id'
            presence = _nearest_request(call, requests)
            service_token = presence['x_service_token'] if presence else None
        else:
            for index, line in enumerate(pool):
                if (index not in used and line['method'] == event['method'] and same_route(event['route'], line['route'])
                        and line['status'] == event['status'] and abs(line['ts_us'] - event['ts_us']) <= 2_000_000):
                    used.add(index)
                    cls = line['user_agent_class']
                    caller = 'nova' if cls in ('nova', 'openstack-client') else cls
                    how = 'apache'
                    break
        timeline.append({'ts_us': event['ts_us'], 'caller': caller, 'attribution': how,
                         'method': event['method'], 'route': event['route'], 'status': event['status'],
                         'response_bytes': event['response_bytes'], 'glance_duration_ms': event['duration_ms'],
                         'request_id_correlated': how == 'request-id', 'x_service_token': service_token})
    origin = timeline[0]['ts_us'] if timeline else 0
    for event in timeline:
        event['t_ms'] = round((event.pop('ts_us') - origin) / 1000, 1)
    return timeline


def same_route(go_route, apache_route):
    # Glance Go logs version discovery ("/") as "unmatched" by design.
    return go_route == apache_route or (go_route == 'unmatched' and apache_route == '/')


def is_nova(caller):
    return caller == 'nova' or caller.startswith('n-')


def _nearest_request(call, requests):
    """REQ precedes its 'call ... used request id' line in the same process."""
    candidates = [r for r in requests if r['source'] == call['source'] and r['method'] == call['method']
                  and r['route'] == call['route'] and r['ts_us'] <= call['ts_us']]
    return max(candidates, key=lambda r: r['ts_us']) if candidates else None


def summarize(timeline, image_size):
    nova = [e for e in timeline if is_nova(e['caller'])]
    downloads = [e for e in nova if e['method'] == 'GET' and e['route'] == '/v2/images/{id}/file']
    metadata = [e for e in nova if e['method'] == 'GET' and e['route'] == '/v2/images/{id}']
    presence = [e['x_service_token'] for e in nova if e['x_service_token'] is not None]
    return {
        'nova_calls': len(nova),
        'request_id_correlated': sum(e['request_id_correlated'] for e in nova),
        'attributed_by_apache': sum(e['attribution'] == 'apache' for e in nova),
        'callers': sorted({e['caller'] for e in nova}),
        'metadata_reads_200': sum(e['status'] == 200 for e in metadata),
        'downloads_200_full': sum(e['status'] == 200 and e['response_bytes'] == image_size for e in downloads),
        # None = not observable (keystoneauth DEBUG disabled); never guessed.
        'composite_auth_seen': any(presence) if presence else None,
        'non_nova_glance_requests': len(timeline) - len(nova),
    }


def flow_accepted(summary):
    """Minimum observable B26 flow: Nova read metadata and downloaded all bytes from Go,
    with every Glance Go response in the exclusive window attributed to Nova."""
    return (summary['metadata_reads_200'] >= 1 and summary['downloads_200_full'] >= 1
            and summary['non_nova_glance_requests'] == 0)


def mermaid(timeline):
    names = {'n-api': 'NovaAPI', 'n-cpu': 'NovaCompute', 'n-cond-cell1': 'NovaConductor',
             'n-super-cond': 'NovaConductor', 'nova': 'Nova'}
    lines = ['sequenceDiagram', '    participant Apache as Apache /image', '    participant Go as Glance Go']
    for event in timeline:
        actor = names.get(event['caller'], 'Otro')
        auth = ' +X-Service-Token' if event.get('x_service_token') else ''
        lines.append(f"    {actor}->>Apache: {event['method']} {event['route']}{auth}")
        lines.append(f"    Apache->>Go: {event['method']} {event['route']}")
        size = '' if event['response_bytes'] is None else f", {event['response_bytes']} B"
        took = '' if event['glance_duration_ms'] is None else f", {event['glance_duration_ms']} ms"
        lines.append(f"    Go-->>{actor}: {event['status']}{size}{took} (t={event['t_ms']} ms)")
    return '\n'.join(lines) + '\n'


def journal(units, since_us, until_us):
    args = ['sudo', '-n', 'journalctl', '--no-pager', '-o', 'json',
            '--since', '@%d' % (since_us // 1_000_000), '--until', '@%d' % (until_us // 1_000_000 + 1)]
    for unit in units:
        args += ['-u', unit]
    return subprocess.run(args, check=True, capture_output=True, text=True).stdout.splitlines()


def collect(go_unit, since_us, until_us, image_size, apache_log=Path('/var/log/apache2/horizon_access.log')):
    """Live collection inside the VM. Returns a sanitized, self-contained record."""
    go = go_events(journal([go_unit], since_us, until_us))
    calls, requests = nova_events(journal(NOVA_UNITS, since_us, until_us))
    try:
        lines = subprocess.run(['sudo', '-n', 'tail', '-n', '20000', str(apache_log)],
                               check=True, capture_output=True, text=True).stdout.splitlines()
    except subprocess.CalledProcessError:
        lines = []
    apache = apache_events(lines, since_us, until_us)
    timeline = correlate(go, calls, requests, apache)
    summary = summarize(timeline, image_size)
    return {'schema': 1, 'window_s': round((until_us - since_us) / 1_000_000, 1), 'timeline': timeline,
            'apache': [{k: v for k, v in e.items() if k != 'ts_us'} for e in apache],
            'summary': summary, 'accepted': flow_accepted(summary)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--go-unit', required=True)
    parser.add_argument('--since-us', type=int, required=True)
    parser.add_argument('--until-us', type=int, required=True)
    parser.add_argument('--image-size', type=int, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    record = collect(args.go_unit, args.since_us, args.until_us, args.image_size)
    (args.output / 'flow.json').write_text(json.dumps(record, indent=2) + '\n')
    (args.output / 'flow.mmd').write_text(mermaid(record['timeline']))
    print(json.dumps(record['summary']))
    return 0 if record['accepted'] else 1


if __name__ == '__main__':
    sys.exit(main())
