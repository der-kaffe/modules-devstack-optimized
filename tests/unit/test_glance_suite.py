import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT/'scripts/benchmarks'))
spec = importlib.util.spec_from_file_location('glance_suite', ROOT/'scripts/benchmarks/glance_suite.py')
suite = importlib.util.module_from_spec(spec)
spec.loader.exec_module(suite)


class SuiteSafetyTests(unittest.TestCase):
    def test_failed_or_tampered_restoration_blocks_next_case(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            out = root/'session-01/restoration'
            out.mkdir(parents=True)
            evidence = root/'proof'
            evidence.write_text('fixture only')
            proof = {'restored':True, 'vm_state':'shut off', 'errors':[],
                     'evidence':[{'path':str(evidence), 'sha256':hashlib.sha256(evidence.read_bytes()).hexdigest()}]}
            path = out/'restoration-verified.json'
            path.write_text(json.dumps(proof))
            self.assertEqual(suite.restoration_proof(root), str(path))
            evidence.write_text('changed')
            with self.assertRaisesRegex(ValueError, 'evidence changed'):
                suite.restoration_proof(root)
            proof['restored'] = False
            path.write_text(json.dumps(proof))
            with self.assertRaisesRegex(ValueError, 'not verified'):
                suite.restoration_proof(root)

    def test_sequential_driver_continues_failed_case_only_after_restore(self):
        import os
        import subprocess
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            source = root/'source'
            scripts = source/'scripts/benchmarks'
            scripts.mkdir(parents=True)
            # Entirely synthetic process fixture: no HTTP, SSH or lab access.
            fake = scripts/'glance_lab_campaign.py'
            fake.write_text("""import json,sys,pathlib,hashlib
c=json.loads(pathlib.Path(sys.argv[sys.argv.index('--config')+1]).read_text());root=pathlib.Path(c['output']);p=root/'session-01/restoration';p.mkdir(parents=True)
e=root/'fixture-evidence';e.write_text('synthetic only')
(p/'restoration-verified.json').write_text(json.dumps({'restored':True,'vm_state':'shut off','errors':[],'evidence':[{'path':str(e),'sha256':hashlib.sha256(e.read_bytes()).hexdigest()}]}))
ok=c['cases'][0][0]=='list'
(root/'job-report.json').write_text(json.dumps({'vm_state':'shut off','complete':ok,'cases_completed':[{}] if ok else [],'error':None if ok else 'synthetic failure'}))
sys.exit(0 if ok else 1)
""")
            bin_dir = root/'bin'
            bin_dir.mkdir()
            virsh = bin_dir/'virsh'
            virsh.write_text('#!/bin/sh\nif [ "$LC_ALL" = C ]; then printf "shut off\\n"; else printf "apagado\\n"; fi\n')
            virsh.chmod(0o700)
            inhibitor = bin_dir/'systemd-inhibit'
            inhibitor.write_text('#!/bin/sh\nexec sleep 600\n')
            inhibitor.chmod(0o700)
            protocol = root/'protocol.json'
            cases = [['create',1,None],['list',1,None]]
            protocol.write_text(json.dumps({'repetitions':1,'warmup_s':0,'measurement_s':1,'cases':cases}))
            manifest = root/'sources.json'
            manifest.write_text(json.dumps({'scripts/benchmarks/glance_lab_campaign.py':hashlib.sha256(fake.read_bytes()).hexdigest()}))
            config = root/'config.json'
            config.write_text(json.dumps({'output':str(root/'out'),'maximum_seconds':1800,'cases':cases,
                'source_manifest':str(manifest),'base_config':{'source_root':str(source),'protocol':str(protocol),'stage':'pilot'}}))
            result = subprocess.run([sys.executable,str(ROOT/'scripts/benchmarks/glance_suite.py'),'--config',str(config),'--allow-mutations'],
                                    capture_output=True,text=True,env=dict(os.environ,PATH=str(bin_dir)+os.pathsep+os.environ['PATH']),timeout=20)
            self.assertEqual(result.returncode,1)
            state = json.loads((root/'out/state.json').read_text())
            self.assertEqual([row['accepted'] for row in state['results']],[False,True])
            self.assertEqual([row['case'] for row in state['results']],cases)
            self.assertIsNone(state['error'])
