import argparse
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace as NS

SPEC = importlib.util.spec_from_file_location('integrated_boot', Path(__file__).resolve().parents[2] / 'scripts/benchmarks/integrated_boot.py')
b = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(b)


class Gateway:
    def __init__(self):
        self.items = {'foreign': {'id': 'foreign', 'name': 'retained-B26', 'metadata': {}, 'status': 'SHUTOFF'}}
        self.raise_after_create = False
        self.bad_allocation = False
        self.deleted = []
        self.root = None

    def servers(self):
        return list(self.items.values())

    def inputs(self, args):
        return {'image_id': 'image', 'flavor_id': 'flavor', 'network_id': 'network',
                'resources': {'VCPU': 1, 'MEMORY_MB': 512, 'DISK_GB': 1}}

    def create(self, name, campaign, inputs):
        intent = json.loads((self.root / 'report.json').read_text())
        assert intent['state'] == 'CREATE_INTENT' and intent['server_id'] is None
        self.items['owned'] = {'id': 'owned', 'name': name, 'metadata': {'tt_campaign': campaign}, 'status': 'ACTIVE'}
        if self.raise_after_create:
            raise TimeoutError('response lost after server created')
        return 'owned'

    def get(self, ident):
        return self.items.get(ident)

    def delete(self, ident):
        self.deleted.append(ident)
        self.items.pop(ident, None)

    def allocations(self, ident):
        if ident not in self.items:
            return {}
        return {'provider': {'resources': {'VCPU': 1, 'MEMORY_MB': 512, 'DISK_GB': 2 if self.bad_allocation else 1}}}


class IntegratedBootTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.args = argparse.Namespace(output=Path(self.tmp.name) / 'run', configuration='D',
                                       allow_mutations=True, boot_timeout=1, delete_timeout=1, poll=.01)
        self.gateway = Gateway()
        self.gateway.root = self.args.output

    def test_active_and_cleanup_are_observations_not_joint_gate(self):
        r = b.observe(self.args, self.gateway)
        self.assertTrue(r['boot_active'])
        self.assertTrue(r['allocation_verified'])
        self.assertTrue(r['cleanup_verified'])
        self.assertFalse(r['gate_accepted'])
        self.assertFalse(r['deployment_identity_verified'])
        self.assertFalse(r['official_accepted'])
        self.assertEqual(self.gateway.deleted, ['owned'])
        self.assertEqual(set(self.gateway.items), {'foreign'})
        b.cleanup(self.gateway, self.args.output, r, 1, .01)
        self.assertEqual(self.gateway.deleted, ['owned'])

    def test_ambiguous_post_recovery_uses_durable_intent(self):
        self.gateway.raise_after_create = True
        r = b.observe(self.args, self.gateway)
        self.assertEqual(r['state'], 'NEEDS_ATTENTION')
        self.assertEqual(r['execution_error'], 'TimeoutError')
        self.assertTrue(r['cleanup_verified'])
        self.assertEqual(self.gateway.deleted, ['owned'])

    def test_wrong_allocations_fail_even_when_ACTIVE(self):
        self.gateway.bad_allocation = True
        r = b.observe(self.args, self.gateway)
        self.assertTrue(r['boot_active'])
        self.assertFalse(r['allocation_verified'])
        self.assertTrue(r['cleanup_verified'])
        self.assertEqual(r['state'], 'NEEDS_ATTENTION')

    def test_foreign_metadata_never_deleted(self):
        r = b.observe(self.args, self.gateway)
        self.gateway.items['other'] = {'id': 'other', 'name': r['name'], 'metadata': {'tt_campaign': 'foreign'}}
        with self.assertRaises(ValueError):
            b.cleanup(self.gateway, self.args.output, r, 1, .01)
        self.assertNotIn('other', self.gateway.deleted)

    def test_sdk_image_uses_documented_hash_property_names(self):
        gateway = object.__new__(b.SDKGateway)
        image = NS(id='image', status='active', hash_algo='sha512', hash_value='a'*128)
        flavor = NS(id='flavor', ram=256, disk=1, vcpus=1)
        gateway.connection = NS(image=NS(get_image=lambda ident: image),
                                compute=NS(get_flavor=lambda ident: flavor),
                                network=NS(get_network=lambda ident: NS(id='network')))
        args = NS(image_id='image', image_sha512='a'*128, flavor_id='flavor', network_id='network')
        result = gateway.inputs(args)
        self.assertEqual(result['image_sha512'], 'a'*128)
        self.assertEqual(result['resources']['MEMORY_MB'], 256)
        image.hash_value = 'b'*128
        with self.assertRaises(ValueError):
            gateway.inputs(args)

    def test_no_mutation_without_opt_in(self):
        self.args.allow_mutations = False
        with self.assertRaises(ValueError):
            b.observe(self.args, self.gateway)
        self.assertFalse(self.args.output.exists())
        self.assertEqual(self.gateway.deleted, [])


if __name__ == '__main__':
    unittest.main()
