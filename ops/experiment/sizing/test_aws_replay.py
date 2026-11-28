import json
from pathlib import Path
import tempfile
import unittest

from fleet_replay import capacity
from placement_replay import Inventory, replay
from zone_service import intervals


class ReplayTests(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()
