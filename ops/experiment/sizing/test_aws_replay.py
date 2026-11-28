import json
from pathlib import Path
import tempfile
import unittest

from fleet_replay import capacity
from placement_replay import Inventory, replay
from mixed_replay import exits, host_timeline, read_rate, replay_writes
from zone_service import intervals


class ReplayTests(unittest.TestCase):
    def test_mixed_replay_conserves_write_arrivals(self):
        rows = [
            dict(offered=10, shed=2, queued=4, in_flight=2),
            dict(offered=10, shed=7, queued=4, in_flight=2),
        ]
        self.assertEqual(exits(rows, initial=1), 6)
        result = replay_writes(rows, initial=1, capacity=6, service_rate=3, ack_share=1)
        self.assertEqual(result, dict(acked_s=3, shed=9, end_backlog=6))

    def test_mixed_replay_reader_rate_drops_when_responses_exceed_the_period(self):
        one_sample = [dict(read_latency_us_log2=[0] * 19 + [1])]
        fast = read_rate(one_sample, readers=10, target=1000)
        self.assertEqual(fast["midpoint"], 25.4)
        self.assertIsNone(read_rate([], readers=10, target=1000))

    def test_mixed_replay_observed_machine_is_not_a_writer(self):
        machine = dict(id="host", ready_offer=True)
        def frame(t, state, rate):
            return dict(t=t, fleet=dict(machines=[machine],
                vnodes=[dict(machine="host", state=state, applied_s=rate)]))
        frames = [frame(2, "receiving", None), frame(5, "stale", 10), frame(8, "receiving", 5)]
        self.assertEqual(host_timeline(frames)["host"],
                         dict(first_observed_s=2, first_offer_s=2, first_write_s=8))

    def test_finishing_boot_inside_observation_interval(self):
        frames = [(0,2,1,10),(10,2,1,10)]
        m = capacity(frames, "pipeline", 1, 4, 5, .5)
        self.assertEqual(m["final_hosts"], 4)
        self.assertEqual(m["demand_above_target_host_seconds"], 7.5)
        self.assertEqual(m["billed_host_seconds"], 40)

    def test_pending_boot_not_counted_twice(self):
        frames = [(t,2,1,10) for t in (0,1,2,10)]
        self.assertEqual(capacity(frames, "pipeline",1,20,10,.5)["final_hosts"],4)
        self.assertEqual(capacity(frames, "ready_only",1,20,10,.5)["final_hosts"],10)

    def test_alternative_grant_requires_fresh_prior_compatible_inventory(self):
        with tempfile.TemporaryDirectory() as directory:
            p = Path(directory)/"facts"
            def row(host,t,names):
                return dict(agent=host,wall=t,facts=dict(running=len(names),vnodes=names,
                            items=dict.fromkeys(names,100),cores=4))
            p.write_text("\n".join(json.dumps(r) for r in
                                  (row("src",1,["hot"]),row("dst",1,[]),row("dst",9,["hot"]))))
            inv = Inventory(p)
        events = [((1,"hot","p"),dict(write_rate=500)),
                  ((5,"other","q"),dict(machine="dst",applied="existing",write_rate=70))]
        def focus(age):
            return replay(events,inv,0,10,"rate",30,age,"hot")["focus"][0][2]
        self.assertEqual(focus(5),5)
        self.assertIsNone(focus(2))
        self.assertNotIn("hot",inv.at(5,5)["dst"]["vnodes"])

    def test_installation_gaps_and_resets_do_not_create_fake_progress(self):
        with tempfile.TemporaryDirectory() as directory:
            p = Path(directory)/"live"
            frames=[]
            for t,age,count in [(10,10,5),(15,15,20),(20,20,35),(25,2,1),(30,7,8)]:
                frames.append(dict(t=t,fleet=dict(transfers=[
                    dict(id="op",age_s=age,installed_zones=count)])))
            p.write_text("\n".join(map(json.dumps,frames)))
            self.assertEqual(intervals(p)["op"],[(5,15),(5,15)])

    def test_stale_transfer_breaks_interval_even_when_fleet_is_fresh(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"live"
            frames = []
            for t in (10, 15, 20, 25, 30, 35):
                transfer = dict(id="donor:op", age_s=t, installed_zones=t*2,
                                stale=t == 20)
                frames.append(dict(t=t, fleet=dict(transfers=[transfer])))
            path.write_text("\n".join(map(json.dumps, frames)))
            self.assertEqual(intervals(path, after=15, before=35)["donor:op"], [(5, 10)])


if __name__ == "__main__":
    unittest.main()
