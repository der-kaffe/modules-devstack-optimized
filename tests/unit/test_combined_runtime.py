import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts/integration'))
S = importlib.util.spec_from_file_location('combined_runtime', ROOT / 'scripts/integration/combined_runtime.py')
r = importlib.util.module_from_spec(S)
S.loader.exec_module(r)


class RuntimeIsolationTests(unittest.TestCase):
    def test_path_and_private_ownership_checked_before_mutations(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name) / 'tt-placement-isolated'
            r.validate_runtime_root(root)
            for wrong in (Path('relative'), Path(name) / 'unrelated'):
                with self.assertRaises(ValueError):
                    r.validate_runtime_root(wrong)
            root.mkdir(mode=0o700)
            r.validate_runtime_root(root)
            root.chmod(0o755)
            with self.assertRaises(ValueError):
                r.validate_runtime_root(root)
            root.chmod(0o700)
            link = Path(name) / 'tt-placement-link'
            link.symlink_to(root, target_is_directory=True)
            with self.assertRaises(ValueError):
                r.validate_runtime_root(link)

    def test_open_port_without_own_main_pid_is_not_readiness(self):
        from unittest.mock import patch
        with patch.object(r.subprocess,'check_output',side_effect=['123\n','users:(("other",pid=987,fd=3))']):
            self.assertFalse(r.listener_owned_by('fixture.service',9293))
        with patch.object(r.subprocess,'check_output',side_effect=['123\n','users:(("glance",pid=123,fd=3))']):
            self.assertTrue(r.listener_owned_by('fixture.service',9293))
