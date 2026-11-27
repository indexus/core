"""python3 -m unittest discover -s tools/prep/scenes"""

import json
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import scenes  # noqa: E402

VECTORS = HERE.parent / "lidar" / "testdata" / "space-vectors.json"


class Scenes(unittest.TestCase):
    def test_a_scene_is_a_record_at_its_origin(self):
        entry = {"id": "anglet", "origin": [-1.520707, 43.484986], "collections": {"terrain": "TerraLidHD2025v1"}}
        [record] = scenes.records([entry])
        self.assertEqual(len(scenes.COLLECTION), 16)
        self.assertEqual(record["Collection"], scenes.COLLECTION)
        self.assertEqual(len(record["Location"]), 16)
        self.assertEqual(record["Payload"], entry)
        # the SDK's encoding (shared vectors): the same point in DVF's collection
        vector = next(v for v in json.loads(VECTORS.read_text())["points"]
                      if v["collection"] == "DvFMV2020idx0001" and v["depth"] == 5)
        self.assertEqual(scenes.gps.encode(*vector["point"], 5, "DvFMV2020idx0001"), vector["location"])

    def test_ids_are_unique(self):
        with self.assertRaises(ValueError):
            scenes.records([{"id": "a", "origin": [0, 0]}, {"id": "a", "origin": [1, 1]}])


if __name__ == "__main__":
    unittest.main()
