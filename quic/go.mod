// The HTTP/3 build of the node (NET-1, docs/http3.md) is its own module so
// that the node's module requires nothing: Go has no HTTP/3 in the standard
// library. It links quic-go, reuses the gateway's http3server and fills the
// node's extension point internal/adapter/transport.
module github.com/indexus/go-indexus-core/quic

go 1.26.0

require (
	github.com/indexus/go-indexus-core v0.0.0
	github.com/indexus/go-indexus-core/gateway v0.0.0
	github.com/quic-go/quic-go v0.65.0
)

require (
	github.com/quic-go/qpack v0.6.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)

replace (
	github.com/indexus/go-indexus-core => ../
	github.com/indexus/go-indexus-core/gateway => ../gateway
)
