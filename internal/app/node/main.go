package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/http/monitoring"
	"github.com/indexus/go-indexus-core/internal/adapter/http/p2p"
	"github.com/indexus/go-indexus-core/internal/adapter/observability/logging"
	"github.com/indexus/go-indexus-core/internal/adapter/peer"
	"github.com/indexus/go-indexus-core/internal/auth"
	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/mesh"
)

const asciiArt = `

██╗███╗   ██╗██████╗ ███████╗██╗  ██╗██╗   ██╗███████╗
██║████╗  ██║██╔══██╗██╔════╝╚██╗██╔╝██║   ██║██╔════╝
██║██╔██╗ ██║██║  ██║█████╗   ╚███╔╝ ██║   ██║███████╗
██║██║╚██╗██║██║  ██║██╔══╝   ██╔██╗ ██║   ██║╚════██║
██║██║ ╚████║██████╔╝███████╗██╔╝ ██╗╚██████╔╝███████║
╚═╝╚═╝  ╚═══╝╚═════╝ ╚══════╝╚═╝  ╚═╝ ╚═════╝ ╚══════╝

`

const appName = "Indexus"

var (
	version = "dev"
	commit  = "none"
)

const (
	jobInterval = 10 * time.Second

	shutdownTimeout = 15 * time.Second
)

type Config struct {
	DisableCompaction  bool
	Replication        string // "", "sync" or "ack" (config.go)
	ReplicationMachine string // "", "host" or "endpoint"
	RetiredExit        time.Duration
	// Spawned: started for an autoscale target (identity.prefer_near).
	Spawned           bool
	Cold              indexus.CausalCompactionConfig
	MoveFrom          string
	CreateCluster     bool
	ClusterID         string
	TopologyEndpoints string
	ClusterCapacity   int64
	Name              string
	Bootstraps        []indexus.Contact
	MonitoringPort    int
	P2pPort           int
	Storage           string
	OmitHistory       bool
	StorageMemory     int64 // bytes; 0: the store's default
	Engine            string
	TLSDir            string
	Advertise         []string

	IssuerURL     string
	NetworkID     string
	NetworkPubKey string
	NodeKeyPath   string
	CertPath      string
	RequireAuth   bool

	Autoscale    autoscale.Config
	Listen       string
	Monitoring   string
	MemoryRefuse float64
	WriteRate    float64 // CAP-2
	// CopyBudgetDir and CopySlots: the host's copy budget (HOST-COPY-1).
	CopyBudgetDir  string
	CopySlots      int
	ShowVersion    bool
	InitializeOnly bool
	CheckOnly      bool
}

// Main is shared by the production entry point and the diagnostic tool.
func Main(buildVersion, buildCommit string, diagnostic func(*indexus.Node) (func(), error)) {
	version, commit = buildVersion, buildCommit
	logging.Setup("node")
	autoscale.CapHeapToHost()
	config, err := parseConfig(os.Args[1:])
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	if config.ShowVersion {
		fmt.Printf("%s %s\n", appName, versionString())
		return
	}
	if config.CheckOnly {
		slog.Info("configuration validated")
		return
	}
	banner(config)
	if err := run(config, diagnostic); err != nil {
		slog.Error("node stopped on error", "err", err)
		os.Exit(1)
	}
	slog.Info("node stopped")
}

func banner(config Config) {
	fmt.Print(asciiArt)
	fmt.Printf("%s %s\n\n", appName, versionString())

	slog.Info("starting node",
		"name", config.Name,
		"monitoring_port", config.MonitoringPort,
		"p2p_port", config.P2pPort,
		"bootstraps", strings.Join(hostsOf(config.Bootstraps), ","),
		"advertise", strings.Join(config.Advertise, ","),
		"storage", config.Storage,
		"issuer", config.IssuerURL,
		"require_auth", config.RequireAuth,
	)

	if config.Autoscale.Enabled {
		slog.Info("autoscale enabled",
			"role", config.Autoscale.Role,
			"pressure_hold", config.Autoscale.PressureHold,
			"cooldown", config.Autoscale.Cooldown,
			"window", config.Autoscale.Window,
		)
	}
}

