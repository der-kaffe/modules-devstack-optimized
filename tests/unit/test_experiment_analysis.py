import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location('experiment_analysis', Path(__file__).resolve().parents[2] / 'scripts/benchmarks/experiment_analysis.py')
a = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(a)


class ExperimentAnalysisTests(unittest.TestCase):
    def blocks(self, count=5):
        return [{'protocol_sha256': 'test-only', 'stage': 'synthetic-test-only', 'repetition': i,
                 'values': {'A': 100 + i, 'B': 90 + i, 'C': 80 + i, 'D': 65 + i}} for i in range(count)]

    def test_pair_deltas_and_interaction_use_repetitions(self):
        r = a.factorial(self.blocks(), 'synthetic_ms')
        self.assertEqual(r['comparisons']['A_vs_B']['difference_second_minus_first']['median'], -10)
        self.assertEqual(r['interaction_D_minus_B_minus_C_plus_A']['median'], -5)
        self.assertEqual(r['n_complete_blocks'], 5)
        self.assertFalse(r['official_accepted'])
        self.assertEqual(r['interaction_D_minus_B_minus_C_plus_A']['ci95_median'], [-5, -5])

    def test_incomplete_duplicates_protocol_or_stage_mix_rejected(self):
        cases = []
        for key in ('stage', 'protocol_sha256'):
            rows = self.blocks()
            rows[1][key] = 'different'
            cases.append(rows)
        rows = self.blocks()
        rows[0]['values'].pop('D')
        cases.append(rows)
        cases.append(self.blocks() + self.blocks()[:1])
        rows = self.blocks()
        rows[0]['values']['D'] = float('nan')
        cases.append(rows)
        for case in cases:
            with self.subTest(case=case), self.assertRaises(ValueError):
                a.factorial(case, 'synthetic_ms')

    def test_fewer_than_five_blocks_no_confidence_interval(self):
        r = a.factorial(self.blocks(2), 'synthetic_ms')
        self.assertIsNone(r['interaction_D_minus_B_minus_C_plus_A']['ci95_median'])
        values = [1, 2, 5, 8, 20]
        self.assertEqual(a.distribution(values), a.distribution(values))
        self.assertEqual(a.distribution(values)['n'], 5)

    def test_paired_ratios_are_blockwise(self):
        rows = [{'stage': 'synthetic-test-only', 'protocol_sha256': 'test', 'repetition': i,
                 'values': {'python': 10*(i+1), 'go': 5*(i+1)}} for i in range(5)]
        r = a.paired(rows, 'synthetic_ms')
        self.assertEqual(r['ratio_go_over_python']['median'], .5)
        self.assertEqual(r['percent_change']['median'], -50)
        self.assertEqual(r['difference_go_minus_python']['n'], 5)
        self.assertFalse(r['official_accepted'])
        with self.assertRaises(ValueError):
            a.paired(rows + rows[:1], 'synthetic_ms')

    def test_negative_or_zero_baseline_no_ratio(self):
        rows = self.blocks()
        rows[0]['values']['A'] = 0
        r = a.factorial(rows, 'synthetic_ms')
        self.assertIsNone(r['comparisons']['A_vs_B']['ratio_second_over_first'])

    def test_glance_raw_counts_and_checksums(self):
        # Actual socket runs against a protocol double, kept in temp directories.
        from test_glance_benchmark import GlanceBenchmarkTests, server, g
        fixture = GlanceBenchmarkTests()
        fixture.setUp()
        try:
            with server() as (_, url):
                args = fixture.args(url, 'download')
                self.assertEqual(g.run(args), 0)
                result = a.glance_run(args.output)
                self.assertTrue(result['valid'])
                self.assertEqual(result['stage'], 'local-test')
                self.assertFalse(result['official_accepted'])
                self.assertGreater(result['operations']['download']['goodput_mib_s'], 0)
                raw = args.output / 'samples.jsonl'
                raw.write_text(raw.read_text() + '{}\n')
                with self.assertRaisesRegex(ValueError, 'checksum'):
                    a.glance_run(args.output)
        finally:
            fixture.doCleanups()

    def test_http_only_keeps_bad_clock_rejected_for_resource_analysis(self):
        from test_glance_benchmark import GlanceBenchmarkTests, server, g
        fixture = GlanceBenchmarkTests()
        fixture.setUp()
        try:
            with server() as (_, url):
                args = fixture.args(url)
                self.assertEqual(g.run(args), 0)
                (args.output / 'resources.jsonl').write_text('{}\n')
                (args.output / 'clock-alignment.json').write_text('{"verified":false}')
                g.checksums(args.output)
                with self.assertRaisesRegex(ValueError, 'clock'):
                    a.glance_run(args.output)
                result = a.glance_run(args.output, include_resources=False)
                self.assertTrue(result['valid'])
                self.assertIsNone(result['resources'])
                self.assertFalse(result['official_accepted'])
                self.assertIn('HTTP only', result['validation_scope'])
        finally:
            fixture.doCleanups()

    def test_recomputed_hash_cannot_hide_inconsistent_summary(self):
        from test_glance_benchmark import GlanceBenchmarkTests, server, g
        fixture = GlanceBenchmarkTests()
        fixture.setUp()
        try:
            with server() as (_, url):
                args = fixture.args(url)
                self.assertEqual(g.run(args), 0)
                path = args.output / 'report.json'
                report = json.loads(path.read_text())
                report['http']['correct'] += 1
                path.write_text(json.dumps(report))
                g.checksums(args.output)
                with self.assertRaisesRegex(ValueError, 'summary'):
                    a.glance_run(args.output)
        finally:
            fixture.doCleanups()


if __name__ == '__main__':
    unittest.main()
