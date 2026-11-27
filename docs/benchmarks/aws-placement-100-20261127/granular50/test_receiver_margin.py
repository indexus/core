import unittest
from receiver_margin import single_arrivals, summarize


class ReceiverMarginTest(unittest.TestCase):
    def test_one_receiver_boundary_and_missing_observations(self):
        baseline = dict(cores=4, cpu_ok=True, host_cpu_ok=True, cpu_pct=16.25,
                        host_cpu_pct=12, disk_ok=True, mem_ok=True, disk_free_pct=80,
                        mem_free_pct=60, free=3, receiving=0)
        variants = ({}, {'cpu_pct': 16.26}, {'cpu_pct': 80, 'host_cpu_pct': 70},
                    {'receiving': 1}, {'disk_free_pct': 39}, {'host_cpu_ok': False})
        rows = [dict(wall=20, facts=baseline | v) for v in variants]
        rows.append(dict(wall=21, facts=baseline))
        r = summarize(rows, 0, 21, [1.5, 1.75])
        self.assertEqual(r['counts'], {'host_samples': 6, 'unknown': 1, 'room_and_no_receiver': 3})
        self.assertEqual(r['cpu'], {'host_cpu_over_60': 1, 'effective_cpu_over_60': 1})
        self.assertEqual(r['candidate_samples_by_receiver_cores'], {'1.5': 2, '1.75': 1})

    def test_isolated_arrival_within_one_plateau(self):
        rows = [dict(host='a', wall=t, running=n, receiving=0, host_cpu_ok=True,
                     host_cpu_pct=cpu, cores=4)
                for t, n, cpu in [(5, 1, 10), (20, 2, 40), (35, 2, 50), (50, 2, 20)]]
        self.assertEqual(single_arrivals(rows, [(0, 70)]),
                         dict(events=1, observed_over_60=0,
                              median_peak_increase_cores=1.6, max_peak_increase_cores=1.6))
        self.assertEqual(single_arrivals(rows, [(20, 70)])['events'], 0)
        rows[-1]['running'] = 3
        self.assertEqual(single_arrivals(rows, [(0, 70)])['events'], 0)
