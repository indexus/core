"""Lambert-93 écrit à la main (lambert.py) : précision et code EPSG des dalles, sans pyproj.
python3 -m unittest discover -s tools/prep/commune -p 'test_lambert.py'

Références :
  - la définition de Lambert-93 (IGN, décret 2000-1276, EPSG:2154) : RGF93 / GRS80
    (a = 6 378 137 m, 1/f = 298,257222101), parallèles 44° et 49° N, origine 46,5° N 3° E
    en (700 000, 6 600 000) ;
  - les formules de Snyder pour la conique conforme sécante (USGS Professional Paper 1395,
    1987, éq. 15-1 à 15-10), d'une autre écriture que celle de l'IGN (ALG0001-ALG0004)
    et aux constantes calculées, non arrondies ; elles sont elles-mêmes vérifiées sur
    l'exemple chiffré de Snyder (p. 296 : Clarke 1866, parallèles 33° et 45° N, origine
    23° N 96° O ; 35° N 75° O -> x = 1 894 410,9 m, y = 1 564 649,5 m) ;
  - sur un parallèle standard, le facteur d'échelle vaut 1 ;
  - pyproj, s'il est installé : 10 000 points tirés sur la France.
"""

import math
import struct
import sys
import unittest
from pathlib import Path

import numpy as np

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import lambert  # noqa: E402

MM = 1e-3


class Snyder:
    """Conique conforme sécante sur ellipsoïde, Snyder (1987) p. 107-109."""

    def __init__(self, a, e, p1, p2, p0, l0, x0=0.0, y0=0.0):
        self.a, self.e, self.l0, self.x0, self.y0 = a, e, math.radians(l0), x0, y0
        p1, p2, p0 = map(math.radians, (p1, p2, p0))
        m1, m2 = self.m(p1), self.m(p2)
        t1, t2, t0 = self.t(p1), self.t(p2), self.t(p0)
        self.n = (math.log(m1) - math.log(m2)) / (math.log(t1) - math.log(t2))
        self.F = m1 / (self.n * t1 ** self.n)
        self.r0 = a * self.F * t0 ** self.n

    def m(self, p):
        return np.cos(p) / np.sqrt(1 - (self.e * np.sin(p)) ** 2)

    def t(self, p):
        s = np.sin(p)
        return np.tan(np.pi / 4 - p / 2) / ((1 - self.e * s) / (1 + self.e * s)) ** (self.e / 2)

    def forward(self, lng, lat):
        r = self.a * self.F * self.t(np.radians(lat)) ** self.n
        th = self.n * (np.radians(lng) - self.l0)
        return self.x0 + r * np.sin(th), self.y0 + self.r0 - r * np.cos(th)

    def scale(self, lat):
        p = np.radians(lat)
        return self.n * self.a * self.F * self.t(p) ** self.n / (self.a * self.m(p))


GRS80_E = math.sqrt((2 - 1 / 298.257222101) / 298.257222101)
L93 = Snyder(6378137.0, GRS80_E, 49.0, 44.0, 46.5, 3.0, 700000.0, 6600000.0)


def france(n=10000, seed=7):
    rng = np.random.default_rng(seed)
    return rng.uniform(-5.5, 10.0, n), rng.uniform(41.0, 51.5, n)


