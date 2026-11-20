import copy
import sys
from pathlib import Path
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2]/'scripts/benchmarks'))
from experiment_analysis import host_capacity_summary


class HostCapacityTests(unittest.TestCase):
    def test_isolated_burst_retained_but_sustained_swapping_rejected(self):
        policy={'minimum_available_bytes':768*1024**2,'maximum_full_psi_avg10_percent':1.,
                'sustained_swapout_bytes_per_s':1024**2,'sustained_swapout_seconds':9,'page_size_bytes':4096}
        rows=[{'utc_ns':i*3_000_000_000,'mem_available_bytes':1024**3,'pressure_memory':'some avg10=0.0\nfull avg10=0.0 avg60=0.0 avg300=0.0 total=0',
               'oom_kill':0,'pswpout':0} for i in range(4)]
        burst=copy.deepcopy(rows)
        for row in burst[1:]:row['pswpout']=1000
        result=host_capacity_summary(burst,0,9_000_000_000,policy)
        self.assertEqual(result['swapout_bytes'],4096000)
        self.assertEqual(result['maximum_sustained_swapout_seconds'],3)
        for i,row in enumerate(burst):row['pswpout']=i*1000
        with self.assertRaisesRegex(ValueError,'sustained host'):
            host_capacity_summary(burst,0,9_000_000_000,policy)
        for key,value in [('mem_available_bytes',100),('oom_kill',1),('pressure_memory','full avg10=1.1 avg60=0 total=0')]:
            invalid=copy.deepcopy(rows);invalid[-1][key]=value
            with self.subTest(key=key), self.assertRaises(ValueError):
                host_capacity_summary(invalid,0,9_000_000_000,policy)
