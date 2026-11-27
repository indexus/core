import json
from pathlib import Path
import tempfile
import unittest
import report


class ReportLatencyTests(unittest.TestCase):
    def summarize(self, records):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'load.jsonl').write_text(''.join(json.dumps(x)+'\n' for x in records))
            return report.summarize(root)

    def test_reports_all_percentiles_and_scheduled_arrival_separately(self):
        result = self.summarize([dict(offered=100, acked=100, shed=0,
            latency_us_log2=[0, 50, 45, 5], intended_latency_us_log2=[0]*10+[100],
            reads=100, totals=100, read_latency_us_log2=[0, 50, 45, 5],
            total_latency_us_log2=[0, 50, 45, 5])])
        client = result['client']
        self.assertEqual([client[k] for k in ('p50_ms', 'p95_ms', 'p99_ms')], [.002, .004, .008])
        self.assertEqual([client[k] for k in ('intended_p50_ms', 'intended_p95_ms', 'intended_p99_ms')], [1.024]*3)
        self.assertEqual(result['readers']['read_p95_ms'], .004)
        self.assertEqual(result['readers']['total_p95_ms'], .004)

    def test_pools_histograms_instead_of_averaging_percentiles(self):
        result = self.summarize([
            dict(offered=99, acked=99, shed=0, latency_us_log2=[0, 99]),
            dict(offered=1, acked=1, shed=0, latency_us_log2=[0]*20+[1])])
        self.assertEqual(result['client']['p99_ms'], .002)
        self.assertEqual(result['client']['latency_samples'], 100)
        # Old archives without intended samples must not fabricate a zero latency.
        self.assertIsNone(result['client']['intended_p99_ms'])
        self.assertEqual(result['client']['intended_samples'], 0)



class ReportControlTests(unittest.TestCase):
    def test_counts_dedicated_membership_and_labels_historical_packets(self):
        for kind, field, label in [('membership', 'membership_bytes', 'membership'),
                                  ('exchange', 'knowledge_bytes', 'historical_knowledge')]:
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                frames = []
                for at, calls in [(0, 0), (10, 5)]:
                    metrics = dict(
                        mesh_sent={kind: dict(calls=calls, request_bytes=100*calls,
                            response_bytes=20*calls, errors=0, **{field: 120*calls})},
                        gossip=dict(partners=1, version='v', known_vnodes=2, **{field: 100}),
                        writes=dict(received=10*calls, relayed=20*calls, hops=[8*calls, 2*calls]),
                        process=dict(cpu_seconds=0), storage=dict(items=0),
                        handoffs=dict(folds=0, taken=0, open=dict(count=1, oldest_seconds=3),
                            freeze_return_to_install_ack_us_log2=[0, calls]))
                    frames.append(dict(t=at, monitor='one', metrics=metrics))
                (root/'metrics.jsonl').write_text(''.join(json.dumps(x)+'\n' for x in frames))
                result = report.summarize(root)
                self.assertEqual(result['network']['gossip_calls_per_node_per_s'], .5)
                self.assertEqual(result['network']['gossip_bytes_per_node_per_s'], 60)
                self.assertEqual(result['network']['control_metadata_kind'], [label])
                self.assertEqual(result['network']['open_transfers_at_end'], 1)
                self.assertEqual(result['network']['relayed_share'], .2)
                self.assertEqual(result['network']['forward_attempts_per_success'], 2.0)
                self.assertEqual(result['handoff_intervals']['freeze_return_to_install_ack']['samples'], 5)

class ReportDrainTests(unittest.TestCase):
    def test_drain_remainder_is_included_in_full_ack_percentiles(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'load.jsonl').write_text(json.dumps(dict(offered=100, acked=90, shed=0, latency_us_log2=[0,90], intended_latency_us_log2=[0,90]))+'\n')
            (root/'load-summary.json').write_text(json.dumps(dict(offered=100, acked=100, shed=0, exhausted=0, queued_unfinished=0, drain_completed=True, latency_us_log2_remaining=[0]*20+[10], intended_latency_us_log2_remaining=[0]*20+[10])))
            result = report.summarize(root)
            self.assertEqual(result['client']['p99_ms'], 1048.576)
            self.assertEqual(result['client']['latency_samples'], 100)
            self.assertTrue(result['client']['latency_covers_final_acks'])
            self.assertEqual(result['load_final']['acked'], 100)

class FinalOnlyTests(unittest.TestCase):
    def test_short_run_with_no_periodic_rows_keeps_terminal_samples(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'load-summary.json').write_text(json.dumps(dict(offered=2, acked=2, shed=0, exhausted=0, queued_unfinished=0, drain_completed=True, latency_us_log2_remaining=[0,2], intended_latency_us_log2_remaining=[0,2], read_latency_us_log2_remaining=None)))
            result = report.summarize(root)
            self.assertEqual(result['client']['latency_samples'], 2)
            self.assertTrue(result['client']['latency_covers_final_acks'])
            self.assertEqual(result['load_final']['acked'], 2)

if __name__ == '__main__':
    unittest.main()
