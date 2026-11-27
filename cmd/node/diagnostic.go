//go:build diagnostic

// The diagnostic build runs the same node with explicit loopback profiling:
// go build -tags diagnostic ./cmd/node
package main

import "github.com/indexus/go-indexus-core/internal/diagnostics"

func init() {
	diagnostic = diagnostics.Start
}