// run builds the node and serves until it fails, is signalled or retires.
// Phases, in order:
//  1. build the node, attach the diagnostic tool and the authentication;
//  2. bind the monitoring and p2p listeners, then configure the cluster store;
//  3. for init only: start the cluster once and stop there;
//  4. serve p2p, open the vnode, serve monitoring, join (the safe jobs start
//     with the vnode's first right), then start the remaining jobs;
//  5. wait for a failure, a signal or the vnode's retirement;
//  6. shut down.
func run(config Config, diagnostic func(*indexus.Node) (func(), error)) error {
	node, err := newNode(config)
	if err != nil {
		return err
	}

	if diagnostic != nil {
		closeDiagnostic, err := diagnostic(node)
		if err != nil {
			return err
		}
		defer closeDiagnostic()
	}
	verifier, cert, err := setupAuth(config, node)
	if err != nil {
		return fmt.Errorf("authentication: %w", err)
	}
	if cert != nil {
		node.SetCert(cert)
	}
	if config.Autoscale.Enabled {
		node.EnableAutoscale(config.Autoscale)
	}

	servers, err := bindServers(config, node, verifier, cert)
	if err != nil {
		return err
	}

	clusterBoot, err := configureCluster(config, node, servers.p2p, verifier, cert)

	if err != nil {
		return fmt.Errorf("configure cluster: %w", err)
	}
	defer clusterBoot.Close()
	defer servers.p2pListener.Close()
	defer servers.monitoringListener.Close()
	if config.InitializeOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return clusterBoot.Start(ctx, node)
	}

	jobs := indexus.NewWorker(node)
	guard, stopGuard := context.WithCancel(context.Background())
	defer stopGuard()
	go node.GuardMemory(guard)

	if err := servers.startCluster(clusterBoot, node, jobs); err != nil {
		return err
	}
	go func() { servers.watch("jobs", jobs.Start()) }()

	runErr := servers.waitForStop(config, node)
	servers.shutdown(jobs)
	return runErr
}

// contactFactory preserves the transport of the node through discovery.
func contactFactory(config Config) func(string, map[string]any, int) indexus.Contact {
	if config.TLSDir != "" {
		return peer.NewTLSContact
	}
	return peer.NewContact
}

func newNode(config Config) (*indexus.Node, error) {
	settings, err := indexus.NewSettings(config.Name, config.P2pPort, jobInterval)
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	settings.SetAdvertise(config.Advertise...)
	settings.SetMemRefusePct(config.MemoryRefuse)

	node, err := indexus.NewNode(settings, contactFactory(config), config.Bootstraps)
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	return node, nil
}

// nodeServers holds the two HTTP servers of a node with their listeners, and
// the first failure reported by one of its background components.
type nodeServers struct {
	monitoring         *monitoring.Handler
	monitoringListener net.Listener
	p2p                *p2p.Handler
	p2pListener        net.Listener
	failed             chan error
}

// bindServers builds both servers and binds their addresses, monitoring
// first. Nothing is served yet.
func bindServers(config Config, node *indexus.Node, verifier *auth.Verifier, cert *auth.NodeCert) (*nodeServers, error) {
	s := &nodeServers{failed: make(chan error, 1)}
	var err error
	s.monitoring = monitoring.NewHttpHandler(config.TLSDir, node, versionString())
	s.monitoringListener, err = net.Listen("tcp", config.Monitoring)
	if err != nil {
		return nil, fmt.Errorf("monitoring listener: %w", err)
	}

	s.p2p = p2p.NewHttpHandler(config.TLSDir, node, contactFactory(config))
	s.p2p.Verifier = verifier
	s.p2p.RequireAuth = config.RequireAuth
	s.p2p.SelfCert = cert
	s.p2pListener, err = net.Listen("tcp", config.Listen)
	if err != nil {
		return nil, fmt.Errorf("p2p listener: %w", err)
	}
	return s, nil
}

// watch records the first real failure of a background component; a server
// closed on purpose is not one.
func (s *nodeServers) watch(component string, err error) {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return
	}
	select {
	case s.failed <- fmt.Errorf("%s: %w", component, err):
	default:
	}
}

// startCluster serves p2p, opens the vnode and joins the cluster within the
// startup time. A join that ran out of time continues in the background; any
// other failure stops the safe jobs and shuts both servers down.
func (s *nodeServers) startCluster(boot *clusterBoot, node *indexus.Node, jobs *indexus.Worker) error {
	go func() { s.watch("p2p", s.p2p.Serve(s.p2pListener)) }()
	startup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := boot.Open(startup, node)
	if err == nil {
		// The vnode is enabled and may already acknowledge writes: it is
		// monitored while it joins, so its machine is seen paying for the
		// join (/status "joining") and a spawn is acknowledged at launch.
		// /health stays 503 until the vnode is active. Monitoring must not
		// start before Open: it reads what Open attaches to the node.
		go func() { s.watch("monitoring", s.monitoring.Serve(s.monitoringListener)) }()
		err = joinWithJobs(startup, jobs, boot.hasRight, func(ctx context.Context) error { return boot.Join(ctx, node) })
	}
	cancel()
	if errors.Is(err, errJoinUnfinished) {
		slog.Warn("join continues in the background", "err", err)
		go keepJoining(node)
		err = nil
	}
	if err != nil {
		_ = s.p2p.Shutdown(context.Background())
		_ = s.monitoring.Shutdown(context.Background())
		return fmt.Errorf("start durable cluster: %w", err)
	}
	return nil
}

