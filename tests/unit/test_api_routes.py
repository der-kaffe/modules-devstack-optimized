import importlib.util
from pathlib import Path
import sys
import unittest

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts/integration'))
S = importlib.util.spec_from_file_location('api_routes', ROOT / 'scripts/integration/api_routes.py')
a = importlib.util.module_from_spec(S)
S.loader.exec_module(a)


class RouteTests(unittest.TestCase):
    def test_glance_changes_only_backend_and_preserves_common_controls(self):
        original = b'KeepAlive Off\nSetEnv proxy-sendchunked 1\nProxyPass "/image" "http://127.0.0.1:60999" retry=0 acquire=1 \n'
        changed = a.go_route('glance', original)
        self.assertEqual(changed.replace(b':9293', b':60999'), original)
        self.assertEqual(set(a.CONFIGS['D']), {'glance', 'placement'})
        with self.assertRaises(ValueError):
            a.go_route('glance', b'unknown site')

    def test_private_python_a_is_distinct_from_final_original_restore(self):
        import hashlib
        import json
        import os
        import tempfile
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as name:
            root=Path(name)
            original={'glance':b'ProxyPass "/image" "http://127.0.0.1:60999"\n',
                      'placement':b'ProxyPass "/placement" "unix:/tmp/uwsgi-uds-placement-api|uwsgi://localhost"\n'}
            sites={}
            state={'sites':{}}
            for service,data in original.items():
                target=root/(service+'.conf');target.write_bytes(data)
                backup=service+'.backup';(root/backup).write_bytes(data)
                sites[service]=(target,'original-'+service,9293 if service=='glance' else 18779)
                state['sites'][service]={'backup':backup,'original_sha256':a.digest(data),'go_sha256':a.digest(a.go_route(service,data)),
                                         'mode':0o600,'uid':os.getuid(),'gid':os.getgid()}
            (root/'combined.json').write_text(json.dumps({'python_sandbox':{'unit':'own-python'}}))
            def replace(path,data,expected,*_):
                self.assertEqual(a.digest(path.read_bytes()),expected)
                path.write_bytes(data)
            with patch.object(a,'SITES',sites),patch.object(a,'command'),patch.object(a,'replace_site',side_effect=replace):
                a.transition(root,state,'A')
                self.assertFalse(state['restored'])
                self.assertIn(b':60998',sites['glance'][0].read_bytes())
                a.transition(root,state,'B')
                self.assertIn(b':9293',sites['glance'][0].read_bytes())
                a.transition(root,state,'A',original_only=True)
                self.assertTrue(state['restored'])
                self.assertEqual(sites['glance'][0].read_bytes(),original['glance'])
                sites['glance'][0].write_bytes(b'foreign edit')
                with self.assertRaises(ValueError):
                    a.transition(root,state,'A',original_only=True)
                self.assertEqual(sites['glance'][0].read_bytes(),b'foreign edit')
