import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT/'scripts/integration'))
SPEC = importlib.util.spec_from_file_location('lab_restoration_lease', ROOT/'scripts/integration/lab_restoration_lease.py')
lease = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(lease)


class KernelLeaseTests(unittest.TestCase):
    def test_actual_flock_then_stale_marker_rejected(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            lock = root/'lock'
            (root/'state.json').write_text(json.dumps({'phase': 'HELD'}))
            code = "import fcntl,sys; f=open(sys.argv[2],'w'); fcntl.flock(f,fcntl.LOCK_EX); print('held',flush=True); input(); f.close(); print('released',flush=True); input()"
            child = subprocess.Popen([sys.executable, '-u', '-c', code, str(root), str(lock)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
            try:
                self.assertEqual(child.stdout.readline().strip(), 'held')
                with patch.object(lease, 'LOCK_PATH', lock), patch.object(lease.subprocess, 'check_output', return_value=str(child.pid)):
                    self.assertTrue(lease.verify_held(root, 'tt-restoration-lease-fixture')['verified'])
                    child.stdin.write('\n');child.stdin.flush()
                    self.assertEqual(child.stdout.readline().strip(), 'released')
                    with self.assertRaisesRegex(ValueError, 'does not hold'):
                        lease.verify_held(root, 'tt-restoration-lease-fixture')
            finally:
                child.terminate();child.wait(timeout=5)
                child.stdin.close();child.stdout.close()

    def test_absent_or_foreign_unit_rejected(self):
        with patch.object(lease.subprocess, 'check_output', return_value='0'):
            with self.assertRaisesRegex(ValueError, 'absent'):
                lease.verify_held(Path('/var/tmp/fixture'), 'tt-restoration-lease-fixture')
        with self.assertRaisesRegex(ValueError, 'own restoration unit'):
            lease.verify_held(Path('/var/tmp/fixture'), 'mysql')