// firstRightPoll is how often a joining vnode is asked for its first right.
const firstRightPoll = 200 * time.Millisecond

// joinWithJobs runs join and, meanwhile, starts the safe jobs as soon as the
// vnode holds a first installed right: it then acknowledges writes, and must
// count its items and tick its autoscale without waiting for the rest of the
// join (up to the whole startup time). The jobs that change rights wait for
// Worker.Start, after the join. A join that failed stops the safe jobs before
// this returns; an unfinished one (errJoinUnfinished) leaves them running,
// like the vnode.
func joinWithJobs(ctx context.Context, jobs *indexus.Worker, hasRight func() bool, join func(context.Context) error) error {
	watch, stop := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		whenFirstRight(watch, hasRight, jobs.StartSafe)
	}()
	err := join(ctx)
	stop()
	<-watched
	if err != nil && !errors.Is(err, errJoinUnfinished) {
		jobs.Stop()
	}
	return err
}

// whenFirstRight calls start once hasRight is true; it gives up when ctx ends.
func whenFirstRight(ctx context.Context, hasRight func() bool, start func()) {
	ticker := time.NewTicker(firstRightPoll)
	defer ticker.Stop()
	for !hasRight() {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
	start()
}

// waitForStop blocks until a component fails, the process is signalled or
// the vnode's retirement has been witnessed for config.RetiredExit. Only a
// failure is returned as an error.
func (s *nodeServers) waitForStop(config Config, node *indexus.Node) error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	retired := make(chan struct{})
	if config.RetiredExit > 0 {
		go watchRetirement(node, config.RetiredExit, retired)
	}

	select {
	case err := <-s.failed:
		return err
	case sig := <-signals:
		slog.Info("shutting down; durable vnode state will resume on restart", "signal", sig.String())
	case <-retired:
		slog.Info("retired vnode leaving: its slot is free", "grace", config.RetiredExit)
	}
	return nil
}

// shutdown stops the jobs, then both servers within shutdownTimeout.
func (s *nodeServers) shutdown(jobs *indexus.Worker) {
	jobs.Close() // Cancel expensive maintenance before waiting for HTTP shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := s.p2p.Shutdown(ctx); err != nil {
		slog.Warn("p2p shutdown", "err", err)
	}
	if err := s.monitoring.Shutdown(ctx); err != nil {
		slog.Warn("monitoring shutdown", "err", err)
	}
	jobs.Close()
}

func setupAuth(config Config, node *indexus.Node) (*auth.Verifier, *auth.NodeCert, error) {
	if !config.RequireAuth && config.IssuerURL == "" {
		return nil, nil, nil
	}

	networkPub := config.NetworkPubKey
	if networkPub == "" {
		fetched, err := fetchNetworkPub(config.IssuerURL)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch network public key: %w", err)
		}
		networkPub = fetched
	}
	networkKey, err := auth.ParsePublicKeyBase64(networkPub)
	if err != nil {
		return nil, nil, fmt.Errorf("network public key: %w", err)
	}

	keyPath := config.NodeKeyPath
	if keyPath == "" {
		keyPath = fmt.Sprintf(".keys/node-%s.ed25519", config.Name)
	}
	keyPair, err := auth.LoadOrCreateKeyPair(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("node key: %w", err)
	}

	cert, err := loadOrIssueCert(config, node, auth.PublicKeyBase64(keyPair.Public))
	if err != nil {
		return nil, nil, fmt.Errorf("node certificate: %w", err)
	}

	if config.IssuerURL != "" {
		token, err := issueServiceToken(config.IssuerURL, node.Name())
		if err != nil {
			slog.Warn("no service token, peer forwarding will be rejected on an auth-gated mesh", "err", err)
		} else {
			peer.OutboundBearer = token
			os.Setenv("INDEXUS_BEARER", token)
		}
	}

	verifier := auth.NewVerifier(config.NetworkID, networkKey)
	if err := verifier.VerifyPeerCert(cert); err != nil {
		return nil, nil, fmt.Errorf("cached node certificate: %w", err)
	}
	if cert.NodeID != node.Name() || cert.PubKey != auth.PublicKeyBase64(keyPair.Public) {
		return nil, nil, errors.New("certificate does not match persistent node identity/key")
	}
	return verifier, cert, nil
}

