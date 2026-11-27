import unittest
from collections import defaultdict

from host_pair_admission import CPU_CEILING, REQUESTS, SCENARIOS, simulate


class HostPairAdmissionTest(unittest.TestCase):
    def test_shared_physical_identity_excludes_copies_in_either_direction(self):
        for fleet in (False, True):
            copies = simulate(0, "mixed_roles", fleet, 0.25).copies
            sources = {copy.source for copy in copies}
            targets = {copy.target for copy in copies}
            self.assertTrue(sources & targets)
            intervals = defaultdict(list)
            for copy in copies:
                self.assertNotEqual(copy.source, copy.target)
                for host in (copy.source, copy.target):
                    intervals[host].append((copy.admitted_at, copy.finished_at))
            for host, times in intervals.items():
                times.sort()
                for left, right in zip(times, times[1:]):
                    self.assertLessEqual(left[1], right[0], f"overlap on host {host}")

    def test_every_request_finishes_within_each_finite_scenario(self):
        for scenario in SCENARIOS:
            for fleet in (False, True):
                for seed in range(8):
                    with self.subTest(scenario=scenario, fleet=fleet, seed=seed):
                        run = simulate(seed, scenario, fleet, 1)
                        self.assertCountEqual([copy.request for copy in run.copies], range(REQUESTS))
                        self.assertLessEqual(run.peak_cpu, CPU_CEILING)
                        self.assertLess(run.summary()["last_copy_s"], 900)

    def test_global_pause_starts_after_the_hook_returns(self):
        copies = simulate(0, "stock", True, 1).copies
        self.assertLess(copies[1].admitted_at - copies[0].admitted_at, 1)


if __name__ == "__main__":
    unittest.main()
