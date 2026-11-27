package node

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/peer"
	"github.com/indexus/go-indexus-core/internal/autoscale"
)

// FileConfig is the only production configuration format. Relative paths are
// resolved against the file, never the process working directory. Secrets stay
// in separate persistent key files. Unknown fields fail closed.
type FileConfig struct {
	Storage struct {
		Path              string `json:"path"`
		IdentityDirectory string `json:"identity_directory,omitempty"`
		OmitHistory       bool   `json:"omit_history,omitempty"`
		// MemoryBytes is the store's memory budget (zonelog.Options), 256 MiB
		// when absent.
		MemoryBytes int64 `json:"memory_bytes,omitempty"`
		// Engine is where record rows live: "zonelog", the only engine.
		Engine string `json:"engine,omitempty"`
	} `json:"storage"`
	Identity struct {
		Name string `json:"name,omitempty"`
		// PreferNear is a target, not an identity (SPAWN-1): the node takes a
		// fresh name sharing the target's first 64 bits, so a retried spawn
		// never reuses the identity of an attempt that failed.
		PreferNear string `json:"prefer_near,omitempty"`
		Network    string `json:"network"`
		Cluster    string `json:"cluster"`
		Issuer     string `json:"issuer,omitempty"`
		PublicKey  string `json:"public_key,omitempty"`
	} `json:"identity"`
	Network struct {
		Listen       string   `json:"listen"`
		Advertise    []string `json:"advertise"`
		Bootstrap    []string `json:"bootstrap,omitempty"`
		Monitoring   string   `json:"monitoring,omitempty"`
		TLSDirectory string   `json:"tls_directory,omitempty"`
	} `json:"network"`
	Limits struct {
		DataBytes       int64   `json:"data_bytes,omitempty"`
		MemoryRefusePct float64 `json:"memory_refuse_pct,omitempty"`
		// WritesPerSecond caps the writes this vnode applies (CAP-2), to
		// simulate a smaller machine in scale experiments. Zero: no cap.
		WritesPerSecond float64 `json:"writes_per_second,omitempty"`
		// CopyBudgetDirectory is shared by the vnodes of one host: together
		// they run at most CopySlots transfer copies at once (HOST-COPY-1).
		// Absent: no budget. CopySlots zero: a quarter of cores, at least one.
		CopyBudgetDirectory string `json:"copy_budget_directory,omitempty"`
		CopySlots           int    `json:"copy_slots,omitempty"`
	} `json:"limits"`
	// Compaction folds cool vnode pairs (COLD-2). On unless set to false.
	Compaction *bool `json:"compaction,omitempty"`
	// Replication copies each zone to the other members of its R(z): "" is
	// off; "sync" copies in the background and acknowledges a write once it
	// is durable here (enough on a redundant disk such as EBS); "ack" also
	// waits, at most a second, for one other member to hold the write (for
	// local NVMe, or to survive the loss of a zone).
	Replication string `json:"replication,omitempty"`
	// ReplicationMachine groups vnodes into machines for R(z): "" or "host"
	// by host address, "endpoint" makes each process a machine (several
	// processes on one host).
	ReplicationMachine string `json:"replication_machine,omitempty"`
	// RetiredExitSeconds, when positive, stops the process that long after its
	// vnode's retirement is witnessed, freeing the machine slot. Zero keeps the
	// retired endpoint forwarding forever.
	RetiredExitSeconds int `json:"retired_exit_seconds,omitempty"`
	// Scaling tunes the autoscaler when a provisioner is configured.
	Scaling struct {
		ItemsPerVNode int     `json:"items_per_vnode,omitempty"`
		CPUTargetPct  float64 `json:"cpu_target_pct,omitempty"`
		// WritesPerVNode: write demand per second that forces scale-up. Unset
		// with limits.writes_per_second, 70% of that cap.
		WritesPerVNode  float64 `json:"writes_per_vnode,omitempty"`
		ResourceSignals *bool   `json:"resource_signals,omitempty"`
	} `json:"scaling"`
	Provisioner string `json:"provisioner,omitempty"`
}

