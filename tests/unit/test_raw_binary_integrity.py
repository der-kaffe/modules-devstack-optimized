import copy
from pathlib import Path
import sys
import unittest

sys.path.insert(0,str(Path(__file__).resolve().parents[2]/'scripts/benchmarks'))
from experiment_analysis import validate_binary_samples


class RawIntegrityTests(unittest.TestCase):
    def test_valid_flag_cannot_hide_digest_or_range_corruption(self):
        m={'payload':{'bytes':1048576,'sha256':'full256','sha512':'full512','range_prefix_sha256':'range256'}}
        full={'valid':True,'operation':'download','response_bytes':1048576,'digests':{'sha256':'full256','sha512':'full512'}}
        partial={'valid':True,'operation':'range','response_bytes':65536,'content_range':'bytes 0-65535/1048576','digests':{'sha256':'range256'}}
        validate_binary_samples(m,[full,partial])
        for row in (full,partial):
            bad=copy.deepcopy(row);bad['digests']['sha256']='corrupt-but-classified-valid'
            with self.assertRaises(ValueError):validate_binary_samples(m,[bad])
        bad=copy.deepcopy(partial);bad['content_range']='bytes 1-65536/1048576'
        with self.assertRaises(ValueError):validate_binary_samples(m,[bad])
