// The node with HTTP/3 (NET-1): cmd/node of the core module, plus
// meshquic. Build it from core/quic: go build -o node ./cmd/node.
package main

import (
	indexus "github.com/indexus/go-indexus-core/indexus"
	nodeapp "github.com/indexus/go-indexus-core/internal/app/node"
	"github.com/indexus/go-indexus-core/quic/meshquic"
)

var version = "dev"
var commit = "none"

// diagnostic stays nil in the shipped binary, which links no profiling server.
// Building with -tags diagnostic sets it (see diagnostic.go), as in cmd/node.
var diagnostic func(*indexus.Node) (func(), error)

func main() {
	meshquic.Install()
	nodeapp.Main(version, commit, diagnostic)
}
