import csv
import json
from pathlib import Path
from tempfile import TemporaryDirectory
import unittest

from cpu_cost import extract


class CounterWindows(unittest.TestCase):
    def test_restart_gaps_and_training_boundary(self):
        def sample(t, name, up, cpu):
            return {"t": t, "monitor": "http://host:1000",
                    "metrics": {"name": name, "uptime_seconds": up,
                                "process": {"cpu_seconds": cpu}, "writes": {"applied": 10 * cpu}}}

        samples = [sample(265, "a", 50, 100), sample(275, "a", 60, 104),
                   sample(272, "a", 57, 103), sample(295, "b", 2, 1),
                   sample(305, "b", 12, 11), sample(315, "b", 22, 16),
                   sample(360, "b", 67, 35), sample(370, "b", 77, 37)]
        with TemporaryDirectory() as directory:
            source, target = [Path(directory) / filename for filename in ("data", "bins")]
            source.write_text("".join(json.dumps(row) + "\n" for row in samples))
            extract(source, target)
            with target.open() as stream:
                rows = list(csv.DictReader(stream))
        self.assertEqual([int(x["start"]) for x in rows], [240, 270, 300, 360])
        self.assertEqual([float(x["cpu_seconds"]) for x in rows], [2, 2, 5, 2])
        self.assertEqual([float(x["writes"]) for x in rows], [20, 20, 50, 20])
        self.assertEqual([float(x["vnode_seconds"]) for x in rows], [5, 5, 10, 10])
