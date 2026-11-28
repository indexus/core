import json
from pathlib import Path
import tempfile
import unittest
import live

class LiveTests(unittest.TestCase):
    def test_single_line_is_not_discarded_and_old_samples_expire(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d)/'metrics.jsonl'
            path.write_text(json.dumps(dict(monitor='node', t=20, metrics={'name':'node'}))+'\n')
            self.assertEqual(len(live.latest_metrics(path, elapsed=25)), 1)
            self.assertEqual(live.latest_metrics(path, elapsed=40), {})

    def test_stale_tiles_do_not_reuse_previous_success(self):
        page = live.page([dict(wall=100, phase='test', offered=2000, acked=2000, vnodes=4, open=0),
                          dict(wall=200, phase='test', metrics_stale=True, load_stale=True)])
        self.assertIn('périmé', page)
        self.assertNotIn('100.0 %', page)

    def test_queue_drops_and_server_refusals_are_distinct(self):
        row = dict(t=1, offered=100, acked=40, shed=20, in_flight=10, queued=30, workers=10,
                   failed={'503':7}, intended_latency_us_log2=[0,0,1])
        point={'wall':1}
        live.build(point, [row], {}, None)
        self.assertEqual((point['shed'],point['refused_503'],point['queued']), (20,7,30))
        self.assertIsNotNone(point['intended_p99_ms'])

    def test_relay_share_counts_successful_paths_not_failed_attempts(self):
        def frame(received, relayed, hops):
            return {'node': dict(mesh_sent={}, gossip=dict(version='v', membership_bytes=10), writes=dict(received=received, relayed=relayed, hops=hops))}
        _, previous = live.network(frame(10, 4, [8, 2]), None, 1)
        point, _ = live.network(frame(20, 24, [16, 4]), previous, 2)
        self.assertEqual(point['relayed_pct'], 20.0)
        self.assertEqual(point['forward_attempts_per_entry'], 2.0)


    def test_network_rates_follow_surviving_vnodes(self):
        def sample(name, count, t):
            return dict(name=name, sample_t=t, writes=dict(received=0, relayed=0),
                        gossip=dict(membership_bytes=10),
                        mesh_sent=dict(membership=dict(request_bytes=count, response_bytes=0)))
        initial = {"a": sample("gone", 75000, 10), "b": sample("active", 0, 10)}
        _, prev = live.network(initial, None, 10)
        next_frame = {"a": sample("replacement", 100000, 15), "b": sample("active", 5120, 15)}
        out, prev = live.network(next_frame, prev, 15)
        self.assertEqual(out["gossip_kb_s"], 1)
        repeated, _ = live.network(next_frame, prev, 20)
        self.assertNotIn("gossip_kb_s", repeated)


class LivePercentileTests(unittest.TestCase):
    def test_three_percentiles_are_kept_for_service_and_scheduled_latency(self):
        row = dict(t=1, offered=100, acked=100, shed=0,
                   latency_us_log2=[0, 50, 45, 5],
                   intended_latency_us_log2=[0]*10+[100])
        point = {'wall': 1}
        live.build(point, [row], {}, None)
        self.assertEqual([point[k] for k in ('p50_ms', 'p95_ms', 'p99_ms')], [.002, .004, .008])
        self.assertEqual([point[k] for k in ('intended_p50_ms', 'intended_p95_ms', 'intended_p99_ms')], [1.024]*3)

    def test_read_percentiles_merge_success_histograms(self):
        rows = [dict(t=i, offered=0, acked=0, shed=0, read_latency_us_log2=hist)
                for i, hist in enumerate(([0, 50], [0, 0, 40, 5, 5]))]
        point = {}
        live.build(point, rows, {}, None)
        self.assertEqual([point[f'read_p{q}_ms'] for q in live.READ_PERCENTILES],
                         [.002, .004, .008, .016])
        live.build(point, [dict(t=3, offered=0, acked=0, shed=0)], {}, None)
        self.assertTrue(all(point[f'read_p{q}_ms'] is None for q in live.READ_PERCENTILES))

if __name__ == '__main__':
    unittest.main()
