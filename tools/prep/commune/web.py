"""HTTP de la préparation, sur la bibliothèque standard (urllib.request), à la place de requests.

Ce que fetch.py attendait de requests + urllib3.Retry, et rien de plus :

  Session(retries=8)  GET et HEAD ; un échec de connexion, une coupure ou un délai dépassé avant
                      les en-têtes, un statut 429/500/502/503/504 sont refaits `retries` fois.
                      Pause de urllib3 2 : rien au premier nouvel essai, puis
                      backoff_factor x 2^(n-1), au plus backoff_max ; Retry-After (secondes ou
                      date HTTP) respecté pour 413/429/503. Statuts encore mauvais à la fin :
                      RetryError ; connexion toujours impossible : TransportError.
  timeout             un nombre ou (connexion, lecture), comme requests : la lecture borne
                      chaque attente du socket, pas la durée totale.
  params              encodés comme requests (urlencode, doseq) : la même URL, la même r.url.
  Response            status_code, url (après redirections), headers, content (lu en entier,
                      comme requests sans stream), json(), raise_for_status() -> HTTPError.

Les erreurs : RequestError (requests.exceptions.RequestException) ; TransportError
(ConnectionError, Timeout et ChunkedEncodingError : connexion, délai, corps coupé) ;
HTTPError ; RetryError. Un corps coupé n'est pas refait ici (urllib3 ne le refaisait pas) :
fetch.py le refait lui-même.

Différences voulues ou sans effet sur les sorties : pas d'Accept-Encoding (le serveur envoie le
corps tel quel, requests le décompressait : même contenu) ; une connexion par requête (urllib
n'en garde pas) ; certificats : ceux du système (SSL_CERT_FILE respecté), plus certifi s'il est
installé (requests utilisait certifi seul).
"""

import email.utils
import http.client
import json as jsonlib
import socket
import ssl
import time
import urllib.error
import urllib.parse
import urllib.request

sleep = time.sleep  # remplacé dans les tests

RETRY_AFTER_STATUS = (413, 429, 503)


class RequestError(Exception):
    pass


class TransportError(RequestError):
    """Connexion impossible, délai dépassé, corps coupé (ConnectionError, Timeout, ChunkedEncodingError)."""


class HTTPError(RequestError):
    def __init__(self, message, response=None):
        super().__init__(message)
        self.response = response


class RetryError(RequestError):
    def __init__(self, message, response=None):
        super().__init__(message)
        self.response = response


class Response:
    def __init__(self, status_code, url, headers, content):
        self.status_code, self.url, self.headers, self.content = status_code, url, headers, content

    @property
    def ok(self):
        return self.status_code < 400

    def json(self):
        return jsonlib.loads(self.content)

    def raise_for_status(self):
        if 400 <= self.status_code < 600:
            kind = "Client" if self.status_code < 500 else "Server"
            raise HTTPError(f"{self.status_code} {kind} Error for url: {self.url}", self)


def encode_url(url, params=None):
    """L'URL avec ses paramètres, comme requests les ajoute (urlencode doseq, `&` s'il y a déjà une requête)."""
    if not params:
        return url
    items = params.items() if hasattr(params, "items") else params
    query = urllib.parse.urlencode([(k, v) for k, v in items if v is not None], doseq=True)
    if not query:
        return url
    scheme, netloc, path, old, fragment = urllib.parse.urlsplit(url)
    return urllib.parse.urlunsplit((scheme, netloc, path, f"{old}&{query}" if old else query, fragment))


def _timeouts(timeout):
    if isinstance(timeout, tuple):
        return timeout[0], timeout[1]
    return timeout, timeout


def _connection(base, read):
    class Connection(base):
        def connect(self):
            super().connect()  # self.timeout : celui de la connexion
            self.sock.settimeout(read)

    return Connection


class _HTTPHandler(urllib.request.HTTPHandler):
    def __init__(self, read):
        super().__init__()
        self.read = read

    def http_open(self, req):
        return self.do_open(_connection(http.client.HTTPConnection, self.read), req)


class _HTTPSHandler(urllib.request.HTTPSHandler):
    def __init__(self, read, context):
        super().__init__(context=context)
        self.read = read

    def https_open(self, req):
        return self.do_open(_connection(http.client.HTTPSConnection, self.read), req, context=self._context)


def _context():
    context = ssl.create_default_context()
    try:
        import certifi
    except ImportError:
        return context
    context.load_verify_locations(certifi.where())
    return context


def _retry_after(headers):
    value = headers.get("Retry-After") if headers is not None else None
    if not value:
        return None
    value = value.strip()
    if value.isdigit():
        return float(value)
    try:
        when = email.utils.parsedate_to_datetime(value)
    except (TypeError, ValueError):
        return None
    return max(0.0, when.timestamp() - time.time())


class Session:
    def __init__(self, retries=8, backoff_factor=2.0, backoff_max=120.0, status_forcelist=(429, 500, 502, 503, 504)):
        self.retries, self.backoff_factor, self.backoff_max = retries, backoff_factor, backoff_max
        self.status_forcelist = frozenset(status_forcelist)
        self._ssl = None

    def get(self, url, **kwargs):
        return self.request("GET", url, **kwargs)

    def head(self, url, allow_redirects=False, **kwargs):
        return self.request("HEAD", url, allow_redirects=allow_redirects, **kwargs)

    def _backoff(self, consecutive):
        if consecutive <= 1:
            return 0.0
        return min(self.backoff_max, self.backoff_factor * 2 ** (consecutive - 1))

    def _opener(self, read, allow_redirects):
        if self._ssl is None:
            self._ssl = _context()
        handlers = [_HTTPHandler(read), _HTTPSHandler(read, self._ssl)]
        if not allow_redirects:
            handlers.append(_NoRedirect())
        return urllib.request.build_opener(*handlers)

    def request(self, method, url, params=None, headers=None, timeout=None, allow_redirects=True):
        full = encode_url(url, params)
        connect, read = _timeouts(timeout)
        opener = self._opener(read, allow_redirects)
        errors = 0
        while True:
            req = urllib.request.Request(full, method=method, headers=dict(headers or {}))
            req.add_header("User-Agent", "indexus-prep")
            try:
                raw = opener.open(req, timeout=connect if connect is not None else socket._GLOBAL_DEFAULT_TIMEOUT)
            except urllib.error.HTTPError as error:
                raw = error  # un statut d'erreur : une réponse comme une autre
            except (urllib.error.URLError, OSError, http.client.HTTPException) as error:
                errors += 1
                if errors > self.retries:
                    raise TransportError(f"{method} {full}: {getattr(error, 'reason', error)}") from error
                sleep(self._backoff(errors))
                continue
            status = raw.status if raw.status is not None else raw.code
            if status in self.status_forcelist and method in ("GET", "HEAD"):
                raw.close()
                errors += 1
                if errors > self.retries:
                    raise RetryError(f"{method} {full}: trop de réponses {status}", Response(status, raw.geturl(), raw.headers, b""))
                wait = _retry_after(raw.headers) if status in RETRY_AFTER_STATUS else None
                sleep(wait if wait else self._backoff(errors))  # Retry-After: 0 -> la pause ordinaire, comme urllib3
                continue
            try:
                body = b"" if method == "HEAD" else raw.read()
            except (OSError, http.client.HTTPException) as error:  # IncompleteRead, reset, délai
                raise TransportError(f"{method} {full}: corps coupé ({error!r})") from error
            finally:
                raw.close()
            return Response(status, raw.geturl(), raw.headers, body)


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None
