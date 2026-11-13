"""B26 suite and Nova → Glance flow capture: offline guards, parsing and acceptance."""
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest

SCRIPTS = Path(__file__).resolve().parents[2] / 'scripts/integration'
sys.path.insert(0, str(SCRIPTS))


def load(name):
    spec = importlib.util.spec_from_file_location(name, SCRIPTS / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


flow = load('glance_flow')
suite = load('glance_suite')

IMAGE = '3f0b7c9e-1d2a-4b5c-8d6e-7f8091a2b3c4'
RID_META, RID_FILE, RID_API = ('req-' + '1' * 8 + '-1111-4111-8111-' + '1' * 12,
                              'req-' + '2' * 8 + '-2222-4222-8222-' + '2' * 12,
                              'req-' + '3' * 8 + '-3333-4333-8333-' + '3' * 12)
TOKEN = 'gAAAAABsecretTOKENvalue'
SIZE = 21430272
T0 = 1_700_000_000_000_000  # µs


def go_line(ts, rid, method, route, status, size, ms=4):
    message = json.dumps({'time': 'x', 'level': 'INFO', 'msg': 'request completed', 'request_id': rid,
                          'method': method, 'route': route, 'status': status, 'duration_ms': ms,
                          'response_bytes': size, 'error_kind': ''})
    return json.dumps({'__REALTIME_TIMESTAMP': str(ts), 'MESSAGE': message})


def nova_line(ts, unit, message):
    return json.dumps({'__REALTIME_TIMESTAMP': str(ts), '_SYSTEMD_UNIT': unit, 'MESSAGE': message})


def apache_line(when, method, path, status, size, agent):
    return f'192.168.122.10 - - [{when}] "{method} {path} HTTP/1.1" {status} {size} "-" "{agent}"'


GO = [go_line(T0 + 100_000, RID_API, 'GET', '/v2/images/{id}', 200, 900),
      go_line(T0 + 4_000_000, RID_META, 'GET', '/v2/images/{id}', 200, 900),
      go_line(T0 + 4_500_000, RID_FILE, 'GET', '/v2/images/{id}/file', 200, SIZE, ms=180)]
URL = f'http://192.168.122.10/image/v2/images/{IMAGE}'
NOVA = [
    nova_line(T0 + 90_000, 'devstack@n-api.service',
              f'REQ: curl -g -i -X GET {URL} -H "User-Agent: nova-api keystoneauth1/5" -H "X-Auth-Token: {{SHA256}}{TOKEN}" -H "X-Service-Token: {{SHA256}}abc"'),
    nova_line(T0 + 110_000, 'devstack@n-api.service', f'GET call to image for {URL} used request id {RID_API}'),
    nova_line(T0 + 3_990_000, 'devstack@n-cpu.service',
              f'REQ: curl -g -i -X GET {URL} -H "User-Agent: nova-compute" -H "X-Auth-Token: {{SHA256}}{TOKEN}" -H "X-Service-Token: {{SHA256}}abc"'),
    nova_line(T0 + 4_010_000, 'devstack@n-cpu.service', f'GET call to image for {URL} used request id {RID_META}'),
    nova_line(T0 + 4_510_000, 'devstack@n-cpu.service', f'GET call to image for {URL}/file used request id {RID_FILE}'),
    nova_line(T0 + 5_000_000, 'devstack@n-cpu.service', 'unrelated line with password=hunter2'),
]


class FlowParsingTests(unittest.TestCase):
    def test_route_templates_hide_ids_and_reject_foreign_paths(self):
        self.assertEqual(flow.route(f'/image/v2/images/{IMAGE}/file'), '/v2/images/{id}/file')
        self.assertEqual(flow.route(f'/v2/images/{IMAGE}?x=1'), '/v2/images/{id}')
        self.assertEqual(flow.route('/image/v2/images'), '/v2/images')
        self.assertEqual(flow.route('/image/'), '/')
        self.assertEqual(flow.route('/placement/resource_providers'), 'other')
        self.assertEqual(flow.route('/image/v2/images/not-a-uuid'), 'other')

    def test_go_journal_only_request_records_with_valid_ids(self):
        noise = [json.dumps({'__REALTIME_TIMESTAMP': '1', 'MESSAGE': '{"msg":"image API listening"}'}),
                 go_line(T0, 'req-bad', 'GET', '/v2/images', 200, 1), 'not json']
        events = flow.go_events(GO + noise)
        self.assertEqual([e['request_id'] for e in events], [RID_API, RID_META, RID_FILE])
        self.assertIsNone(events[0]['error_kind'])

    def test_nova_lines_keep_presence_never_token_values(self):
        calls, requests = flow.nova_events(NOVA)
        self.assertEqual([c['source'] for c in calls], ['n-api', 'n-cpu', 'n-cpu'])
        self.assertTrue(all(r['x_service_token'] and r['x_auth_token'] for r in requests))
        dumped = json.dumps([calls, requests])
        for secret in (TOKEN, 'hunter2', IMAGE, 'SHA256'):
            self.assertNotIn(secret, dumped)

    def test_apache_window_and_scope(self):
        lines = [apache_line('14/Nov/2023:22:13:20 +0000', 'GET', f'/image/v2/images/{IMAGE}/file', 200, SIZE, 'python-glanceclient'),
                 apache_line('14/Nov/2023:23:59:00 +0000', 'GET', f'/image/v2/images/{IMAGE}', 200, 9, 'x'),
                 apache_line('14/Nov/2023:22:13:21 +0000', 'GET', '/placement/', 200, 9, 'x'),
                 'garbage']
        events = flow.apache_events(lines, T0, T0 + 10_000_000)
        self.assertEqual(len(events), 1)
        self.assertEqual((events[0]['route'], events[0]['user_agent_class']), ('/v2/images/{id}/file', 'openstack-client'))


class FlowAcceptanceTests(unittest.TestCase):
    def test_request_id_correlation_reports_callers_and_composite_auth(self):
        calls, requests = flow.nova_events(NOVA)
        timeline = flow.correlate(flow.go_events(GO), calls, requests)
        summary = flow.summarize(timeline, SIZE)
        self.assertEqual(summary['callers'], ['n-api', 'n-cpu'])
        self.assertEqual(summary['request_id_correlated'], 3)
        self.assertTrue(summary['composite_auth_seen'])
        self.assertTrue(flow.flow_accepted(summary))
        self.assertEqual(timeline[0]['t_ms'], 0.0)
        diagram = flow.mermaid(timeline)
        self.assertIn('NovaCompute->>Apache: GET /v2/images/{id} +X-Service-Token', diagram)
        # No REQ line was logged for the download: presence stays unknown, never inferred.
        self.assertIsNone(timeline[-1]['x_service_token'])
        self.assertIn('NovaCompute->>Apache: GET /v2/images/{id}/file\n', diagram)

    def test_apache_fallback_when_keystoneauth_debug_is_off(self):
        go = flow.go_events(GO[1:])
        apache = [{'ts_us': T0 + 4_000_000, 'method': 'GET', 'route': '/v2/images/{id}', 'status': 200, 'user_agent_class': 'openstack-client'},
                  {'ts_us': T0 + 5_000_000, 'method': 'GET', 'route': '/v2/images/{id}/file', 'status': 200, 'user_agent_class': 'openstack-client'}]
        summary = flow.summarize(flow.correlate(go, [], [], apache), SIZE)
        self.assertEqual((summary['attributed_by_apache'], summary['request_id_correlated']), (2, 0))
        self.assertIsNone(summary['composite_auth_seen'])  # unknown, not False
        self.assertTrue(flow.flow_accepted(summary))

    def test_partial_download_or_foreign_traffic_is_not_a_pass(self):
        calls, requests = flow.nova_events(NOVA)
        short = flow.go_events(GO[:2] + [go_line(T0 + 4_500_000, RID_FILE, 'GET', '/v2/images/{id}/file', 200, SIZE - 1)])
        self.assertFalse(flow.flow_accepted(flow.summarize(flow.correlate(short, calls, requests), SIZE)))
        foreign = flow.go_events(GO + [go_line(T0 + 6_000_000, 'req-' + '4' * 8 + '-4444-4444-8444-' + '4' * 12, 'GET', '/v2/images', 200, 5)])
        summary = flow.summarize(flow.correlate(foreign, calls, requests), SIZE)
        self.assertEqual(summary['non_nova_glance_requests'], 1)
        self.assertFalse(flow.flow_accepted(summary))

    def test_go_discovery_route_pairs_with_apache_root(self):
        self.assertTrue(flow.same_route('unmatched', '/'))
        self.assertTrue(flow.same_route('unmatched', '/v2/schemas/{name}'))
        self.assertFalse(flow.same_route('unmatched', '/v2/images'))
        self.assertEqual(flow.route('/image/v2/schemas/image'), '/v2/schemas/{name}')

    def test_exact_window_drops_the_suite_calls_just_before_the_boot(self):
        # First real run: journalctl's whole-second filter let the suite's own
        # upload, ~1 s before create_server, into the Nova window.
        events = flow.go_events(GO)
        kept = flow.within(events, T0 + 3_000_000, T0 + 5_000_000)
        self.assertEqual([e['request_id'] for e in kept], [RID_META, RID_FILE])


class SuiteGuardTests(unittest.TestCase):
    def test_opt_in_precedes_any_file_or_lab_action(self):
        with self.assertRaisesRegex(ValueError, 'opt-in'):
            suite.run(SimpleNamespace(allow_mutations=False))

    def test_existing_evidence_refused(self):
        with tempfile.TemporaryDirectory() as folder:
            with self.assertRaises(FileExistsError):
                suite.run(SimpleNamespace(allow_mutations=True, output=Path(folder)))

    def test_bounded_sizes_and_work_root(self):
        with tempfile.TemporaryDirectory() as folder:
            base = Path(folder).resolve()
            args = dict(allow_mutations=True, binary=Path(__file__), wait_seconds=180, lease_seconds=900, payload_mib=[1])
            with self.assertRaisesRegex(ValueError, 'work root'):
                suite.run(SimpleNamespace(output=base / 'a', work_root=base / 'other', **args))
            args['payload_mib'] = [2]
            with self.assertRaisesRegex(ValueError, 'payload'):
                suite.run(SimpleNamespace(output=base / 'b', work_root=base / 'tt-placement-x', **args))

    def test_pass_requires_every_case_cleanup_and_no_pending_image(self):
        report = {'execution_error': None, 'cases': {n: {'passed': True} for n in suite.CASES},
                  'cleanup': {**{n: True for n in suite.CLEANUP}, 'pending_images': 0}}
        self.assertTrue(suite.suite_passed(report))
        report['cleanup']['pending_images'] = 1
        self.assertFalse(suite.suite_passed(report))
        report['cleanup']['pending_images'] = 0
        report['cases']['nova_glance_flow']['passed'] = False
        self.assertFalse(suite.suite_passed(report))

    def test_placement_is_a_read_only_control_not_an_empty_lab_gate(self):
        base = {'providers': [{'allocations': {'allocations': {'other-server': {}}}}],
                'flat_providers': True, 'no_sharing': True, 'no_provider_allocations': False}
        self.assertTrue(suite.placement_control_ok(base))
        self.assertFalse(suite.placement_control_ok({**base, 'providers': []}))
        self.assertFalse(suite.placement_control_ok({**base, 'no_sharing': False}))

    def test_placement_control_action_records_log_and_does_not_mutate(self):
        class FakePreflight:
            @staticmethod
            def run(values):
                values.output.mkdir()
                (values.output / 'report.json').write_text(json.dumps(
                    {'providers': [{}], 'flat_providers': True, 'no_sharing': True, 'no_provider_allocations': False}))
                print('{"no_allocations": false}')
                return 1  # the campaign gate would fail; the Glance control must not
        with tempfile.TemporaryDirectory() as folder:
            out = Path(folder)
            fake = suite.Suite.__new__(suite.Suite)
            fake.args = SimpleNamespace(output=out)
            fake.action('placement-before', FakePreflight, url='x', token_file=out / 't', output=out / 'placement-before')
            self.assertIn('no_allocations', (out / 'placement-before.log').read_text())

    def test_placement_generations_are_not_a_side_effect(self):
        before = {'providers': [{'provider': {'uuid': 'p', 'generation': 11},
                                 'inventories': {'resource_provider_generation': 11, 'inventories': {'VCPU': {'total': 4}}},
                                 'allocations': {'resource_provider_generation': 11, 'allocations': {'old': {}}}}]}
        after = json.loads(json.dumps(before).replace('11', '12'))
        self.assertNotEqual(before, after)
        self.assertEqual(suite.without_generations(before), suite.without_generations(after))
        changed = json.loads(json.dumps(after).replace('"old"', '"leaked"'))
        self.assertNotEqual(suite.without_generations(before), suite.without_generations(changed))

    def test_server_image_id_accepts_sdk_shapes(self):
        self.assertEqual(suite.server_image_id(SimpleNamespace(image={'id': IMAGE})), IMAGE)
        self.assertEqual(suite.server_image_id(SimpleNamespace(image=SimpleNamespace(id=IMAGE))), IMAGE)


if __name__ == '__main__':
    unittest.main()