func parseConfig(args []string) (Config, error) {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	if command != "serve" && command != "init" && command != "move" && command != "check" {
		return Config{}, fmt.Errorf("expected serve, init, move or check, got %q", command)
	}
	flags := flag.NewFlagSet("node "+command, flag.ContinueOnError)
	path := flags.String("config", "", "Path to the node JSON configuration")
	version := flags.Bool("version", false, "Print the version and exit")
	var source string
	if command == "move" {
		flags.StringVar(&source, "from", "", "HTTP(S) endpoint of the current physical host")
	}
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, errors.New("unexpected positional arguments")
	}
	if *version {
		return Config{ShowVersion: true}, nil
	}
	if *path == "" {
		return Config{}, errors.New("-config is required")
	}
	config, err := loadConfig(*path)
	if err != nil {
		return Config{}, err
	}
	config.CheckOnly = command == "check"
	config.CreateCluster = command == "init"
	config.InitializeOnly = command == "init"
	if command == "init" && len(config.Bootstraps) > 0 {
		return Config{}, errors.New("init creates a genesis; use serve to join a bootstrap")
	}
	if command == "move" {
		if _, err := endpoint(source); err != nil {
			return Config{}, fmt.Errorf("move source: %w", err)
		}
		config.MoveFrom = source
	}
	return config, nil
}

func endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("expected an HTTP(S) origin: %q", raw)
	}
	if u.Port() != "" {
		p, e := strconv.Atoi(u.Port())
		if e != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid endpoint port: %q", raw)
		}
	}
	return u, nil
}

// loadConfig reads the configuration file into a Config. Steps, in order
// (the first failing check is the one reported):
//  1. decode the single JSON object and check the required fields;
//  2. map the file to a Config, relative paths resolved against the file;
//  3. default and check the addresses, the limits and the issuer;
//  4. choose the node name;
//  5. turn the bootstrap origins into contacts;
//  6. configure the autoscaler when a provisioner is set.
func loadConfig(path string) (Config, error) {
	file, err := readFileConfig(path)
	if err != nil {
		return Config{}, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	config := fileToConfig(file, filepath.Dir(absolute))
	if err := checkAddresses(&config); err != nil {
		return Config{}, err
	}
	if err := checkLimits(&config); err != nil {
		return Config{}, err
	}
	if err := checkIssuer(config); err != nil {
		return Config{}, err
	}
	config.Spawned = file.Identity.PreferNear != ""
	if config.Name, err = chooseNodeName(config, file); err != nil {
		return Config{}, err
	}
	if err := addBootstraps(&config, file.Network.Bootstrap); err != nil {
		return Config{}, err
	}
	if err := configureAutoscale(&config, file); err != nil {
		return Config{}, err
	}
	return config, nil
}

// readFileConfig decodes the file: one JSON object of at most 1 MiB, without
// unknown fields, holding the required fields.
func readFileConfig(path string) (FileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileConfig{}, err
	}
	defer f.Close()
	var file FileConfig
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return FileConfig{}, fmt.Errorf("configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return FileConfig{}, errors.New("configuration must contain one JSON object")
	}
	if file.Storage.Path == "" || file.Identity.Network == "" || file.Identity.Cluster == "" {
		return FileConfig{}, errors.New("storage.path, identity.network and identity.cluster are required")
	}
	return file, nil
}

