package main

import (
	indexus "github.com/indexus/go-indexus-core/indexus"
	nodeapp "github.com/indexus/go-indexus-core/internal/app/node"
)

var version = "dev"
var commit = "none"

// diagnostic stays nil in the shipped binary, which links no profiling server.
// Building with -tags diagnostic sets it (see diagnostic.go).
var diagnostic func(*indexus.Node) (func(), error)

func main() { nodeapp.Main(version, commit, diagnostic) }
