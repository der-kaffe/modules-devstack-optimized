import copy
import importlib.util
from pathlib import Path
import sys
import unittest

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/'scripts/benchmarks'))
S=importlib.util.spec_from_file_location('validate_glance_campaign',ROOT/'scripts/benchmarks/validate_glance_campaign.py')
v=importlib.util.module_from_spec(S)
S.loader.exec_module(v)


class CampaignWindowTests(unittest.TestCase):
    def test_truncated_wrong_stage_receipt_and_case_rejected(self):
        protocol={'measurement_s':90,'warmup_s':30,'request_timeout_s':30}
        m={'stage':'official','state':'COMPLETE_WINDOW','cleanup_verified':True,'errors':[],
           'entry_evidence_sha256':'test-only','protocol':{'status':'frozen'},
           'duration_s':90,'warmup_s':30,'timeout_s':30,'scenario':'show','concurrency':1,'payload':None,
           'warmup_start_utc_ns':0,'warmup_end_utc_ns':30_000_000_000,
           'measurement_start_utc_ns':31_000_000_000,'measurement_end_utc_ns':121_000_000_000}
        v.validate_window(m,protocol,'test-only',['show',1,None])
        for key,value in [('stage','pilot'),('cleanup_verified',False),('duration_s',89),
                          ('entry_evidence_sha256','changed'),('measurement_end_utc_ns',100_000_000_000)]:
            changed=copy.deepcopy(m)
            changed[key]=value
            with self.subTest(key=key),self.assertRaises(ValueError):
                v.validate_window(changed,protocol,'test-only',['show',1,None])
        with self.assertRaises(ValueError):
            v.validate_window(m,protocol,'test-only',['download',1,16777216])
