# HTTP/3

The node's module requires no module outside the standard library, and Go has
no HTTP/3 in it. HTTP/3 therefore lives in two optional modules of their own,
which `go test ./...` from `core/` does not build:

| Module | What it is | Requires |
| --- | --- | --- |
| `gateway/` (`github.com/indexus/go-indexus-core/gateway`) | A same-origin HTTPS + HTTP/3 entry point for the dashboard. Imports nothing from the node's module. | quic-go, qpack, x/crypto, x/net, x/text, x/sys |
| `quic/` (`github.com/indexus/go-indexus-core/quic`) | The node built with HTTP/3 (NET-1): `quic/cmd/node`. | the node's module and `gateway/` (by `replace`), quic-go |

## The default node

`cmd/node` serves plain HTTP, or HTTPS with HTTP/1.1 and HTTP/2 when
`network.tls_directory` is set. It does not advertise HTTP/3, and its mesh
client calls peers over its pooled TCP transport. Caddy handles HTTP/3 on the
public production domain. `GET /metrics` counts every mesh call under
`transport.tcp`.

## The HTTP/3 node (NET-1)

Nodes, peers and clients use the same transport (NET-1, see
[invariants](protocol/invariants.md)). Build it from the `quic/` module:

```sh
cd quic && go build -o ../bin/node-h3 ./cmd/node
```

It is `cmd/node` plus `meshquic.Install()`, which fills the node's extension
point `internal/adapter/transport` before the node starts:

- With `network.tls_directory` set, the p2p and monitoring ports serve HTTPS
  (HTTP/1.1 and HTTP/2) over TCP and HTTP/3 over UDP on the same port number,
  through the gateway's `http3server`. Responses advertise HTTP/3 with
  `Alt-Svc`. Without TLS the node serves plain HTTP and never advertises
  HTTP/3.
- The mesh client (`internal/adapter/meshhttp`) calls https peers over HTTP/3.
  If QUIC fails (UDP filtered, HTTP/3 off) it retries the same request at once
  over TCP and keeps that peer on TCP for five minutes. Mesh requests are
  idempotent by RequestID or Operation, so the retry is safe. `GET /metrics`
  counts calls per protocol under `transport` (`http3`, `tcp`, `fallbacks`).

Both builds share the handlers, the authentication, the storage and the
protocol; only the transport differs.

Neighbour contacts retain the scheme of their canonical endpoint. HTTPS is
preserved through bootstrap parsing, ping replies, discovery and read redirects;
TLS nodes create TLS contacts. These calls share the mesh connection pool and
its lazy HTTP/3 hook, rather than a separate plain-HTTP client. A failed QUIC
attempt may fall back to **TCP with TLS**; it never downgrades HTTPS to HTTP.
Probe deadlines leave room for the QUIC handshake and that TLS fallback.


Certificates: peers verify each other with the system roots, so node
certificates must be trusted by every node (a public CA, or a private CA
named by `SSL_CERT_FILE`). Experiments create a throwaway CA with
`experiment ca -dir D` and pass `-ca D` to every agent; see
`ops/experiment/README.md`.

The HTTP/3 node needs the p2p port open for both TCP and UDP. With UDP closed
the mesh still works over TCP, at the cost of one failed handshake per peer
every five minutes.

## Dashboard entry point

From the core repository:

```sh
cd gateway && go run . -listen 127.0.0.1:5443 \
  -upstream http://127.0.0.1:5173 \
  -cert /path/to/server.crt -key /path/to/server.key
```

Open `https://localhost:5443`. Supply a certificate trusted by the browser,
with a SAN matching the hostname. Local tests can use a leaf certificate from
an already trusted local development CA. The command does not install a CA.
Production requires the corresponding TCP and UDP port to be reachable.

The entry point proxies the existing dashboard, including its peer gateway.
`X-Indexus-Transport` reports the protocol of the incoming request. HTTP/3 is
negotiated by the client; a first browser visit may use HTTP/2 before Alt-Svc
has been learned. TCP fallback remains available if UDP is blocked.

Reverse-proxy request contexts propagate HTTP/3 stream cancellation to the
upstream. There is no application retry of writes or 0-RTT write replay.
Authentication, zone ownership, aggregation and P2P convergence use the same
handlers on all transports. The dashboard entry point is not a validator.

## Verification

```sh
(cd gateway && go test -race ./...)
(cd quic && go test -race ./...)
go test -race ./internal/adapter/http/p2p ./internal/adapter/meshhttp
```

Tests use real UDP/QUIC connections, validated test certificates, HTTP/2
fallback, a blocked UDP port, TCP fallback for a peer without HTTP/3, and
cancellation through a reverse proxy. They verify that canceling one HTTP/3
request releases its upstream while subsequent requests still work on the
connection. They do not establish a WAN speedup.
