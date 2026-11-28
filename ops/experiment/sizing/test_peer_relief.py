import unittest

from peer_relief import simulate


def fixture(busy):
    machines = [dict(id=id,cores=4,cpu=cpu,ram=10,disk=10,
                     receiving=receiving,reserve_cores=.5)
                for id,cpu,receiving in [('donor',94,0),('peer',5,busy)]]
    vnodes = [dict(machine='donor',applied_s=3700),dict(machine='peer',applied_s=80)]
    return dict(fleet=dict(machines=machines,vnodes=vnodes))


class PeerReliefTest(unittest.TestCase):
    def test_no_double_spend_of_traffic_or_copy_lane(self):
        for policy in ('periodic_paced','spawn','reuse','leased','share_lane'):
            with self.subTest(policy=policy):
                r = simulate(fixture(0),policy,breadth=2)
                self.assertLess(r['rate_drift'],1e-8)
                self.assertEqual(r['finished']+r['open'],r['started'])
                self.assertLessEqual(r['open'],1)

    def test_busy_receiver_only_serves_new_work_in_paid_slices(self):
        r = simulate(fixture(1),'leased',occupied_for=60,seconds=25)
        self.assertEqual(r['started'],0)
        r = simulate(fixture(1),'share_lane',occupied_for=60,seconds=25)
        self.assertGreater(r['borrowed_lane_seconds'],0)
        self.assertLessEqual(r['borrowed_lane_seconds'],5)
        self.assertLess(r['rate_drift'],1e-8)

    def test_no_peer_with_memory_headroom(self):
        f = fixture(0)
        f['fleet']['machines'][1]['ram']=80
        self.assertEqual(simulate(f,'leased')['started'],0)


if __name__=='__main__':
    unittest.main()
