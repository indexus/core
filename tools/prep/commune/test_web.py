"""web.py (urllib) à la place de requests, sans réseau : un serveur HTTP local, avec des pannes.
python3 -m unittest discover -s tools/prep/commune -p 'test_web.py'

Les comparaisons à requests (l'ancienne implémentation : requests.Session + urllib3.Retry, réglée
comme l'ancien fetch.session) sont sautées si requests n'est pas installé."""

import hashlib
import io
import json
import os
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import fetch  # noqa: E402
import web  # noqa: E402

try:
    import requests
    from requests.adapters import HTTPAdapter
    from urllib3.util.retry import Retry
except ImportError:  # la cible : plus de requests
    requests = None

BLOB = hashlib.sha256(b"0").digest() * 40000  # 1,28 Mo, déterministe


class Server:
    """GET/HEAD avec Range. `plan` : pour chaque chemin, la liste des pannes des requêtes suivantes
    ('503' : 503 avec Retry-After: 0, '500', 'drop' : fermé avant les en-têtes, 'cut' : corps coupé
    à moitié, 'slow' : en-têtes après 1,5 s) ; ensuite tout va bien."""

    def __init__(self):
        self.plan, self.log, self.lock = {}, [], threading.Lock()
        server = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_HEAD(self):
                self.answer(head=True)

            def do_GET(self):
                self.answer(head=False)

            def answer(self, head):
                path = self.path.split("?")[0]
                with server.lock:
                    server.log.append((self.command, self.path, self.headers.get("Range")))
                    faults = server.plan.get(path, [])
                    fault = faults.pop(0) if faults else None
                if fault == "drop":
                    self.close_connection = True
                    self.connection.shutdown(2)
                    return
                if fault == "slow":
                    time.sleep(1.5)
                if fault in ("503", "500"):
                    self.send_response(int(fault))
                    if fault == "503":
                        self.send_header("Retry-After", "0")
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                if path == "/redirect":
                    self.send_response(302)
                    self.send_header("Location", "/blob")
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                if path == "/missing":
                    self.send_response(404)
                    self.send_header("Content-Length", "2")
                    self.end_headers()
                    if not head:
                        self.wfile.write(b"no")
                    return
                if path == "/json":
                    body = json.dumps({"path": self.path, "ok": True}).encode()
                    status = 200
                else:
                    body, status = BLOB, 200
                    rng = self.headers.get("Range")
                    if rng:
                        a, b = rng.split("=")[1].split("-")
                        a, b = int(a), min(int(b), len(BLOB) - 1)
                        body, status = BLOB[a:b + 1], 206
                self.send_response(status)
                self.send_header("Content-Length", str(len(body)))
                if status == 206:
                    self.send_header("Content-Range", f"bytes {a}-{b}/{len(BLOB)}")
                self.end_headers()
                if head:
                    return
                if fault == "cut":
                    self.wfile.write(body[: len(body) // 2])
                    self.wfile.flush()
                    self.close_connection = True
                    self.connection.shutdown(2)
                    return
                self.wfile.write(body)

        class Quiet(ThreadingHTTPServer):
            def handle_error(self, request, client_address):
                pass  # les clients qui partent avant la fin (délai dépassé) : voulu

        self.httpd = Quiet(("127.0.0.1", 0), Handler)
        self.httpd.daemon_threads = True
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


def old_session(retries=8):
    """L'ancien fetch.session, pauses à 0 pour les tests (montée aussi sur http:// pour le serveur local)."""
    s = requests.Session()
    a = HTTPAdapter(max_retries=Retry(total=retries, backoff_factor=0, backoff_max=120,
                                      status_forcelist=[429, 500, 502, 503, 504], respect_retry_after_header=True))
    s.mount("https://", a)
    s.mount("http://", a)
    return s


class Base(unittest.TestCase):
    def setUp(self):
        self.server = Server()
        self.sleeps = []
        self.saved = web.sleep
        web.sleep = self.sleeps.append

    def tearDown(self):
        web.sleep = self.saved
        self.server.close()


class Session(Base):
    def test_params_give_the_url_requests_gave(self):
        params = {"SERVICE": "WFS", "VERSION": "2.0.0", "TYPENAMES": "IGNF_LIDAR-HD_METADONNEE:metadata", "COUNT": 1000,
                  "STARTINDEX": 0, "STYLES": "", "BBOX": "-1.6,43.4,-1.5,43.5,urn:ogc:def:crs:OGC::CRS84", "SKIP": None}
        r = web.Session().get(self.server.url + "/json", params=params, timeout=10)
        self.assertEqual(r.status_code, 200)
        self.assertEqual(r.json()["ok"], True)
        if requests is not None:
            old = old_session().get(self.server.url + "/json", params=params, timeout=10)
            self.assertEqual(r.url, old.url)
            self.assertEqual(r.content, old.content)
            self.assertEqual(r.json(), old.json())
            # une URL qui a déjà une requête
            self.assertEqual(web.encode_url(self.server.url + "/json?a=1", {"b": "x y"}),
                             requests.Request("GET", self.server.url + "/json?a=1", params={"b": "x y"}).prepare().url)

    def test_ranges_and_redirects(self):
        s = web.Session()
        r = s.get(self.server.url + "/blob", headers={"Range": "bytes=100-199"}, timeout=(5, 10))
        self.assertEqual((r.status_code, r.content), (206, BLOB[100:200]))
        h = s.head(self.server.url + "/redirect", timeout=10, allow_redirects=True)
        self.assertEqual((h.status_code, int(h.headers["Content-Length"]), h.url), (200, len(BLOB), self.server.url + "/blob"))
        self.assertEqual(s.head(self.server.url + "/redirect", timeout=10).status_code, 302)  # head : sans suivre, comme requests
        r = s.get(self.server.url + "/redirect", headers={"Range": "bytes=0-9"}, timeout=10)
        self.assertEqual((r.status_code, r.content), (206, BLOB[:10]))  # Range gardé après la redirection
        if requests is not None:
            o = old_session()
            self.assertEqual(o.get(self.server.url + "/redirect", headers={"Range": "bytes=0-9"}, timeout=10).content, r.content)
            self.assertEqual(o.head(self.server.url + "/redirect", timeout=10).status_code, 302)

    def test_transient_statuses_and_drops_are_retried(self):
        self.server.plan["/blob"] = ["503", "drop", "500", "503"]
        r = web.Session().get(self.server.url + "/blob", timeout=10)
        self.assertEqual(r.content, BLOB)
        self.assertEqual(len([e for e in self.server.log if e[1] == "/blob"]), 5)
        # urllib3 2 : rien au premier nouvel essai, puis 2 x 2^(n-1) ; Retry-After: 0 ne compte pas
        self.assertEqual(self.sleeps, [0.0, 4.0, 8.0, 16.0])
        if requests is not None:
            self.server.log.clear()
            self.server.plan["/blob"] = ["503", "drop", "500", "503"]
            self.assertEqual(old_session().get(self.server.url + "/blob", timeout=10).content, BLOB)
            self.assertEqual(len([e for e in self.server.log if e[1] == "/blob"]), 5)

    def test_backoff_is_capped(self):
        s = web.Session(backoff_factor=2, backoff_max=120)
        self.assertEqual([s._backoff(n) for n in range(1, 9)], [0, 4, 8, 16, 32, 64, 120, 120])
        self.assertEqual(web._retry_after({"Retry-After": "7"}), 7.0)
        self.assertEqual(web._retry_after({"Retry-After": "Wed, 21 Oct 2015 07:28:00 GMT"}), 0.0)
        self.assertIsNone(web._retry_after({}))

    def test_exhausted_retries(self):
        self.server.plan["/blob"] = ["500"] * 3
        with self.assertRaises(web.RetryError):
            web.Session(retries=2).get(self.server.url + "/blob", timeout=10)
        self.server.plan["/blob"] = ["drop"] * 3
        with self.assertRaises(web.TransportError):
            web.Session(retries=2).get(self.server.url + "/blob", timeout=10)
        if requests is not None:
            self.server.plan["/blob"] = ["500"] * 3
            with self.assertRaises(requests.exceptions.RetryError):
                old_session(2).get(self.server.url + "/blob", timeout=10)
            self.server.plan["/blob"] = ["drop"] * 3
            with self.assertRaises(requests.exceptions.ConnectionError):
                old_session(2).get(self.server.url + "/blob", timeout=10)

    def test_a_cut_body_and_a_read_timeout_are_transport_errors(self):
        self.server.plan["/blob"] = ["cut"]
        with self.assertRaises(web.TransportError):
            web.Session().get(self.server.url + "/blob", timeout=10)
        self.assertEqual(len([e for e in self.server.log if e[1] == "/blob"]), 1)  # pas refait par la session
        self.server.plan["/blob"] = ["slow"]
        with self.assertRaises(web.TransportError):
            web.Session(retries=0).get(self.server.url + "/blob", timeout=(5, 0.5))
        if requests is not None:
            self.server.plan["/blob"] = ["cut"]
            with self.assertRaises((requests.exceptions.ChunkedEncodingError, requests.exceptions.ConnectionError)):
                old_session().get(self.server.url + "/blob", timeout=10)

    def test_raise_for_status(self):
        r = web.Session().get(self.server.url + "/missing", timeout=10)
        self.assertEqual((r.status_code, r.content), (404, b"no"))
        with self.assertRaises(web.HTTPError):
            r.raise_for_status()
        self.assertTrue(issubclass(web.HTTPError, web.RequestError) and issubclass(web.TransportError, web.RequestError))


class Fetch(Base):
    def test_local_tile_survives_cut_chunks(self):
        saved = fetch.TILE_CHUNK
        fetch.TILE_CHUNK = 100_000
        self.server.plan["/blob"] = ["cut", "drop", "503", "cut", "cut"]
        try:
            with tempfile.TemporaryDirectory() as tmp:
                d = {"name": "0334_6276", "url": self.server.url + "/blob"}
                path = fetch.local_tile(d, tmp, keep=2, streams=4, log=lambda **k: None)
                self.assertEqual(path.read_bytes(), BLOB)
                self.assertEqual(sorted(os.listdir(tmp)), ["0334_6276.copc.laz"])
                ranges = sorted(e[2] for e in self.server.log if e[0] == "GET")
                self.assertEqual(len(set(ranges)), -(-len(BLOB) // 100_000))
        finally:
            fetch.TILE_CHUNK = saved

    def test_range_cache_and_remote_file(self):
        cache = fetch.RangeCache(limit=1 << 20, pause=0)
        self.server.plan["/blob"] = [None, "cut", "drop"]  # le HEAD passe ; la première plage est coupée puis refaite
        f = fetch.RemoteFile(self.server.url + "/blob", cache)
        self.assertEqual(f.size, len(BLOB))
        f.seek(1000)
        self.assertEqual(f.read(500), BLOB[1000:1500])
        self.assertEqual(f.tell(), 1500)
        f.seek(1000)
        self.assertEqual(f.read(500), BLOB[1000:1500])  # du cache
        self.assertEqual(cache.stats["hits"], 1)
        f.seek(-10, io.SEEK_END)
        self.assertEqual(f.read(), BLOB[-10:])
        self.assertEqual(f.read(5), b"")
        buf = bytearray(8)
        f.seek(5)
        self.assertEqual((f.readinto(buf), bytes(buf)), (8, BLOB[5:13]))
        self.assertEqual(io.BufferedReader(fetch.RemoteFile(self.server.url + "/blob", cache)).read(64), BLOB[:64])


if __name__ == "__main__":
    unittest.main()
