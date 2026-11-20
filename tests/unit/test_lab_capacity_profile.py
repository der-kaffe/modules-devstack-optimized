import importlib.util
from pathlib import Path
import tempfile
import unittest

S = importlib.util.spec_from_file_location('lab_capacity_profile', Path(__file__).resolve().parents[2] / 'scripts/integration/lab_capacity_profile.py')
p = importlib.util.module_from_spec(S)
S.loader.exec_module(p)


class CapacityProfileTests(unittest.TestCase):
    def test_worker_patch_preserves_other_bytes_and_credentials(self):
        original = b'[uwsgi]\nprocesses = 2\nthreads = 1\nsecret = unchanged\n'
        desired = b'[uwsgi]\nprocesses = 1\nthreads = 1\nsecret = unchanged\n'
        self.assertEqual(p.single_worker(original, 'processes'), desired)
        self.assertEqual(p.single_worker(desired, 'processes'), desired)
        for data in (b'processes=16\n', b'processes=2\nprocesses=2\n', b'no-worker-here'):
            with self.assertRaises(ValueError):
                p.single_worker(data, 'processes')

    def test_hash_lease_refuses_foreign_edit(self):
        with tempfile.TemporaryDirectory() as name:
            file = Path(name) / 'conf'
            file.write_bytes(b'foreign')
            with self.assertRaises(ValueError):
                p.replace(file, b'new', {p.digest(b'expected')}, {})
            self.assertEqual(file.read_bytes(), b'foreign')