// fileToConfig copies the file's fields into a Config, unchecked. Relative
// paths are resolved against dir, the directory of the configuration file;
// the node key and certificate live in the identity directory, by default
// next to the storage.
func fileToConfig(file FileConfig, dir string) Config {
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	config := Config{Storage: resolve(file.Storage.Path), OmitHistory: file.Storage.OmitHistory, StorageMemory: file.Storage.MemoryBytes, Engine: file.Storage.Engine, ClusterID: file.Identity.Cluster, NetworkID: file.Identity.Network, Name: file.Identity.Name, RequireAuth: true, Listen: file.Network.Listen, Monitoring: file.Network.Monitoring, Advertise: file.Network.Advertise, TLSDir: resolve(file.Network.TLSDirectory), ClusterCapacity: file.Limits.DataBytes, MemoryRefuse: file.Limits.MemoryRefusePct, WriteRate: file.Limits.WritesPerSecond, DisableCompaction: file.Compaction != nil && !*file.Compaction, Replication: file.Replication, ReplicationMachine: file.ReplicationMachine, RetiredExit: time.Duration(file.RetiredExitSeconds) * time.Second, NetworkPubKey: file.Identity.PublicKey, IssuerURL: strings.TrimRight(file.Identity.Issuer, "/")}
	config.CopyBudgetDir, config.CopySlots = resolve(file.Limits.CopyBudgetDirectory), file.Limits.CopySlots
	identityDir := resolve(file.Storage.IdentityDirectory)
	if identityDir == "" {
		identityDir = filepath.Dir(config.Storage)
	}
	config.NodeKeyPath = filepath.Join(identityDir, "node.ed25519")
	config.CertPath = filepath.Join(identityDir, "node.cert.json")
	return config
}

// checkAddresses defaults the P2P, monitoring and advertised addresses to
// the loopback, derives both ports and checks them.
func checkAddresses(config *Config) error {
	if config.Listen == "" {
		config.Listen = "127.0.0.1:21000"
	}
	if config.Monitoring == "" {
		config.Monitoring = "127.0.0.1:19000"
	}
	var err error
	if config.P2pPort, err = listenPort(config.Listen); err != nil {
		return err
	}
	if config.MonitoringPort, err = listenPort(config.Monitoring); err != nil {
		return err
	}
	if config.Listen == config.Monitoring {
		return errors.New("P2P and monitoring must have distinct addresses")
	}
	if len(config.Advertise) == 0 {
		config.Advertise = []string{"127.0.0.1"}
	}
	for _, host := range config.Advertise {
		if net.ParseIP(host) == nil {
			return fmt.Errorf("advertise must contain IP addresses: %q", host)
		}
	}
	return nil
}

func listenPort(address string) (int, error) {
	_, raw, err := net.SplitHostPort(address)
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 || v > 65535 {
		return 0, fmt.Errorf("invalid listen port: %q", address)
	}
	return v, nil
}

// checkLimits defaults the data capacity (32 GiB) and the memory refusal
// threshold (60%), and checks them with the storage memory budget.
func checkLimits(config *Config) error {
	if config.ClusterCapacity == 0 {
		config.ClusterCapacity = 32 << 30
	}
	if config.ClusterCapacity < 1 {
		return errors.New("limits.data_bytes must be positive")
	}
	if config.StorageMemory != 0 && (config.StorageMemory < 16<<20 || config.StorageMemory > 64<<30) {
		return errors.New("storage.memory_bytes must be between 16 MiB and 64 GiB")
	}
	if config.CopySlots < 0 {
		return errors.New("limits.copy_slots must not be negative")
	}
	if config.MemoryRefuse == 0 {
		config.MemoryRefuse = 60
	}
	if config.MemoryRefuse < 1 || config.MemoryRefuse > 95 {
		return errors.New("limits.memory_refuse_pct must be between 1 and 95")
	}
	return nil
}

// checkIssuer requires a source for the network key: an issuer to fetch it
// from, or the key itself.
func checkIssuer(config Config) error {
	if config.IssuerURL == "" && config.NetworkPubKey == "" {
		return errors.New("identity.issuer or identity.public_key is required; authentication cannot be disabled")
	}
	if config.IssuerURL == "" {
		return nil
	}
	_, err := endpoint(config.IssuerURL)
	return err
}

