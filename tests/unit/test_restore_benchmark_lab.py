import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[2]
S=importlib.util.spec_from_file_location('restore_benchmark_lab',ROOT/'scripts/integration/restore_benchmark_lab.py')
r=importlib.util.module_from_spec(S)
S.loader.exec_module(r)


class RestoreSafetyTests(unittest.TestCase):
    def test_failed_route_restore_never_powers_off(self):
        with tempfile.TemporaryDirectory() as name:
            config={'ssh':['ssh','fixture-only'],'runtime':'/home/stack/tt-placement-fixture',
                    'remote_code':'/home/stack/fixture'}
            completed=subprocess.CompletedProcess([],2,stdout='',stderr='fixture failure')
            with patch.object(r.subprocess,'check_output',return_value=r.VM_UUID+'\n'),patch.object(r.subprocess,'run',return_value=completed) as run:
                report=r.restore(config,Path(name)/'result')
            self.assertFalse(report['restored'])
            self.assertEqual(report['vm_state'],'NEEDS_ATTENTION')
            for call in run.call_args_list:
                self.assertNotIn('shutdown',call.args[0])
                self.assertNotIn('destroy',call.args[0])
