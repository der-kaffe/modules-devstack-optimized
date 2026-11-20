import hashlib
import json
from pathlib import Path
import tempfile
import time
import unittest
from types import SimpleNamespace

from test_glance_benchmark import g


class OfficialContractTests(unittest.TestCase):
    def test_freeze_identity_parameters_and_artifacts_are_required(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            proof = root / 'proof.json'
            proof.write_text('{"test_fixture_only":true}')
            protocol = {'status': 'frozen', 'official_execution_enabled': True, 'repetitions': 5,
                        'measurement_s': 120, 'warmup_s': 30, 'cases': [['show', 1, None]],
                        'entry_gates': ['test-only']}
            raw = json.dumps(protocol).encode()
            receipt = {'protocol_sha256': hashlib.sha256(raw).hexdigest(),
                       'instrument_sha256': hashlib.sha256(Path(g.__file__).read_bytes()).hexdigest(),
                       'code_sha': 'a' * 40, 'valid_until_utc_s': time.time() + 60,
                       'gates': {'test-only': {'passed': True, 'path': str(proof),
                                               'sha256': hashlib.sha256(proof.read_bytes()).hexdigest()}}}
            path = root / 'receipt.json'
            path.write_text(json.dumps(receipt))
            args = SimpleNamespace(duration=120, warmup=30, scenario='show', concurrency=1,
                                   payload_info=None, entry_evidence=path, code_sha='a' * 40)
            g.official_contract(args, protocol, raw)
            self.assertTrue(args.entry_evidence_sha256)
            args.duration = 119
            with self.assertRaises(ValueError):
                g.official_contract(args, protocol, raw)
            args.duration = 120
            proof.write_text('modified')
            with self.assertRaises(ValueError):
                g.official_contract(args, protocol, raw)
