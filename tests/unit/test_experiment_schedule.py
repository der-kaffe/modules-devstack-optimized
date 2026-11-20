import importlib.util
import json
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('plan_experiments', ROOT / 'scripts/benchmarks/plan_experiments.py')
p = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(p)


class ScheduleTests(unittest.TestCase):
    def test_glance_reproducible_alternating_pairs(self):
        protocol = json.loads((ROOT / 'docs/benchmarks/glance/protocol-v1.json').read_text())
        campaign = '10000000-0000-4000-8000-000000000001'
        a = p.schedule(protocol, 'glance', campaign, False)
        self.assertEqual(a, p.schedule(protocol, 'glance', campaign, False))
        self.assertFalse(a['execution_authorized_by_plan'])
        self.assertEqual(len(a['blocks']), 300)
        self.assertEqual(len({b['campaign_id'] for b in a['blocks']}), 300)
        for i in range(0, len(a['blocks']), 5):
            group = a['blocks'][i:i+5]
            for x, y in zip(group, group[1:]):
                self.assertEqual(x['order'], list(reversed(y['order'])))
            self.assertEqual({b['payload_bytes'] for b in group}, {group[0]['payload_bytes']})

    def test_integrated_blocks_have_all_four_cells(self):
        protocol = json.loads((ROOT / 'docs/benchmarks/integrated/protocol-v1.json').read_text())
        a = p.schedule(protocol, 'integrated', '10000000-0000-4000-8000-000000000002', False)
        self.assertEqual(len(a['blocks']), 5)
        for b in a['blocks']:
            self.assertEqual(set(b['order']), set('ABCD'))
        for column in zip(*(b['order'] for b in a['blocks'][:4])):
            self.assertEqual(set(column), set('ABCD'))
        self.assertEqual(len(p.schedule(protocol, 'integrated', a['campaign_id'], True)['blocks']), 1)


if __name__ == '__main__':
    unittest.main()
