// provisioner runs process orchestration separately from the identity issuer.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/adapter/provisioner"
	"github.com/indexus/go-indexus-core/internal/auth"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("provisioner", "err", err)
		os.Exit(1)
	}
}
func run() error {
	addr := flag.String("addr", "127.0.0.1:22010", "Control-plane listen address")
	issuer := flag.String("issuer", "http://127.0.0.1:22000", "Identity issuer origin")
	network := flag.String("network", "indexus-aws", "Authentication network")
	bootstrap := flag.String("bootstrap", "127.0.0.1", "Bootstrap IP addresses")
	template := flag.String("launchTemplate", "", "AWS launch template")
	region := flag.String("region", "eu-west-3", "AWS region")
	local := flag.String("localDir", "", "Local process scripts, instead of AWS")
	max := flag.Int("spawnMax", 1, "Maximum extra instances; 0 delegates capacity to localDir hooks")
	// The node already keeps one scale-up in flight until it lands (SCALE-1);
	// a long wait here also stops a hot vnode that needs a second split.
	requesterCooldown := flag.Duration("requesterCooldown", 3*time.Minute, "Minimum time between two scale-ups asked by the same vnode")
	readyReserve := flag.Float64("readyReservePct", 15, "Percentage of used CPU above 30% fleet utilization reserved for additional receivers (0..100)")
	flag.Parse()
	if !(*readyReserve >= 0 && *readyReserve <= 100) {
		return errors.New("readyReservePct must be between 0 and 100")
	}
	if *max < 0 || *max == 0 && *local == "" {
		return errors.New("spawnMax must be positive, or 0 with localDir capacity management")
	}
	if (*template == "") == (*local == "") {
		return errors.New("choose exactly one of launchTemplate or localDir")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(*issuer, "/")+"/v1/network/pubkey", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("issuer HTTP %d", response.StatusCode)
	}
	var identity struct {
		PublicKey string `json:"public_key"`
		NetworkID string `json:"network_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&identity); err != nil {
		return err
	}
	if identity.NetworkID != *network {
		return errors.New("issuer network mismatch")
	}
	key, err := auth.ParsePublicKeyBase64(identity.PublicKey)
	if err != nil {
		return err
	}
	verifier := auth.NewVerifier(*network, key)
	server := provisioner.NewServer(identity.PublicKey)
	server.BootstrapIPs = strings.Split(*bootstrap, ",")
	server.LaunchTemplateID = *template
	server.Region = *region
	server.LocalDir = *local
	server.SpawnMaxExtra = *max
	server.ScaleCooldown = *requesterCooldown
	server.ReadyReservePct = *readyReserve
	if *local != "" {
		server.LaunchTemplateID = "local"
	}
	return server.Serve(ctx, *addr, func(r *http.Request) bool {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			return false
		}
		token, err := auth.DecodeToken(raw)
		return err == nil && verifier.VerifyClientToken(token) == nil && token.HasScope("write")
	})
}