class Precision(unittest.TestCase):
    def test_snyder_reproduces_its_worked_example(self):
        clarke = Snyder(6378206.4, math.sqrt(0.00676866), 33.0, 45.0, 23.0, -96.0)
        x, y = clarke.forward(-75.0, 35.0)
        self.assertAlmostEqual(float(x), 1894410.9, delta=0.06)
        self.assertAlmostEqual(float(y), 1564649.5, delta=0.06)

    def test_the_constants_are_those_of_the_definition(self):
        self.assertAlmostEqual(lambert.N, L93.n, delta=1e-10)
        self.assertAlmostEqual(lambert.C, 6378137.0 * L93.F, delta=1e-3)
        self.assertAlmostEqual(lambert.YS, 6600000.0 + L93.r0, delta=1e-3)
        self.assertAlmostEqual(lambert.E, GRS80_E, delta=1e-15)
        # un facteur d'échelle de 1 sur les deux parallèles standard
        self.assertAlmostEqual(float(L93.scale(44.0)), 1.0, delta=1e-12)
        self.assertAlmostEqual(float(L93.scale(49.0)), 1.0, delta=1e-12)

    def test_the_origin(self):
        x, y = lambert.to_l93(3.0, 46.5)
        self.assertLess(math.hypot(float(x) - 700000.0, float(y) - 6600000.0), 0.2 * MM)
        lng, lat = lambert.from_l93(700000.0, 6600000.0)
        self.assertAlmostEqual(float(lng), 3.0, delta=1e-12)
        self.assertLess(abs(float(lat) - 46.5) * 111_200, 0.2 * MM)

    def test_forward_under_a_millimetre_on_france(self):
        lng, lat = france()
        x, y = lambert.to_l93(lng, lat)
        sx, sy = L93.forward(lng, lat)
        self.assertLess(float(np.max(np.hypot(x - sx, y - sy))), 1 * MM)  # mesuré : 0,14 mm

    def test_inverse_under_a_millimetre_on_france(self):
        lng, lat = france()
        sx, sy = L93.forward(lng, lat)  # les coordonnées exactes de ces points
        lo, la = lambert.from_l93(sx, sy)
        metres = np.hypot((lo - lng) * 111_320 * np.cos(np.radians(lat)), (la - lat) * 110_600)
        self.assertLess(float(metres.max()), 1 * MM)
        x, y = lambert.to_l93(*lambert.from_l93(sx, sy))  # aller-retour
        self.assertLess(float(np.max(np.hypot(x - sx, y - sy))), 1e-6)

    def test_against_pyproj_when_installed(self):
        try:
            from pyproj import Transformer
        except ImportError:
            self.skipTest("pyproj absent (ce n'est plus une dépendance)")
        lng, lat = france()
        t = Transformer.from_crs("EPSG:4326", "EPSG:2154", always_xy=True)
        px, py = t.transform(lng, lat)
        x, y = lambert.to_l93(lng, lat)
        self.assertLess(float(np.max(np.hypot(x - px, y - py))), 1 * MM)
        lo, la = lambert.from_l93(px, py)
        self.assertLess(float(np.max(np.hypot((lo - lng) * 111_320 * np.cos(np.radians(lat)), (la - lat) * 110_600))), 1 * MM)


# Le WKT des dalles LiDAR HD (OGC WKT 1, celui qu'écrivent PDAL/GDAL pour EPSG:2154)
WKT = ('PROJCS["RGF93 v1 / Lambert-93",GEOGCS["RGF93 v1",DATUM["Reseau_Geodesique_Francais_1993_v1",'
       'SPHEROID["GRS 1980",6378137,298.257222101,AUTHORITY["EPSG","7019"]],AUTHORITY["EPSG","6171"]],'
       'PRIMEM["Greenwich",0,AUTHORITY["EPSG","8901"]],UNIT["degree",0.0174532925199433,AUTHORITY["EPSG","9122"]],'
       'AUTHORITY["EPSG","4171"]],PROJECTION["Lambert_Conformal_Conic_2SP"],PARAMETER["latitude_of_origin",46.5],'
       'PARAMETER["central_meridian",3],PARAMETER["standard_parallel_1",49],PARAMETER["standard_parallel_2",44],'
       'PARAMETER["false_easting",700000],PARAMETER["false_northing",6600000],UNIT["metre",1,AUTHORITY["EPSG","9001"]],'
       'AXIS["Easting",EAST],AXIS["Northing",NORTH],AUTHORITY["EPSG","2154"]]')
