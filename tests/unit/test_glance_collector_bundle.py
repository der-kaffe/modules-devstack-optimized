import ast
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class CollectorBundleTests(unittest.TestCase):
    def test_fresh_remote_bundle_includes_transitive_sampler(self):
        tree = ast.parse((ROOT/'scripts/benchmarks/glance_lab_campaign.py').read_text())
        names = next(ast.literal_eval(node.value) for node in tree.body if isinstance(node, ast.Assign) and any(isinstance(t, ast.Name) and t.id == 'REMOTE_BENCHMARK_MODULES' for t in node.targets))
        with tempfile.TemporaryDirectory() as name:
            bundle = Path(name)
            # Reproduce the actual fresh-directory import failure, without
            # accidentally finding a module in the working repository.
            shutil.copyfile(ROOT/'scripts/benchmarks/placement_resources.py', bundle/'placement_resources.py')
            command = [sys.executable, '-I', '-c', 'import sys; sys.path.insert(0,'+repr(name)+'); from placement_resources import collect']
            missing = subprocess.run(command, capture_output=True, text=True)
            self.assertNotEqual(missing.returncode, 0)
            self.assertIn('sample_processes', missing.stderr)
            for item in names:
                shutil.copyfile(ROOT/'scripts/benchmarks'/item, bundle/item)
            ready = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(ready.returncode, 0, ready.stderr)
