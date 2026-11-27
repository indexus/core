// The optional HTTP/3 edge (docs/http3.md) is its own module so that the
// node's module links no QUIC stack: Go has no HTTP/3 server in the
// standard library, and production serves HTTP/3 through Caddy.
module github.com/indexus/go-indexus-core/gateway

go 1.26.0

require github.com/quic-go/quic-go v0.65.0

require (
	github.com/quic-go/qpack v0.6.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)