WKT2 = ('PROJCRS["RGF93 v1 / Lambert-93",BASEGEOGCRS["RGF93 v1",DATUM["Reseau Geodesique Francais 1993 v1",'
        'ELLIPSOID["GRS 1980",6378137,298.257222101,LENGTHUNIT["metre",1]]],PRIMEM["Greenwich",0,'
        'ANGLEUNIT["degree",0.0174532925199433]],ID["EPSG",4171]],CONVERSION["Lambert-93",'
        'METHOD["Lambert Conic Conformal (2SP)",ID["EPSG",9802]],PARAMETER["Latitude of false origin",46.5],'
        'PARAMETER["Longitude of false origin",3],PARAMETER["Latitude of 1st standard parallel",49],'
        'PARAMETER["Latitude of 2nd standard parallel",44],PARAMETER["Easting at false origin",700000],'
        'PARAMETER["Northing at false origin",6600000]],CS[Cartesian,2],AXIS["easting (X)",east],'
        'AXIS["northing (Y)",north],LENGTHUNIT["metre",1],ID["EPSG",2154]]')
COMPOUND = ('COMPD_CS["RGF93 v1 / Lambert-93 + NGF-IGN69 height",' + WKT + ',VERT_CS["NGF-IGN69 height",'
            'VERT_DATUM["Nivellement General de la France - IGN69",2005,AUTHORITY["EPSG","5119"]],'
            'UNIT["metre",1,AUTHORITY["EPSG","9001"]],AXIS["Gravity-related height",UP],AUTHORITY["EPSG","5720"]],'
            'AUTHORITY["EPSG","5698"]]')


class Vlr:
    def __init__(self, record_id, user_id="LASF_Projection", string=None, data=None):
        self.record_id, self.user_id, self.string, self._data = record_id, user_id, string, data

    def record_data_bytes(self):
        return self._data


class Header:
    def __init__(self, vlrs, evlrs=None):
        self.vlrs, self.evlrs = vlrs, evlrs


def geokeys(projected):
    keys = [(1024, 0, 1, 1), (3072, 0, 1, projected)]
    return struct.pack(f"<{4 + 4 * len(keys)}H", 1, 1, 0, len(keys), *[w for k in keys for w in k])


class Epsg(unittest.TestCase):
    """header_epsg à la place de LasHeader.parse_crs().to_epsg() : ce que pyproj répondrait."""

    def test_wkt(self):
        self.assertEqual(lambert.wkt_epsg(WKT), 2154)
        self.assertEqual(lambert.wkt_epsg(WKT2), 2154)
        self.assertEqual(lambert.wkt_epsg(COMPOUND), 5698)  # pyproj : le code du composé, pas 2154
        bare = WKT.replace(',AUTHORITY["EPSG","2154"]]', "]")
        self.assertEqual(lambert.wkt_epsg(bare), 2154)  # sans code : reconnu à ses paramètres
        self.assertIsNone(lambert.wkt_epsg(bare.replace('"false_easting",700000', '"false_easting",600000')))
        self.assertEqual(lambert.wkt_epsg(WKT.replace('"2154"]]', '"3857"]]')), 3857)
        self.assertEqual(lambert.wkt_epsg(WKT + "\x00\x00"), 2154)  # VLR complétée de zéros

    def test_header(self):
        self.assertEqual(lambert.header_epsg(Header([Vlr(2112, string=WKT)])), 2154)
        self.assertEqual(lambert.header_epsg(Header([Vlr(34735, data=geokeys(2154))])), 2154)
        self.assertEqual(lambert.header_epsg(Header([Vlr(34735, data=geokeys(2154)), Vlr(2112, string=COMPOUND)])), 5698)  # WKT d'abord
        self.assertEqual(lambert.header_epsg(Header([Vlr(2112, string=WKT)], [Vlr(2112, string=WKT2)])), 2154)
        self.assertIsNone(lambert.header_epsg(Header([Vlr(1, user_id="copc")])))
        self.assertIsNone(lambert.header_epsg(Header([])))
        raw = Vlr(2112)
        raw.string, raw.record_data = None, WKT.encode() + b"\x00"
        self.assertEqual(lambert.header_epsg(Header([raw])), 2154)


if __name__ == "__main__":
    unittest.main()
