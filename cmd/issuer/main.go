package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/indexus/go-indexus-core/internal/adapter/issuer"
	"github.com/indexus/go-indexus-core/internal/adapter/observability/logging"
	"github.com/indexus/go-indexus-core/internal/auth"
)

const appName = "Indexus Issuer"

// Build metadata, set at link time; see app/node.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	logging.Setup("issuer")

	if err := run(); err != nil {
		slog.Error("issuer stopped on error", "err", err)
		os.Exit(1)
	}

	slog.Info("issuer stopped")
}

func run() error {
	addr := flag.String("addr", ":22000", "Listen address")
	networkID := flag.String("network", "indexus-aws", "Network id")
	keyPath := flag.String("key", ".keys/issuer.ed25519", "Path to the issuer ed25519 private key")
	bootstrap := flag.String("bootstrap", "", "Comma-separated bootstrap IPs handed to issued nodes")
	p2pPort := flag.Int("p2pPort", 21000, "Peer-to-peer port written into certificates")
	showVersion := flag.Bool("version", false, "Print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s (%s, %s)\n", appName, version, commit, runtime.Version())
		return nil
	}

	keys, err := auth.LoadOrCreateKeyPair(*keyPath)
	if err != nil {
		return fmt.Errorf("issuer key: %w", err)
	}

	server := issuer.NewServer(auth.NewIssuer(*networkID, keys))
	server.P2PPort = *p2pPort
	if *bootstrap != "" {
		server.BootstrapIPs = strings.Split(*bootstrap, ",")
	}
	slog.Info("starting issuer", "version", version, "network", *networkID, "public_key", auth.PublicKeyBase64(keys.Public))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return server.Serve(ctx, *addr)
}
