import configparser
import importlib.util
from pathlib import Path
import sys
import unittest

ROOT=Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/'scripts/integration'))
S=importlib.util.spec_from_file_location('glance_python_sandbox',ROOT/'scripts/integration/glance_python_sandbox.py')
p=importlib.util.module_from_spec(S)
S.loader.exec_module(p)


class PythonIsolationTests(unittest.TestCase):
    def test_private_store_db_and_logs_preserve_authentication(self):
        original='[DEFAULT]\nenabled_backends=fast:file\n[database]\nconnection=original-fixture\n[fast]\nfilesystem_store_datadir=/original/store\n[keystone_authtoken]\npassword=fixture-only\nauth_url=http://keystone/identity\n'
        changed,names=p.private_configuration(original,Path('/private/runtime'),'private-fixture')
        parsed=configparser.ConfigParser(interpolation=None)
        parsed.read_string(changed)
        self.assertEqual(parsed['database']['connection'],'private-fixture')
        self.assertEqual(parsed['keystone_authtoken']['password'],'fixture-only')
        self.assertEqual(parsed['keystone_authtoken']['auth_url'],'http://keystone/identity')
        self.assertNotIn('/original/store',changed)
        for name in names:
            self.assertTrue(parsed[name]['filesystem_store_datadir'].startswith('/private/runtime/'))
        self.assertIn('/original/store',original)
        with self.assertRaises(ValueError):
            p.private_configuration(original.replace('fast:file','fast:rbd'),Path('/private/runtime'),'fixture')

    def test_control_export_excludes_secrets_and_distinguishes_absent_defaults(self):
        import json
        text = '[database]\nconnection=mysql://fixture-secret\nmax_pool_size=5\n[keystone_authtoken]\npassword=fixture-secret\ntoken_cache_time=600\nmemcache_pool_maxsize=fixture-secret\n'
        result = p.configured_controls(text)
        self.assertNotIn('fixture-secret',json.dumps(result))
        self.assertEqual(result['database']['max_pool_size'],5)
        self.assertIsNone(result['database']['max_overflow'])
        self.assertEqual(result['keystone_authtoken']['token_cache_time'],600)