// chooseNodeName takes the first name available, in this order: the name of
// the certificate already on disk, the configured name, a fresh name near
// identity.prefer_near, a name hinted by a bootstrap, a random name.
func chooseNodeName(config Config, file FileConfig) (string, error) {
	name := stickyNodeName(config)
	if name == "" && file.Identity.PreferNear != "" {
		target, err := indexus.BASE64.Decode(file.Identity.PreferNear)
		if err != nil {
			return "", fmt.Errorf("identity.prefer_near: %w", err)
		}
		if name, err = indexus.BASE64.RandomNameNear(target, preferNearBits); err != nil {
			return "", err
		}
	}
	if name == "" && len(file.Network.Bootstrap) > 0 {
		// JOIN-1: halve one of the network's largest cells instead of landing
		// on a random point. A bootstrap that cannot answer leaves it random.
		name = joinHint(file.Network.Bootstrap)
	}
	if name == "" {
		var err error
		if name, err = indexus.BASE64.RandomName(); err != nil {
			return "", err
		}
	}
	if err := indexus.CheckName(name); err != nil {
		return "", errors.New("invalid node identity")
	}
	return name, nil
}

// addBootstraps turns each bootstrap origin into a contact with a random
// name, on the origin's port or its scheme's default, and keeps the origins
// as the topology endpoints.
func addBootstraps(config *Config, bootstraps []string) error {
	var origins []string
	for _, origin := range bootstraps {
		u, err := endpoint(origin)
		if err != nil {
			return err
		}
		port := 80
		if u.Scheme == "https" {
			port = 443
		}
		if u.Port() != "" {
			port, _ = strconv.Atoi(u.Port())
		}
		name, err := indexus.BASE64.RandomName()
		if err != nil {
			return err
		}
		contact := peer.NewContact(name, map[string]any{u.Hostname(): nil}, port).(*peer.Contact)
		contact.SetScheme(u.Scheme)
		config.Bootstraps = append(config.Bootstraps, contact)
		origins = append(origins, strings.TrimRight(origin, "/"))
	}
	config.TopologyEndpoints = strings.Join(origins, ",")
	return nil
}

// configureAutoscale enables the autoscaler when a provisioner is set. The
// write limit defaults to 70% of the vnode's write cap, and any write limit
// is smoothed over 10 seconds.
func configureAutoscale(config *Config, file FileConfig) error {
	if file.Scaling.CPUTargetPct < 0 || file.Scaling.CPUTargetPct > 100 {
		return fmt.Errorf("scaling.cpu_target_pct must be between 0 and 100")
	}
	if file.Provisioner == "" {
		return nil
	}
	if _, err := endpoint(file.Provisioner); err != nil {
		return err
	}
	config.Autoscale = autoscale.Config{Enabled: true, ProvisionerURL: strings.TrimRight(file.Provisioner, "/"), StoragePath: filepath.Dir(config.Storage), MemRefusePct: config.MemoryRefuse,
		CPULimitPct: file.Scaling.CPUTargetPct, ItemsLimit: file.Scaling.ItemsPerVNode, ItemsOnly: file.Scaling.ResourceSignals != nil && !*file.Scaling.ResourceSignals,
		WritesLimit: file.Scaling.WritesPerVNode}
	if config.Autoscale.WritesLimit == 0 && file.Limits.WritesPerSecond > 0 {
		config.Autoscale.WritesLimit = 0.7 * file.Limits.WritesPerSecond
	}
	if config.Autoscale.WritesLimit > 0 {
		config.Autoscale.WritesHold = 10 * time.Second
	}
	return nil
}

// joinHint asks each bootstrap in turn for a JOIN-1 name, briefly.
func joinHint(bootstraps []string) string {
	client := &http.Client{Timeout: 3 * time.Second}
	for _, origin := range bootstraps {
		res, err := client.Get(strings.TrimRight(origin, "/") + "/join-hint")
		if err != nil {
			continue
		}
		var body struct{ Name string }
		err = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if err == nil && res.StatusCode == http.StatusOK && len(body.Name) == indexus.BASE64.IDLength() {
			if _, err := indexus.BASE64.Decode(body.Name); err == nil {
				return body.Name
			}
		}
	}
	return ""
}

// preferNearBits is how much of a prefer_near target a spawned name keeps:
// enough to land inside any cell the autoscaler aims at, short of the whole id.
const preferNearBits = 64