func fetchNetworkPub(issuerURL string) (string, error) {
	resp, err := http.Get(issuerURL + "/v1/network/pubkey")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body struct {
		PublicKey string `json:"public_key"`
		NetworkID string `json:"network_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.PublicKey == "" {
		return "", errors.New("issuer returned no public key")
	}
	return body.PublicKey, nil
}

func loadOrIssueCert(config Config, node *indexus.Node, nodePub string) (*auth.NodeCert, error) {
	if cert, ok := readCert(config.CertPath); ok && cert.Signature != "" {
		return cert, nil
	}
	if config.IssuerURL == "" {
		return nil, errors.New("no certificate file and no issuer URL")
	}

	payload, err := json.Marshal(map[string]any{
		"node_id": node.Name(),
		"pub_key": nodePub,
		"ip":      node.IP(),
		"port":    node.Port(),
	})
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(config.IssuerURL+"/v1/issue/node", "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("issuer status %d: %s", resp.StatusCode, string(raw))
	}

	var body struct {
		Cert *auth.NodeCert `json:"cert"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body.Cert == nil {
		return nil, errors.New("issuer returned an empty certificate")
	}

	if config.CertPath != "" {
		if err := writeCert(config.CertPath, body.Cert); err != nil {
			slog.Warn("certificate not cached, the next restart will get a new identity", "path", config.CertPath, "err", err)
		}
	}

	return body.Cert, nil
}

func issueServiceToken(issuerURL, nodeID string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"client_id": "node-service:" + nodeID,
		"scopes":    []string{"read", "write"},
	})
	if err != nil {
		return "", err
	}

	resp, err := http.Post(issuerURL+"/v1/issue/token", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("issuer status %d: %s", resp.StatusCode, string(raw))
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", err
	}
	return body.Token, nil
}

func stickyNodeName(config Config) string {
	if cert, ok := readCert(config.CertPath); ok && cert.NodeID != "" {
		return cert.NodeID
	}
	return config.Name
}

func readCert(path string) (*auth.NodeCert, bool) {
	if path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cert auth.NodeCert
	if err := json.Unmarshal(raw, &cert); err != nil {
		return nil, false
	}
	return &cert, true
}

func writeCert(path string, cert *auth.NodeCert) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cert, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func versionString() string {
	return fmt.Sprintf("%s (%s, %s)", version, commit, runtime.Version())
}

func hostsOf(contacts []indexus.Contact) []string {
	hosts := make([]string, 0, len(contacts))
	for _, contact := range contacts {
		for ip := range contact.IPs() {
			hosts = append(hosts, fmt.Sprintf("%s:%d", ip, contact.Port()))
		}
	}
	return hosts
}

// watchRetirement closes retired once the vnode's retirement has been
// witnessed for grace: long enough for stale peers to learn the survivor.
func watchRetirement(node *indexus.Node, grace time.Duration, retired chan<- struct{}) {
	var since time.Time
	for range time.Tick(5 * time.Second) {
		if !node.RetirementWitnessed() {
			since = time.Time{}
			continue
		}
		if since.IsZero() {
			since = time.Now()
		}
		if time.Since(since) >= grace {
			close(retired)
			return
		}
	}
}

// keepJoining retries the join until it completes (JOIN-2): a saturated donor
// slows a join down, it must not make the joiner give up.
func keepJoining(node *indexus.Node) {
	for attempt := 1; ; attempt++ {
		time.Sleep(15 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := node.JoinCausalCluster(ctx)
		cancel()
		if err == nil {
			node.SetJoining(false)
			slog.Info("join completed", "attempts", attempt+1)
			return
		}
		if errors.Is(err, mesh.ErrNotNeeded) {
			// The spike that asked for this vnode is over: it holds nothing
			// and leaves, which frees its slot (and, empty, its machine).
			slog.Info("join not needed any more, leaving", "attempts", attempt+1)
			os.Exit(0)
		}
		if errors.Is(err, indexus.ErrReceiverClosed) || errors.Is(err, indexus.ErrNotOwner) {
			// The vnode is draining a cold fold or already retired: stop asking
			// donors to hand off regions to a closed receiver.
			node.SetJoining(false)
			slog.Info("join stopped: vnode draining or retired", "attempts", attempt+1, "err", err)
			return
		}
		slog.Warn("join still unfinished", "attempt", attempt+1, "err", err)
	}
}
