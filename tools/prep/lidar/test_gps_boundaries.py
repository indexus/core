"""Boundary parity tests for gps.cell_index / gps.encode (Q11, REM-119)."""

import math
import unittest

import numpy as np

import gps


class GPSBoundaryTest(unittest.TestCase):
    def test_q11_known_sdk_boundary_vectors(self):
        col = "TownsAROME0128v2"
        # Nevers: old ceil((lat+90)/180*2^48)-1 diverged at depth 16 (...4mn vs ...4my).
        self.assertEqual(gps.encode(46.9896, 3.159, 16, col), "LoiOJS_8OcO9k4my")
        # Dyadic boundary + 1 ULP: old formula diverged even at depth 1 (L vs N).
        self.assertEqual(gps.encode(67.50000000000001, 3.159, 1, col), "N")
        # Exact dyadic boundary keeps the lower cell (strict '>' goes upper).
        r_tie, c_tie = gps.cell_index(67.5, 0.0, 3)
        r_up, c_up = gps.cell_index(math.nextafter(67.5, math.inf), math.nextafter(0.0, math.inf), 3)
        self.assertEqual((int(r_tie), int(c_tie)), (6, 3))
        self.assertEqual((int(r_up), int(c_up)), (7, 4))

    def test_vectorized_matches_scalar(self):
        lats = np.array([-90.0, -45.0, 0.0, 46.9896, 67.5, 67.50000000000001, 90.0])
        lngs = np.array([-180.0, -90.0, 0.0, 3.159, 45.0, 90.0, 180.0])
        rows, cols = gps.cell_index(lats, lngs, 48)
        self.assertEqual(rows.shape, lats.shape)
        self.assertEqual(cols.shape, lngs.shape)
        for i, (lat, lng) in enumerate(zip(lats, lngs)):
            r, c = gps.cell_index(float(lat), float(lng), 48)
            self.assertEqual((int(rows[i]), int(cols[i])), (int(r), int(c)))


if __name__ == "__main__":
    unittest.main()
